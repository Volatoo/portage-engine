// Package capacity executes PostgreSQL-fenced capacity actions against cloud
// providers. Scheduler reconciliation only writes desired state; this package
// is the sole owner of slow infrastructure side effects.
package capacity

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/builder"
)

// ProvisionResult contains only non-secret provider state required to resume
// or delete an instance. Providers must never return bootstrap credentials in
// Attributes because this value is persisted in PostgreSQL.
type ProvisionResult struct {
	RemoteStateRef string
	Attributes     map[string]string
}

// Provider mutates exactly the instance identity assigned by PostgreSQL.
// Provision and Delete must be idempotent: the actuator may replay either call
// after losing its process between the provider effect and ledger commit.
type Provider interface {
	Provision(
		context.Context,
		*builder.CapacityActionClaim,
		*builder.CapacityInstance,
	) (ProvisionResult, error)
	Delete(context.Context, *builder.CapacityInstance) error
}

// ProviderSet rejects unknown provider names rather than falling back to a
// default that could affect an unintended infrastructure account.
type ProviderSet map[string]Provider

func (s ProviderSet) resolve(name string) (Provider, error) {
	provider := s[strings.ToLower(strings.TrimSpace(name))]
	if provider == nil {
		return nil, fmt.Errorf("capacity provider %q is not configured", name)
	}
	return provider, nil
}

type Config struct {
	Owner          string
	Lease          time.Duration
	PollInterval   time.Duration
	OperationLimit time.Duration
	IdleInterval   time.Duration
}

func (c Config) validate() error {
	if strings.TrimSpace(c.Owner) == "" || len(c.Owner) > 256 {
		return fmt.Errorf("capacity actuator owner is required")
	}
	if c.Lease < 3*time.Second {
		return fmt.Errorf("capacity actuator lease must be at least 3 seconds")
	}
	if c.PollInterval <= 0 || c.PollInterval >= c.Lease/2 {
		return fmt.Errorf("capacity actuator poll interval must be below half the lease")
	}
	if c.OperationLimit <= 0 || c.IdleInterval <= 0 {
		return fmt.Errorf("capacity actuator operation and idle intervals must be positive")
	}
	return nil
}

type Actuator struct {
	ledger    builder.CapacityActuatorLedger
	providers ProviderSet
	config    Config
}

func New(
	ledger builder.CapacityActuatorLedger,
	providers ProviderSet,
	config Config,
) (*Actuator, error) {
	if ledger == nil {
		return nil, fmt.Errorf("capacity actuator ledger is required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("at least one capacity provider is required")
	}
	for name, provider := range providers {
		if strings.TrimSpace(name) == "" || provider == nil {
			return nil, fmt.Errorf("capacity provider registry contains an invalid entry")
		}
	}
	return &Actuator{ledger: ledger, providers: providers, config: config}, nil
}

// Run processes actions until cancellation. A failed provider operation is
// returned to the durable queue with bounded database-managed backoff.
func (a *Actuator) Run(ctx context.Context) error {
	for {
		worked, err := a.RunOne(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Capacity actuator action failed: %v", err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if worked {
			continue
		}
		timer := time.NewTimer(a.config.IdleInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RunOne claims and executes at most one action.
func (a *Actuator) RunOne(ctx context.Context) (bool, error) {
	claim, err := a.ledger.ClaimCapacityAction(
		ctx, a.config.Owner, a.config.Lease,
	)
	if err != nil || claim == nil {
		return false, err
	}
	provider, err := a.providers.resolve(claim.Pool.Provider)
	if err == nil {
		err = a.executeWithLease(ctx, claim, provider)
	}
	if err == nil {
		return true, nil
	}
	retryErr := a.ledger.RetryCapacityAction(ctx, claim, err.Error())
	if retryErr != nil {
		return true, errors.Join(err, retryErr)
	}
	return true, err
}

func (a *Actuator) executeWithLease(
	parent context.Context,
	claim *builder.CapacityActionClaim,
	provider Provider,
) error {
	ctx, cancel := context.WithTimeout(parent, a.config.OperationLimit)
	defer cancel()
	renewErrors := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(a.config.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := a.ledger.RenewCapacityAction(
					ctx, claim, a.config.Lease,
				); err != nil {
					select {
					case renewErrors <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	err := a.execute(ctx, claim, provider)
	close(done)
	select {
	case renewErr := <-renewErrors:
		return errors.Join(err, fmt.Errorf("renew capacity action lease: %w", renewErr))
	default:
		return err
	}
}

func (a *Actuator) execute(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
	provider Provider,
) error {
	switch claim.Kind {
	case "scale-up":
		return a.scaleUp(ctx, claim, provider)
	case "scale-down":
		return a.scaleDown(ctx, claim, provider)
	default:
		return fmt.Errorf("unsupported capacity action kind %q", claim.Kind)
	}
}

func (a *Actuator) scaleUp(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
	provider Provider,
) error {
	instance, err := a.ledger.ReserveCapacityInstance(ctx, claim)
	if err != nil {
		return err
	}
	if instance.RemoteStateRef == "" {
		result, err := provider.Provision(ctx, claim, instance)
		if err != nil {
			return fmt.Errorf("provision %s: %w", instance.ProviderInstanceID, err)
		}
		if err := a.ledger.RecordCapacityInstanceProvisioned(
			ctx, claim, instance, result.RemoteStateRef, result.Attributes,
		); err != nil {
			return err
		}
	}
	return a.wait(ctx, func() (bool, error) {
		return a.ledger.ConfirmCapacityInstanceHeartbeat(ctx, claim, instance)
	}, "executor heartbeat")
}

func (a *Actuator) scaleDown(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
	provider Provider,
) error {
	instance, err := a.ledger.SelectCapacityInstanceForDrain(ctx, claim)
	if err != nil {
		return err
	}
	if instance == nil {
		return fmt.Errorf("no actuator-owned active instance exists in pool %s", claim.Pool.ID)
	}
	if instance.State == "draining" {
		if err := a.wait(ctx, func() (bool, error) {
			return a.ledger.CapacityInstanceDrained(ctx, claim, instance)
		}, "executor drain"); err != nil {
			return err
		}
		if err := a.ledger.BeginCapacityInstanceDelete(
			ctx, claim, instance,
		); err != nil {
			return err
		}
	}
	if err := provider.Delete(ctx, instance); err != nil {
		return fmt.Errorf("delete %s: %w", instance.ProviderInstanceID, err)
	}
	return a.ledger.CompleteCapacityInstanceDelete(ctx, claim, instance)
}

func (a *Actuator) wait(
	ctx context.Context,
	observe func() (bool, error),
	description string,
) error {
	for {
		ready, err := observe()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		timer := time.NewTimer(a.config.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for %s: %w", description, ctx.Err())
		case <-timer.C:
		}
	}
}
