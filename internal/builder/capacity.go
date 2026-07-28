package builder

import (
	"context"
	"time"
)

// CapacityActionClaim is a PostgreSQL-leased instruction. Provider calls are
// allowed only while Owner/Fence/LeaseExpiresAt remain current.
type CapacityActionClaim struct {
	ID             string                          `json:"id"`
	Pool           SchedulerCapacityPoolDefinition `json:"pool"`
	Kind           string                          `json:"kind"`
	RequestedSlots int                             `json:"requested_slots"`
	ObservedSlots  int                             `json:"observed_slots"`
	DeltaSlots     int                             `json:"delta_slots"`
	Reason         string                          `json:"reason"`
	Owner          string                          `json:"owner"`
	Fence          int64                           `json:"fence"`
	LeaseExpiresAt time.Time                       `json:"lease_expires_at"`
}

// CapacityInstance is infrastructure created only by a fenced capacity action.
// OwnerToken and provider identity must both match before lifecycle mutation.
type CapacityInstance struct {
	ID                 string            `json:"id"`
	PoolID             string            `json:"pool_id"`
	CreateActionID     string            `json:"create_action_id"`
	DeleteActionID     string            `json:"delete_action_id,omitempty"`
	Provider           string            `json:"provider"`
	ProviderInstanceID string            `json:"provider_instance_id"`
	OwnerToken         string            `json:"owner_token"`
	Generation         int64             `json:"generation"`
	State              string            `json:"state"`
	RemoteStateRef     string            `json:"remote_state_ref,omitempty"`
	Attributes         map[string]string `json:"attributes,omitempty"`
}

// CapacityActuatorLedger is deliberately independent of DurableScheduler. It
// keeps slow provider side effects outside queue/fairness transactions.
type CapacityActuatorLedger interface {
	RequestCapacityAction(
		context.Context, string, string, int, int, string,
	) (*CapacityActionClaim, error)
	ClaimCapacityAction(
		context.Context, string, time.Duration,
	) (*CapacityActionClaim, error)
	RenewCapacityAction(
		context.Context, *CapacityActionClaim, time.Duration,
	) error
	ReserveCapacityInstance(
		context.Context, *CapacityActionClaim,
	) (*CapacityInstance, error)
	RecordCapacityInstanceProvisioned(
		context.Context, *CapacityActionClaim, *CapacityInstance,
		string, map[string]string,
	) error
	ConfirmCapacityInstanceHeartbeat(
		context.Context, *CapacityActionClaim, *CapacityInstance,
	) (bool, error)
	SelectCapacityInstanceForDrain(
		context.Context, *CapacityActionClaim,
	) (*CapacityInstance, error)
	CapacityInstanceDrained(
		context.Context, *CapacityActionClaim, *CapacityInstance,
	) (bool, error)
	BeginCapacityInstanceDelete(
		context.Context, *CapacityActionClaim, *CapacityInstance,
	) error
	CompleteCapacityInstanceDelete(
		context.Context, *CapacityActionClaim, *CapacityInstance,
	) error
	RetryCapacityAction(
		context.Context, *CapacityActionClaim, string,
	) error
}
