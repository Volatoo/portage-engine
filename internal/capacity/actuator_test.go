package capacity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/builder"
)

type fakeLedger struct {
	claim          *builder.CapacityActionClaim
	instance       *builder.CapacityInstance
	heartbeatAfter int
	drainedAfter   int
	heartbeatCalls int
	drainedCalls   int
	provisioned    bool
	beganDelete    bool
	completed      bool
	retried        string
}

func (f *fakeLedger) RequestCapacityAction(
	context.Context, string, string, int, int, string,
) (*builder.CapacityActionClaim, error) {
	return nil, errors.New("unused")
}
func (f *fakeLedger) ClaimCapacityAction(
	context.Context, string, time.Duration,
) (*builder.CapacityActionClaim, error) {
	claim := f.claim
	f.claim = nil
	return claim, nil
}
func (f *fakeLedger) RenewCapacityAction(
	context.Context, *builder.CapacityActionClaim, time.Duration,
) error {
	return nil
}
func (f *fakeLedger) ReserveCapacityInstance(
	context.Context, *builder.CapacityActionClaim,
) (*builder.CapacityInstance, error) {
	return f.instance, nil
}
func (f *fakeLedger) RecordCapacityInstanceProvisioned(
	_ context.Context,
	_ *builder.CapacityActionClaim,
	_ *builder.CapacityInstance,
	remoteStateRef string,
	_ map[string]string,
) error {
	f.provisioned = true
	f.instance.RemoteStateRef = remoteStateRef
	return nil
}
func (f *fakeLedger) ConfirmCapacityInstanceHeartbeat(
	context.Context,
	*builder.CapacityActionClaim,
	*builder.CapacityInstance,
) (bool, error) {
	f.heartbeatCalls++
	if f.heartbeatCalls >= f.heartbeatAfter {
		f.completed = true
		return true, nil
	}
	return false, nil
}
func (f *fakeLedger) SelectCapacityInstanceForDrain(
	context.Context, *builder.CapacityActionClaim,
) (*builder.CapacityInstance, error) {
	return f.instance, nil
}
func (f *fakeLedger) CapacityInstanceDrained(
	context.Context,
	*builder.CapacityActionClaim,
	*builder.CapacityInstance,
) (bool, error) {
	f.drainedCalls++
	return f.drainedCalls >= f.drainedAfter, nil
}
func (f *fakeLedger) BeginCapacityInstanceDelete(
	context.Context,
	*builder.CapacityActionClaim,
	*builder.CapacityInstance,
) error {
	f.beganDelete = true
	f.instance.State = "deleting"
	return nil
}
func (f *fakeLedger) CompleteCapacityInstanceDelete(
	context.Context,
	*builder.CapacityActionClaim,
	*builder.CapacityInstance,
) error {
	f.completed = true
	return nil
}
func (f *fakeLedger) RetryCapacityAction(
	_ context.Context,
	_ *builder.CapacityActionClaim,
	detail string,
) error {
	f.retried = detail
	return nil
}

type fakeProvider struct {
	provisionCalls int
	deleteCalls    int
	provisionErr   error
}

func (f *fakeProvider) Provision(
	context.Context,
	*builder.CapacityActionClaim,
	*builder.CapacityInstance,
) (ProvisionResult, error) {
	f.provisionCalls++
	return ProvisionResult{
		RemoteStateRef: "state/pve/example",
		Attributes:     map[string]string{"vmid": "1099"},
	}, f.provisionErr
}
func (f *fakeProvider) Delete(
	context.Context,
	*builder.CapacityInstance,
) error {
	f.deleteCalls++
	return nil
}

func testConfig() Config {
	return Config{
		Owner:          "actuator-a",
		Lease:          3 * time.Second,
		PollInterval:   time.Millisecond,
		OperationLimit: 100 * time.Millisecond,
		IdleInterval:   time.Millisecond,
	}
}

func testClaim(kind string) *builder.CapacityActionClaim {
	return &builder.CapacityActionClaim{
		ID: "action-1",
		Pool: builder.SchedulerCapacityPoolDefinition{
			ID:       "capacity-pool:test",
			Provider: "pve",
		},
		Kind:       kind,
		Owner:      "actuator-a",
		Fence:      1,
		DeltaSlots: 1,
	}
}

func testInstance(state string) *builder.CapacityInstance {
	return &builder.CapacityInstance{
		ID:                 "instance-1",
		PoolID:             "capacity-pool:test",
		Provider:           "pve",
		ProviderInstanceID: "portage-capacity-instance-1",
		OwnerToken:         "owner-token",
		Generation:         1,
		State:              state,
	}
}

func TestActuatorScaleUpWaitsForHeartbeat(t *testing.T) {
	ledger := &fakeLedger{
		claim: testClaim("scale-up"), instance: testInstance("provisioning"),
		heartbeatAfter: 2,
	}
	provider := &fakeProvider{}
	actuator, err := New(
		ledger, ProviderSet{"pve": provider}, testConfig(),
	)
	if err != nil {
		t.Fatal(err)
	}
	worked, err := actuator.RunOne(context.Background())
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if !worked || provider.provisionCalls != 1 || !ledger.provisioned ||
		ledger.heartbeatCalls != 2 || !ledger.completed {
		t.Fatalf("scale-up lifecycle incomplete: %#v %#v", ledger, provider)
	}
}

func TestActuatorRetriesProviderFailure(t *testing.T) {
	ledger := &fakeLedger{
		claim: testClaim("scale-up"), instance: testInstance("provisioning"),
		heartbeatAfter: 1,
	}
	provider := &fakeProvider{provisionErr: errors.New("provider unavailable")}
	actuator, err := New(
		ledger, ProviderSet{"pve": provider}, testConfig(),
	)
	if err != nil {
		t.Fatal(err)
	}
	worked, err := actuator.RunOne(context.Background())
	if !worked || err == nil || ledger.retried == "" || ledger.completed {
		t.Fatalf("provider failure was not durably retried: worked=%v err=%v ledger=%#v", worked, err, ledger)
	}
}

func TestActuatorScaleDownDrainsBeforeExactDelete(t *testing.T) {
	ledger := &fakeLedger{
		claim: testClaim("scale-down"), instance: testInstance("draining"),
		drainedAfter: 2,
	}
	provider := &fakeProvider{}
	actuator, err := New(
		ledger, ProviderSet{"pve": provider}, testConfig(),
	)
	if err != nil {
		t.Fatal(err)
	}
	worked, err := actuator.RunOne(context.Background())
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if !worked || ledger.drainedCalls != 2 || !ledger.beganDelete ||
		provider.deleteCalls != 1 || !ledger.completed {
		t.Fatalf("scale-down lifecycle incomplete: %#v %#v", ledger, provider)
	}
}

func TestActuatorRejectsUnknownProviderWithoutSideEffect(t *testing.T) {
	claim := testClaim("scale-up")
	claim.Pool.Provider = "unknown"
	ledger := &fakeLedger{
		claim: claim, instance: testInstance("provisioning"),
		heartbeatAfter: 1,
	}
	provider := &fakeProvider{}
	actuator, err := New(
		ledger, ProviderSet{"pve": provider}, testConfig(),
	)
	if err != nil {
		t.Fatal(err)
	}
	worked, err := actuator.RunOne(context.Background())
	if !worked || err == nil || provider.provisionCalls != 0 ||
		ledger.retried == "" {
		t.Fatalf("unknown provider handling mismatch: worked=%v err=%v ledger=%#v provider=%#v", worked, err, ledger, provider)
	}
}
