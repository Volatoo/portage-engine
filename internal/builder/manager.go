// Package builder manages package build requests and infrastructure.
package builder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"mime"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/slchris/portage-engine/internal/binpkg"
	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/distcc"
	"github.com/slchris/portage-engine/internal/iac"
	"github.com/slchris/portage-engine/internal/metrics"
	"github.com/slchris/portage-engine/internal/signing"
	artifactstorage "github.com/slchris/portage-engine/internal/storage"
	"github.com/slchris/portage-engine/internal/workergateway"
	"github.com/slchris/portage-engine/pkg/config"
)

// BuildRequest represents a package build request.
type BuildRequest struct {
	ProjectID     string            `json:"-"`
	RequestedBy   string            `json:"-"`
	PackageName   string            `json:"package_name"`
	Version       string            `json:"version"`
	Arch          string            `json:"arch"`
	UseFlags      []string          `json:"use_flags"`
	CloudProvider string            `json:"cloud_provider"`
	ProfileID     string            `json:"profile_id,omitempty"`
	RepositoryIDs []string          `json:"repository_ids,omitempty"`
	ResourceClass string            `json:"resource_class,omitempty"`
	MachineSpec   map[string]string `json:"machine_spec"`
	// ResolvedContext is produced by the server catalog. It is never decoded
	// from a public request and is the authoritative infrastructure selection.
	ResolvedContext *catalog.ResolvedBuildContext `json:"-"`
	// ConfigBundle, when set, carries the full Portage configuration to build
	// with. It is forwarded verbatim to the remote builder so the build applies
	// the exact USE flags / make.conf / repos the client specified.
	ConfigBundle *ConfigBundle `json:"config_bundle,omitempty"`
	// CompileLease is server-owned and never accepted from a public request.
	CompileLease *distcc.Lease `json:"-"`
	// IdempotencyKey is supplied by the control-plane HTTP boundary. It is not
	// forwarded to builders; PostgreSQL uses it to deduplicate submissions.
	IdempotencyKey string `json:"-"`
}

// BuildResponse represents a build request response.
type BuildResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// RequestError identifies a request rejected before any job or infrastructure
// is allocated. HTTP handlers map it to 400 instead of reporting an operator
// or builder availability failure.
type RequestError struct {
	err error
}

func (e *RequestError) Error() string { return e.err.Error() }
func (e *RequestError) Unwrap() error { return e.err }

// IsRequestError reports whether err is a client-side build contract error.
func IsRequestError(err error) bool {
	var requestErr *RequestError
	return errors.As(err, &requestErr)
}

// IdempotencyConflictError means an idempotency key was reused with a
// different request. HTTP adapters expose it as a conflict.
type IdempotencyConflictError struct {
	err error
}

func (e *IdempotencyConflictError) Error() string { return e.err.Error() }
func (e *IdempotencyConflictError) Unwrap() error { return e.err }

// NewIdempotencyConflictError wraps a conflicting idempotency-key reuse.
func NewIdempotencyConflictError(err error) error {
	return &IdempotencyConflictError{err: err}
}

// IsIdempotencyConflict reports whether err is an idempotency-key conflict.
func IsIdempotencyConflict(err error) bool {
	var conflict *IdempotencyConflictError
	return errors.As(err, &conflict)
}

// AdmissionError is a durable project-policy rejection. Code is stable for
// API/CLI automation; the human message may become more descriptive.
type AdmissionError struct {
	Code       string
	Limit      int
	Used       int
	RetryAfter time.Duration
	err        error
}

func (e *AdmissionError) Error() string { return e.err.Error() }
func (e *AdmissionError) Unwrap() error { return e.err }

// NewAdmissionError constructs a policy rejection without exposing the
// persistence package through the public HTTP boundary.
func NewAdmissionError(code string, limit, used int, retryAfter time.Duration, err error) error {
	return &AdmissionError{
		Code: code, Limit: limit, Used: used, RetryAfter: retryAfter, err: err,
	}
}

// AsAdmissionError returns the stable admission detail from a wrapped error.
func AsAdmissionError(err error) (*AdmissionError, bool) {
	var admission *AdmissionError
	return admission, errors.As(err, &admission)
}

// PhaseCapacityError means a durable attempt must wait at a pipeline
// checkpoint. The attempt lease remains renewable while PostgreSQL capacity is
// unavailable; callers must not treat this as a build failure.
type PhaseCapacityError struct {
	Phase string
	Limit int
	Used  int
}

func (e *PhaseCapacityError) Error() string {
	return fmt.Sprintf(
		"project phase capacity unavailable: phase=%s used=%d limit=%d",
		e.Phase, e.Used, e.Limit,
	)
}

// AsPhaseCapacityError extracts a durable checkpoint-cap rejection.
func AsPhaseCapacityError(err error) (*PhaseCapacityError, bool) {
	var capacity *PhaseCapacityError
	return capacity, errors.As(err, &capacity)
}

// LedgerError means the durable job ledger rejected or could not persist a
// mutation. The in-memory queue must not accept that mutation.
type LedgerError struct {
	err error
}

func (e *LedgerError) Error() string { return e.err.Error() }
func (e *LedgerError) Unwrap() error { return e.err }

// IsLedgerError reports whether err originated from the durable ledger gate.
func IsLedgerError(err error) bool {
	var ledgerErr *LedgerError
	return errors.As(err, &ledgerErr)
}

// BuildStatus represents the status of a build job.
type BuildStatus struct {
	JobID        string    `json:"job_id"`
	ProjectID    string    `json:"project_id,omitempty"`
	RequestedBy  string    `json:"requested_by,omitempty"`
	Status       string    `json:"status"`
	PackageName  string    `json:"package_name"`
	Version      string    `json:"version"`
	Arch         string    `json:"arch"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	InstanceID   string    `json:"instance_id,omitempty"`
	Error        string    `json:"error,omitempty"`
	ArtifactPath string    `json:"artifact_path,omitempty"`
	// ArtifactURL is the web path of the stored artifact on the binhost
	// (e.g. /binpkgs/app-misc/jq-1.7-1.gpkg.tar).
	ArtifactURL string `json:"artifact_url,omitempty"`
	// Artifacts lists the web paths of every binpkg stored for this job
	// (dependencies included); ArtifactURL stays the requested package's own.
	Artifacts []string `json:"artifacts,omitempty"`
	// ArtifactPaths mirrors Artifacts with local filesystem paths (cleanup).
	ArtifactPaths []string `json:"-"`
	// Staging fields are server-internal quarantine state. They are never
	// exposed as downloadable artifacts before verification and promotion.
	StagingRoot       string   `json:"-"`
	VerificationToken string   `json:"-"`
	StagedArtifacts   []string `json:"-"`
	StagedPrimary     string   `json:"-"`
	// Signed reports whether the builder signed the produced packages.
	Signed bool `json:"signed,omitempty"`
	// SigningKeyID is the key the isolated signer actually reported for this
	// generation. Publication happens minutes to hours later, possibly on
	// another replica and after a signer key rotation, so the immutable
	// generation manifest must stamp this value rather than re-read whatever
	// public key file the control plane currently holds.
	SigningKeyID string `json:"signing_key_id,omitempty"`
	// ResolvedContext records the exact server-owned profile, repositories,
	// mirror bundle, and image generation selected for this job.
	ResolvedContext *catalog.ResolvedBuildContext `json:"resolved_context,omitempty"`
	// FailedStage names the pipeline stage a failed job died in
	// (provision/deploy/build/collect/verify), for accurate UI attribution.
	FailedStage string `json:"failed_stage,omitempty"`
	Log         string `json:"log,omitempty"`
	// Request is the immutable, resolved request used by the DB-1 shadow
	// ledger. It is hidden from APIs and the legacy JSON snapshot.
	Request *BuildRequest `json:"-"`
	// DB-2 claim identity is hidden from public APIs. Every transition and the
	// final publish gate must present this attempt/fence tuple.
	AttemptID  string `json:"-"`
	FenceToken int64  `json:"-"`
	LeaseOwner string `json:"-"`
}

// PhaseWorkClaim is one independently leased pipeline stage. ClaimFence is
// separate from AttemptFence so a timed-out executor cannot commit after a
// different replica has reclaimed the same stage.
type PhaseWorkClaim struct {
	ID             string        `json:"id"`
	ProjectID      string        `json:"project_id"`
	JobID          string        `json:"job_id"`
	AttemptID      string        `json:"attempt_id"`
	AttemptFence   int64         `json:"attempt_fence"`
	Phase          string        `json:"phase"`
	Sequence       int           `json:"sequence"`
	ClaimOwner     string        `json:"claim_owner"`
	ClaimFence     int64         `json:"claim_fence"`
	LeaseExpiresAt time.Time     `json:"lease_expires_at"`
	Request        *BuildRequest `json:"-"`
	Status         *BuildStatus  `json:"-"`
}

// PhaseInstanceContext is the non-secret subset required to hand a disposable
// VM from provision to build/verify/publish, including its shared Terraform
// workspace for terminal cleanup.
type PhaseInstanceContext struct {
	ID              string            `json:"id"`
	Provider        string            `json:"provider"`
	Status          string            `json:"status"`
	IPAddress       string            `json:"ip_address"`
	PublicIP        string            `json:"public_ip,omitempty"`
	PrivateIP       string            `json:"private_ip,omitempty"`
	Arch            string            `json:"arch"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	TerraformDir    string            `json:"terraform_dir"`
	SSHUser         string            `json:"ssh_user,omitempty"`
	BuilderEndpoint string            `json:"builder_endpoint"`
	CreatedAt       time.Time         `json:"created_at"`
	TTL             time.Duration     `json:"ttl"`
}

// PhaseExecutionContext is the durable hand-off document. In object-storage
// mode StagingRoot is deliberately empty: token plus immutable relative paths
// are the hand-off authority and each replica creates its own scratch path.
// Certificate private keys and cloud credentials are deliberately absent.
type PhaseExecutionContext struct {
	Instance          *PhaseInstanceContext `json:"instance,omitempty"`
	WorkerID          string                `json:"worker_id,omitempty"`
	StagingRoot       string                `json:"staging_root,omitempty"`
	VerificationToken string                `json:"verification_token,omitempty"`
	StagedArtifacts   []string              `json:"staged_artifacts,omitempty"`
	StagedPrimary     string                `json:"staged_primary,omitempty"`
	Signed            bool                  `json:"signed,omitempty"`
	SigningKeyID      string                `json:"signing_key_id,omitempty"`
	ArtifactPath      string                `json:"artifact_path,omitempty"`
	ArtifactURL       string                `json:"artifact_url,omitempty"`
	ArtifactPaths     []string              `json:"artifact_paths,omitempty"`
	Artifacts         []string              `json:"artifacts,omitempty"`
}

// LedgerCreateResult describes either a newly inserted shadow-ledger row or
// the existing row returned for an idempotent retry.
type LedgerCreateResult struct {
	JobID   string
	Status  *BuildStatus
	Created bool
}

// JobLedger is the DB-1 shadow persistence contract. The in-memory queue
// remains authoritative until DB-2, but a new job may not start unless its
// initial row and claim transition are durable.
type JobLedger interface {
	CreateJob(context.Context, *BuildRequest, *BuildStatus) (LedgerCreateResult, error)
	RecordTransition(context.Context, *BuildStatus, *BuildStatus) error
	HideJob(context.Context, *BuildStatus, string) error
}

// SchedulerClaim is an atomically claimed PostgreSQL queue item.
type SchedulerClaim struct {
	Request *BuildRequest
	Status  *BuildStatus
}

// SchedulerRuntimeStatus is a low-cardinality database view of queue and
// executor health. It deliberately contains no package names, job IDs, or
// request payloads so it is safe to expose on the operator monitor.
type SchedulerRuntimeStatus struct {
	Authority          string                   `json:"authority"`
	QueuedTasks        int                      `json:"queued_tasks"`
	UnschedulableTasks int                      `json:"unschedulable_tasks"`
	RunningTasks       int                      `json:"running_tasks"`
	ActiveLeases       int                      `json:"active_leases"`
	ExpiredLeases      int                      `json:"expired_leases"`
	RegisteredWorkers  int                      `json:"registered_workers"`
	ActiveWorkers      int                      `json:"active_workers"`
	CapabilityWorkers  int                      `json:"capability_workers"`
	StaleWorkers       int                      `json:"stale_workers"`
	AttemptsLastHour   int                      `json:"attempts_last_hour"`
	LeaseExpiries      LeaseExpiryStatus        `json:"lease_expiries"`
	OldestQueuedAt     *time.Time               `json:"oldest_queued_at,omitempty"`
	OldestLeaseExpires *time.Time               `json:"oldest_lease_expires_at,omitempty"`
	Fairness           SchedulerFairnessStatus  `json:"fairness"`
	WorkerScoring      WorkerScoringStatus      `json:"worker_scoring"`
	TargetHistory      TargetHistoryStatus      `json:"target_history"`
	Autoscaler         SchedulerAutoscaleStatus `json:"autoscaler"`
}

// LeaseExpiryStatus is a fixed-cardinality view of durable scheduler recovery
// outcomes. Adding an identity-bearing field here would also add an unsafe
// monitoring dimension, so details remain in job and audit events.
type LeaseExpiryStatus struct {
	AttemptRequeued   int64 `json:"attempt_requeued"`
	AttemptFailed     int64 `json:"attempt_failed"`
	AttemptCanceled   int64 `json:"attempt_canceled"`
	AdmissionRequeued int64 `json:"admission_requeued"`
	AdmissionFailed   int64 `json:"admission_failed"`
	AdmissionCanceled int64 `json:"admission_canceled"`
	PhaseReclaimed    int64 `json:"phase_reclaimed"`
}

type TargetHistoryStatus struct {
	GeneratedAt      time.Time                 `json:"generated_at"`
	RetentionDays    int                       `json:"retention_days"`
	SLOTargetPercent float64                   `json:"slo_target_percent"`
	MinimumSamples   int                       `json:"minimum_samples"`
	Projection       MonitorProjectionStatus   `json:"projection"`
	Targets          []TargetReliabilityStatus `json:"targets,omitempty"`
}

// MonitorProjectionStatus reports whether the cached Monitor snapshot a
// replica is serving predates the newest terminal event. Both watermarks
// originate in PostgreSQL, but their distance is not the measure: the
// projection is a view over the same base tables, so a gap only says which
// event the snapshot was built before, and its size is the quiet period ahead
// of that event. LagSeconds is therefore the age of the snapshot being served
// — zero on a fresh load, when caught up, and when the source is empty — and
// AlertThresholdSeconds is the read-through cache TTL that bounds it, so a
// reading above it means the refresh contract broke rather than that an
// operator-tunable alerting threshold was crossed.
type MonitorProjectionStatus struct {
	Valid                  bool       `json:"valid"`
	State                  string     `json:"state"`
	ObservedAt             time.Time  `json:"observed_at"`
	SourceWatermarkPresent bool       `json:"source_watermark_present"`
	SourceWatermarkAt      *time.Time `json:"source_watermark_at,omitempty"`
	ProjectedWatermarkAt   *time.Time `json:"projected_watermark_at,omitempty"`
	LagSeconds             int64      `json:"lag_seconds"`
	AlertThresholdSeconds  int64      `json:"alert_threshold_seconds"`
}

type TargetReliabilityStatus struct {
	TargetID        string                    `json:"target_id"`
	ProjectID       string                    `json:"project_id"`
	ProjectName     string                    `json:"project_name"`
	Provider        string                    `json:"provider"`
	ExecutionZone   string                    `json:"execution_zone"`
	Architecture    string                    `json:"architecture"`
	BuildMode       string                    `json:"build_mode"`
	ProfileID       string                    `json:"profile_id"`
	ImageID         string                    `json:"image_id"`
	ImageGeneration string                    `json:"image_generation"`
	ResourceClass   string                    `json:"resource_class"`
	Windows         []TargetReliabilityWindow `json:"windows"`
}

type TargetReliabilityWindow struct {
	Name                   string  `json:"name"`
	Hours                  int     `json:"hours"`
	Samples                int     `json:"samples"`
	Successes              int     `json:"successes"`
	Failures               int     `json:"failures"`
	Canceled               int     `json:"canceled"`
	SuccessRatePercent     float64 `json:"success_rate_percent"`
	SLOMet                 bool    `json:"slo_met"`
	InsufficientData       bool    `json:"insufficient_data"`
	QueueP50Seconds        int64   `json:"queue_p50_seconds"`
	QueueP95Seconds        int64   `json:"queue_p95_seconds"`
	RunP50Seconds          int64   `json:"run_p50_seconds"`
	RunP95Seconds          int64   `json:"run_p95_seconds"`
	ReservedCostMicrounits int64   `json:"reserved_cost_microunits"`
	ChargedCostMicrounits  int64   `json:"charged_cost_microunits"`
	DominantFailureClass   string  `json:"dominant_failure_class,omitempty"`
}

type WorkerScoringStatus struct {
	DecisionsLastHour      int                    `json:"decisions_last_hour"`
	MultiCandidateLastHour int                    `json:"multi_candidate_last_hour"`
	Recent                 []WorkerDecisionStatus `json:"recent,omitempty"`
}

type WorkerDecisionStatus struct {
	WorkKind       string    `json:"work_kind"`
	Phase          string    `json:"phase,omitempty"`
	Worker         string    `json:"worker"`
	CandidateCount int       `json:"candidate_count"`
	PressureScore  int       `json:"pressure_score"`
	RecentFailures int       `json:"recent_failures"`
	Reason         string    `json:"reason"`
	SelectedAt     time.Time `json:"selected_at"`
}

type SchedulerFairnessStatus struct {
	Enabled             bool  `json:"enabled"`
	EligibleProjects    int   `json:"eligible_projects"`
	StarvedProjects     int   `json:"starved_projects"`
	AdmissionDispatches int64 `json:"admission_dispatches"`
	PhaseDispatches     int64 `json:"phase_dispatches"`
	MaxQueueWaitSeconds int64 `json:"max_queue_wait_seconds"`
}

type SchedulerAutoscalePolicy struct {
	Mode             string
	MinSlots         int
	MaxSlots         int
	TargetReady      int
	Cooldown         time.Duration
	ScaleDownDelay   time.Duration
	Pools            []SchedulerCapacityPoolDefinition
	ProviderMaxSlots map[string]int
}

type SchedulerAutoscaleStatus struct {
	Scope                string                        `json:"scope"`
	Mode                 string                        `json:"mode"`
	ActiveSlots          int                           `json:"active_slots"`
	BusySlots            int                           `json:"busy_slots"`
	Backlog              int                           `json:"backlog"`
	UnschedulableBacklog int                           `json:"unschedulable_backlog"`
	DesiredSlots         int                           `json:"desired_slots"`
	Recommendation       string                        `json:"recommendation"`
	Reason               string                        `json:"reason,omitempty"`
	UnderTargetSince     *time.Time                    `json:"under_target_since,omitempty"`
	LastChangedAt        *time.Time                    `json:"last_changed_at,omitempty"`
	LastEvaluatedAt      *time.Time                    `json:"last_evaluated_at,omitempty"`
	Pools                []SchedulerCapacityPoolStatus `json:"pools,omitempty"`
	Actuator             CapacityActuatorStatus        `json:"actuator"`
}

type CapacityActuatorStatus struct {
	OpenActions           int                      `json:"open_actions"`
	FailedActions         int                      `json:"failed_actions"`
	ProvisioningInstances int                      `json:"provisioning_instances"`
	ActiveInstances       int                      `json:"active_instances"`
	DrainingInstances     int                      `json:"draining_instances"`
	DeletingInstances     int                      `json:"deleting_instances"`
	Actions               []CapacityActionStatus   `json:"actions,omitempty"`
	Instances             []CapacityInstanceStatus `json:"instances,omitempty"`
}

type CapacityActionStatus struct {
	ID             string     `json:"id"`
	PoolID         string     `json:"pool_id"`
	Kind           string     `json:"kind"`
	State          string     `json:"state"`
	RequestedSlots int        `json:"requested_slots"`
	ObservedSlots  int        `json:"observed_slots"`
	Attempts       int        `json:"attempts"`
	FailureDetail  string     `json:"failure_detail,omitempty"`
	RequestedAt    time.Time  `json:"requested_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

type CapacityInstanceStatus struct {
	ID                  string            `json:"id"`
	PoolID              string            `json:"pool_id"`
	Provider            string            `json:"provider"`
	ProviderInstanceID  string            `json:"provider_instance_id"`
	Generation          int64             `json:"generation"`
	State               string            `json:"state"`
	Attributes          map[string]string `json:"attributes,omitempty"`
	HeartbeatObservedAt *time.Time        `json:"heartbeat_observed_at,omitempty"`
	DrainRequestedAt    *time.Time        `json:"drain_requested_at,omitempty"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

type SchedulerCapacityPoolStatus struct {
	SchedulerCapacityPoolDefinition
	ProviderMaxSlots     int        `json:"provider_max_slots"`
	Mode                 string     `json:"mode"`
	ActiveSlots          int        `json:"active_slots"`
	BusySlots            int        `json:"busy_slots"`
	Backlog              int        `json:"backlog"`
	UnschedulableBacklog int        `json:"unschedulable_backlog"`
	DesiredSlots         int        `json:"desired_slots"`
	Recommendation       string     `json:"recommendation"`
	Reason               string     `json:"reason,omitempty"`
	UnderTargetSince     *time.Time `json:"under_target_since,omitempty"`
	LastChangedAt        *time.Time `json:"last_changed_at,omitempty"`
	LastEvaluatedAt      *time.Time `json:"last_evaluated_at,omitempty"`
}

// InfraRecord and ArtifactRecord are attempt-owned DB-3 metadata. They carry
// no provider credentials or file contents.
type InfraRecord struct {
	Provider           string            `json:"provider"`
	ProviderInstanceID string            `json:"provider_instance_id"`
	State              string            `json:"state"`
	RemoteStateRef     string            `json:"remote_state_ref,omitempty"`
	Attributes         map[string]string `json:"attributes,omitempty"`
	FailureDetail      string            `json:"failure_detail,omitempty"`
	CleanupAfter       *time.Time        `json:"cleanup_after,omitempty"`
	DeletedAt          *time.Time        `json:"deleted_at,omitempty"`
}

// ArtifactRecord is the durable metadata projection for one build artifact.
type ArtifactRecord struct {
	Kind        string         `json:"kind"`
	State       string         `json:"state"`
	Digest      string         `json:"digest"`
	SizeBytes   int64          `json:"size_bytes"`
	MediaType   string         `json:"media_type"`
	Location    string         `json:"location"`
	Lineage     map[string]any `json:"lineage,omitempty"`
	Published   *time.Time     `json:"published_at,omitempty"`
	RetainUntil *time.Time     `json:"retention_until,omitempty"`
}

// LogRecord is one timestamped durable build-log entry.
type LogRecord struct {
	OccurredAt time.Time `json:"occurred_at"`
	Message    string    `json:"message"`
}

// RuntimeMetadataLedger persists side effects owned by a fenced attempt.
type RuntimeMetadataLedger interface {
	RecordInfra(context.Context, *BuildStatus, InfraRecord) error
	RecordArtifacts(context.Context, *BuildStatus, []ArtifactRecord) error
}

// ArtifactBudget is the attempt-scoped quarantine byte snapshot. ActiveBytes
// includes every retained generation (for example unsigned and signed).
type ArtifactBudget struct {
	LimitBytes  int64 `json:"limit_bytes"`
	ActiveBytes int64 `json:"active_bytes"`
	PeakBytes   int64 `json:"peak_bytes"`
}

// ArtifactBudgetLedger is the PostgreSQL authority for private quarantine
// bytes. Generation updates are idempotent and serialized per attempt.
type ArtifactBudgetLedger interface {
	ArtifactBudget(context.Context, *BuildStatus) (ArtifactBudget, error)
	SetArtifactGenerationBytes(context.Context, *BuildStatus, string, int64) (ArtifactBudget, error)
	ReleaseArtifactGeneration(context.Context, *BuildStatus, string, string) error
	ReleaseArtifactBudget(context.Context, *BuildStatus, string) error
}

// InfraCleanupClaim is a PostgreSQL-leased Terraform workspace whose original
// build attempt is no longer allowed to own external side effects.
type InfraCleanupClaim struct {
	ID                 string
	Provider           string
	ProviderInstanceID string
	RemoteStateRef     string
	CleanupFence       int64
}

// InfraCleanupLedger turns leaked-instance reclamation into a cross-replica
// queue. PostgreSQL elects one owner/fence; Terraform state stays on the shared
// DATA_DIR volume.
type InfraCleanupLedger interface {
	ClaimInfraCleanup(context.Context, string, time.Duration) (*InfraCleanupClaim, error)
	CompleteInfraCleanup(context.Context, string, string, int64) error
	FailInfraCleanup(context.Context, string, string, int64, string) error
}

// ArtifactPromotionLedger serializes the short external publication saga
// across control-plane replicas while re-validating the attempt fence.
type ArtifactPromotionLedger interface {
	AcquireArtifactPromotion(context.Context, *BuildStatus) (func() error, error)
}

// DurableLogLedger persists and queries build logs across control-plane replicas.
type DurableLogLedger interface {
	AppendLogs(context.Context, *BuildStatus, []LogRecord) error
	LoadLogs(context.Context, string) (string, error)
}

// DurableScheduler is the DB-2 queue authority. Redis/notifications may wake
// workers later, but correctness depends only on these PostgreSQL operations.
type DurableScheduler interface {
	JobLedger
	ClaimNext(context.Context, string, time.Duration, ...[]string) (*SchedulerClaim, error)
	RenewClaim(context.Context, *BuildStatus, time.Duration) error
	CheckClaim(context.Context, *BuildStatus) error
	LoadVisible(context.Context) (map[string]*BuildStatus, error)
	CancelJob(context.Context, string, string) (*BuildStatus, error)
	RetryJob(context.Context, string) (*BuildStatus, error)
	RuntimeStatus(context.Context) (SchedulerRuntimeStatus, error)
}

// DurablePhaseScheduler is the IAM-1B2b2c execution authority. Active mode is
// enabled only when the installed PostgreSQL repository implements this full
// contract, preventing a mixed legacy/phase deployment.
type DurablePhaseScheduler interface {
	ActivatePhasePlan(context.Context, *BuildStatus) error
	ClaimPhaseWork(context.Context, string, time.Duration, []string, ...string) (*PhaseWorkClaim, error)
	RenewPhaseWork(context.Context, *PhaseWorkClaim, time.Duration) error
	CompletePhaseWork(context.Context, *PhaseWorkClaim) error
	SavePhaseExecutionContext(context.Context, *PhaseWorkClaim, *PhaseExecutionContext) error
	LoadPhaseExecutionContext(context.Context, *PhaseWorkClaim) (*PhaseExecutionContext, error)
	FinalizePhaseWork(context.Context, *PhaseWorkClaim, *BuildStatus, *BuildStatus) error
	FailPhaseWorkAndJob(context.Context, *PhaseWorkClaim, *BuildStatus, string) error
}

// DurableAutoscaleObserver writes shared recommendations only. It is
// intentionally separate from DurableScheduler so tests and compatibility
// repositories do not accidentally gain infrastructure side effects.
type DurableAutoscaleObserver interface {
	ReconcileAutoscaling(
		context.Context,
		SchedulerAutoscalePolicy,
	) (SchedulerAutoscaleStatus, error)
}

// CompileScheduler is the independent hard-routing layer consulted only after
// normal project fairness has claimed a job/phase.
type CompileScheduler interface {
	ReserveCompileSlots(context.Context, distcc.ReservationRequest) (*distcc.Lease, error)
	HeartbeatCompileLease(context.Context, distcc.Lease, time.Duration, time.Duration) error
	CheckCompileLease(context.Context, distcc.Lease, time.Duration) error
	RecordCompileObservation(context.Context, distcc.Lease, distcc.Observation) error
	ReleaseCompileLease(context.Context, distcc.Lease, string) error
}

// queuedJob pairs a build request with the job ID assigned at submission, so a
// worker processes exactly the job it dequeued instead of re-deriving one by
// scanning for a matching package (which let concurrent workers double-process
// one job and strand another).
type queuedJob struct {
	jobID string
	req   *BuildRequest
}

// Manager manages build requests and infrastructure provisioning.
type Manager struct {
	config       *config.ServerConfig
	iacMgr       *iac.Manager
	jobs         map[string]*BuildStatus
	jobsMu       sync.RWMutex
	workQueue    chan *queuedJob
	remoteBuilds map[string]string // jobID -> builderURL
	rrNext       atomic.Uint32     // round-robin cursor over RemoteBuilders
	submitMu     sync.Mutex
	jobLedger    JobLedger
	scheduler    DurableScheduler
	compileQueue CompileScheduler
	phaseQueue   DurablePhaseScheduler
	metadata     RuntimeMetadataLedger
	artifactDB   ArtifactBudgetLedger
	infraCleanup InfraCleanupLedger
	promotionDB  ArtifactPromotionLedger
	signing      signing.Coordinator
	logLedger    DurableLogLedger
	stopCh       chan struct{}
	shutdownOnce sync.Once
	// workersMu guards workersStarted. A sync.Once is deliberately not used:
	// a start that fails must stay retryable, because the executor pool is the
	// only thing that ever polls for accepted builds.
	workersMu      sync.Mutex
	workersStarted bool
	cleanupOnce    sync.Once
	stopped        bool // guarded by submitMu
	schedulerID    string
	executorCaps   []string
	wakeHook       func()
	eventHook      func(BuildStatus)

	// onArtifactStored, when set, is called after an artifact lands in the
	// binhost PKGDIR (the server uses it to refresh the Packages index).
	onArtifactStored func()
	// promoteArtifacts is the only path from quarantine into the public
	// PKGDIR. The server wires it to binpkg.Store.PromoteStaged.
	promoteArtifacts func(string, []string, string, string) ([]string, error)

	// gpgKeyProvider supplies only the isolated signer's public identity. The
	// third return is retained for API compatibility and must always be nil.
	gpgKeyProvider func() (string, []byte, []byte)

	// cloudSettings is the runtime-adjustable cloud provisioning config,
	// swapped atomically by the settings API. Workers take a snapshot per
	// build, so an update never races an in-flight provision.
	cloudSettings atomic.Pointer[config.CloudSettings]
	buildCatalog  atomic.Pointer[catalog.Catalog]
	workerBroker  *workergateway.Broker
	workerIssuer  workergateway.Issuer
	artifactStore artifactstorage.Storage
}

// SetArtifactStoredHook registers a callback invoked after an artifact has
// been stored into the binhost PKGDIR.
func (m *Manager) SetArtifactStoredHook(f func()) {
	m.onArtifactStored = f
}

// SetArtifactPromotionHook installs the verified artifact promotion callback.
func (m *Manager) SetArtifactPromotionHook(f func(string, []string, string, string) ([]string, error)) {
	m.promoteArtifacts = f
}

// SetArtifactStorage installs the cross-replica artifact authority. S3 mode
// keeps only disposable process-local scratch; every phase hand-off is
// rematerialized from immutable, token-scoped objects.
func (m *Manager) SetArtifactStorage(store artifactstorage.Storage) {
	m.artifactStore = store
	if m.objectQuarantineEnabled() {
		m.workerBroker.SetUploadRoot(m.artifactQuarantineBase())
		m.workerBroker.SetUploadObjectSink(m.storeGatewayUpload)
		return
	}
	m.workerBroker.SetUploadObjectSink(nil)
}

// SetGPGKeyProvider registers the isolated signer's public identity for
// verification and mirror publication. It is never used during deployment.
func (m *Manager) SetGPGKeyProvider(f func() (string, []byte, []byte)) {
	m.gpgKeyProvider = f
}

// SetWorkerIssuer installs the startup-validated workload issuer before any
// executor goroutine can request bootstrap material.
func (m *Manager) SetWorkerIssuer(issuer workergateway.Issuer) error {
	if issuer == nil {
		return fmt.Errorf("worker issuer is required")
	}
	m.workerIssuer = issuer
	return nil
}

// NewManager creates a new build manager.
func NewManager(cfg *config.ServerConfig) *Manager {
	var iacOpts []iac.ManagerOption
	if cfg.CloudInstanceTTL > 0 {
		iacOpts = append(iacOpts, iac.WithDefaultTTL(time.Duration(cfg.CloudInstanceTTL)*time.Minute))
	}
	if cfg.DataDir != "" {
		iacOpts = append(iacOpts, iac.WithWorkspaceDir(filepath.Join(cfg.DataDir, "terraform-workspaces")))
		if !cfg.Database.Enabled || !cfg.Database.Required {
			// Only a required database guarantees the durable cleanup ledger
			// exists — a failed optional open is merely logged, and the server
			// keeps provisioning VMs. Deciding the local state file from
			// Database.Enabled alone left that configuration with no VM record
			// anywhere: neither instances.json nor the cleanup queue, so a
			// restart orphaned every live instance.
			iacOpts = append(iacOpts, iac.WithStateFile(filepath.Join(cfg.DataDir, "instances.json")))
		}
	}

	controlPlaneID := strings.TrimSpace(cfg.ControlPlaneID)
	if controlPlaneID == "" {
		controlPlaneID, _ = os.Hostname()
	}
	if controlPlaneID == "" {
		controlPlaneID = uuid.NewString()
	}
	mgr := &Manager{
		config:       cfg,
		iacMgr:       iac.NewManager(iacOpts...),
		jobs:         make(map[string]*BuildStatus),
		workQueue:    make(chan *queuedJob, 100),
		remoteBuilds: make(map[string]string),
		stopCh:       make(chan struct{}),
		schedulerID:  fmt.Sprintf("control-plane/sec1-v%d/%s", ExecutorProtocolVersion, controlPlaneID),
		workerIssuer: workergateway.NewFileIssuer(
			cfg.WorkerGatewayIssuerID,
			cfg.WorkerGatewayIssuerCert,
			cfg.WorkerGatewayIssuerKey,
		),
	}
	mgr.workerBroker = workergateway.NewBroker(func(ctx context.Context, identity workergateway.Identity) error {
		if mgr.scheduler == nil {
			return fmt.Errorf("durable scheduler is unavailable")
		}
		mgr.jobsMu.RLock()
		job := mgr.jobs[identity.JobID]
		if job != nil {
			copyStatus := *job
			job = &copyStatus
		}
		mgr.jobsMu.RUnlock()
		if job == nil || job.AttemptID != identity.AttemptID ||
			job.FenceToken != identity.FenceToken {
			return fmt.Errorf("worker certificate does not match the active job attempt")
		}
		return mgr.scheduler.CheckClaim(ctx, job)
	})
	if cfg.BinpkgPath != "" {
		mgr.workerBroker.SetUploadRoot(mgr.artifactQuarantineBase())
	}
	initialCloudSettings := config.CloudSettingsFromServerConfig(cfg)
	initialCloudSettings.SkipVerifyInstall = false
	mgr.cloudSettings.Store(initialCloudSettings)
	// Restored IaC instances retain only non-secret Terraform state on disk.
	// Resolve destroy credentials from the current in-memory cloud settings so
	// a restart can reclaim them without ever persisting passwords or tokens in
	// instances.json.
	mgr.iacMgr.SetCredentialResolver(func(_ string) *iac.CloudCredentials {
		return mgr.cloudCredentials(mgr.CloudSettings())
	})
	mgr.buildCatalog.Store(catalog.NewCompatibility(catalog.CompatibilityOptions{
		Provider: cfg.CloudProvider, BuildMode: cfg.BuildMode,
		Template: cfg.CloudPVETemplate,
	}))

	return mgr
}

// WorkerGatewayHandler is mounted only on the dedicated mTLS listener.
func (m *Manager) WorkerGatewayHandler() http.Handler {
	return m.workerBroker.Handler()
}

func (m *Manager) WorkerGatewayStatus() workergateway.RuntimeStatus {
	return m.workerBroker.Status()
}

// WorkerIssuerStatus returns only redacted provider health metadata. Private
// keys, bearer tokens, certificates, and CSR material never cross this
// observability boundary.
func (m *Manager) WorkerIssuerStatus() workergateway.IssuerRuntimeStatus {
	return workergateway.IssuerStatus(m.workerIssuer)
}

// SetJobLedger installs the PostgreSQL job authority before durable jobs are
// projected into the manager. A nil ledger preserves standalone memory+JSON
// compatibility.
func (m *Manager) SetJobLedger(ledger JobLedger) {
	m.jobLedger = ledger
	if scheduler, ok := ledger.(DurableScheduler); ok {
		m.scheduler = scheduler
	}
	if compileQueue, ok := ledger.(CompileScheduler); ok {
		m.compileQueue = compileQueue
	}
	if phaseQueue, ok := ledger.(DurablePhaseScheduler); ok {
		m.phaseQueue = phaseQueue
	}
	if metadata, ok := ledger.(RuntimeMetadataLedger); ok {
		m.metadata = metadata
	}
	if artifacts, ok := ledger.(ArtifactBudgetLedger); ok {
		m.artifactDB = artifacts
	}
	if cleanup, ok := ledger.(InfraCleanupLedger); ok {
		m.infraCleanup = cleanup
	}
	if promotion, ok := ledger.(ArtifactPromotionLedger); ok {
		m.promotionDB = promotion
	}
	if coordinator, ok := ledger.(signing.Coordinator); ok {
		m.signing = coordinator
	}
	if logs, ok := ledger.(DurableLogLedger); ok {
		m.logLedger = logs
	}
	if gatewayStore, ok := ledger.(workergateway.DurableStore); ok {
		m.workerBroker.SetDurableStore(gatewayStore)
	}
}

// StartInfrastructureCleanup starts exactly one cleanup authority after cloud
// credentials/settings are loaded. PostgreSQL mode must never also run the
// process-local instances.json scanner.
func (m *Manager) StartInfrastructureCleanup() {
	m.cleanupOnce.Do(func() {
		go m.artifactQuarantineCleanupLoop()
		if m.infraCleanup != nil {
			go m.infraCleanupLoop(m.infraCleanup)
			return
		}
		m.iacMgr.StartCleanupRoutine()
	})
}

func (m *Manager) artifactQuarantineCleanupLoop() {
	m.cleanupExpiredArtifactQuarantines()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.cleanupExpiredArtifactQuarantines()
		}
	}
}

func (m *Manager) cleanupExpiredArtifactQuarantines() {
	if m.objectQuarantineEnabled() {
		m.cleanupExpiredObjectQuarantines()
	}
	base := m.artifactQuarantineBase()
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() || !capabilityTokenRegex.MatchString(entry.Name()) {
			continue
		}
		root := filepath.Join(base, entry.Name())
		marker, markerErr := os.ReadFile(filepath.Join(root, verificationCapabilityFile)) // #nosec G304 -- root comes from an entry returned by ReadDir and the marker name is fixed.
		if markerErr == nil {
			expiry, parseErr := strconv.ParseInt(strings.TrimSpace(string(marker)), 10, 64)
			if parseErr != nil || now.Unix() >= expiry {
				_ = os.RemoveAll(root)
			}
			continue
		}
		info, statErr := entry.Info()
		if statErr == nil && now.Sub(info.ModTime()) > quarantineOrphanAge {
			_ = os.RemoveAll(root)
		}
	}
}

func (m *Manager) cleanupExpiredObjectQuarantines() {
	keys, err := m.artifactStore.List(".quarantine/")
	if err != nil {
		fmt.Printf("Warning: object quarantine listing failed: %v\n", err)
		return
	}
	tokens := make(map[string]bool)
	for _, key := range keys {
		parts := strings.Split(key, "/")
		if len(parts) >= 3 && parts[0] == ".quarantine" &&
			capabilityTokenRegex.MatchString(parts[1]) {
			if len(parts) == 3 && parts[2] == verificationCapabilityFile {
				tokens[parts[1]] = true
			} else if _, exists := tokens[parts[1]]; !exists {
				tokens[parts[1]] = false
			}
		}
	}
	active := make(map[string]struct{})
	m.jobsMu.RLock()
	for _, job := range m.jobs {
		if capabilityTokenRegex.MatchString(job.VerificationToken) &&
			job.Status != "completed" && job.Status != "failed" &&
			job.Status != "canceled" {
			active[job.VerificationToken] = struct{}{}
		}
	}
	m.jobsMu.RUnlock()
	now := time.Now()
	for token, hasCapability := range tokens {
		if !hasCapability {
			if _, isActive := active[token]; isActive {
				continue
			}
			// A capability-less prefix is not evidence of an orphan: the signer
			// writes its artifacts, then Packages, then the capability marker,
			// and the control plane only learns that output token once the
			// signing task has completed. This replica's job map cannot see
			// another replica's in-flight token at all, because
			// VerificationToken is never persisted. Only the bucket's own
			// clock proves nobody is still writing.
			if !m.objectQuarantineIsOrphaned(token, now) {
				continue
			}
			if err := m.deleteObjectQuarantine(token); err != nil {
				fmt.Printf("Warning: orphaned object quarantine %s was not deleted: %v\n", token, err)
			}
			continue
		}
		key, keyErr := artifactstorage.QuarantineCapabilityKey(token)
		if keyErr != nil {
			continue
		}
		document, downloadErr := artifactstorage.DownloadBytes(
			m.artifactStore, key, m.artifactQuarantineBase(), 1<<20,
		)
		manifest, parseErr := artifactstorage.ParseQuarantineManifest(document, time.Time{})
		if downloadErr != nil || parseErr != nil || !now.Before(manifest.ExpiresAt) {
			if err := m.deleteObjectQuarantine(token); err != nil {
				fmt.Printf("Warning: expired object quarantine %s was not deleted: %v\n", token, err)
			}
		}
	}
}

// objectQuarantineIsOrphaned reports whether every object under the token's
// prefix is older than the orphan threshold. The threshold is far larger than
// the signer lease and the control plane's signer wait, so a generation that
// is still being written or verified anywhere in the cluster is never a
// deletion candidate. A backend that cannot report object times keeps them.
func (m *Manager) objectQuarantineIsOrphaned(token string, now time.Time) bool {
	prefix, err := artifactstorage.QuarantineGenerationPrefix(token)
	if err != nil {
		return false
	}
	newest, known, err := artifactstorage.NewestObjectTime(m.artifactStore, prefix)
	if err != nil {
		fmt.Printf("Warning: object quarantine %s age is unknown: %v\n", token, err)
		return false
	}
	return known && now.Sub(newest) > quarantineOrphanAge
}

// SetEphemeralHooks connects optional Redis acceleration. These callbacks must
// never be used to decide build correctness; PostgreSQL polling remains active.
func (m *Manager) SetEphemeralHooks(wake func(), event func(BuildStatus)) {
	m.wakeHook = wake
	m.eventHook = event
}

// StartWorkers starts the configured executor pool after the server has loaded
// its catalog, signing state, and persisted projection. A returned error means
// nothing polls for accepted builds, so the caller must surface it rather than
// keep answering 201 to submissions no executor will ever claim.
func (m *Manager) StartWorkers() error {
	m.workersMu.Lock()
	defer m.workersMu.Unlock()
	if m.workersStarted {
		return nil
	}
	runtimeRole := strings.TrimSpace(m.config.RuntimeRole)
	if runtimeRole == "" {
		runtimeRole = "control-plane"
	}
	if m.config.PhaseExecutorMode == "active" {
		if m.phaseQueue == nil || m.scheduler == nil ||
			!m.config.WorkerGatewayEnabled || len(m.remoteBuilders()) > 0 {
			return fmt.Errorf("active phase executor prerequisites are unavailable; no executor was started")
		}
	}
	if m.scheduler != nil {
		capabilities, err := m.resolveExecutorCapabilities()
		if err != nil {
			return fmt.Errorf("executor capabilities are invalid: %w; no executor was started", err)
		}
		m.executorCaps = capabilities
		if runtimeRunsAdmission(runtimeRole) {
			if observer, ok := m.scheduler.(DurableAutoscaleObserver); ok &&
				m.config.SchedulerAutoscaleMode != "" {
				go m.autoscaleObservationLoop(observer)
			}
		}
	}
	m.workersStarted = true
	if m.config.PhaseExecutorMode == "active" {
		// Admission is deliberately separate from execution. Capacity-
		// blocked phase work stays in PostgreSQL and consumes no executor
		// goroutine or VM slot.
		if runtimeRunsAdmission(runtimeRole) {
			// A restarted admission replica must resume renewal for phase plans
			// that are waiting for external executor capacity. Otherwise the
			// original two-minute admission lease can expire before a PVE clone
			// finishes and consume every job attempt without running a phase.
			m.jobsMu.RLock()
			for jobID, status := range m.jobs {
				if status != nil && status.AttemptID != "" &&
					!terminalStatus(status.Status) {
					go m.renewActivePhaseAttemptLoop(jobID)
				}
			}
			m.jobsMu.RUnlock()
			go m.durablePhaseAdmissionLoop()
		}
		if runtimeRunsPhaseExecution(runtimeRole) {
			for i := 0; i < m.config.MaxWorkers; i++ {
				go m.durablePhaseWorker(i)
			}
		}
		return nil
	}
	if runtimeRole == "api" || runtimeRole == "executor" {
		fmt.Printf(
			"Warning: separated runtime role %q requires active phase execution; no legacy worker was started\n",
			runtimeRole,
		)
		return nil
	}
	for i := 0; i < m.config.MaxWorkers; i++ {
		go m.worker(i)
	}
	return nil
}

func runtimeRunsAdmission(role string) bool {
	return role != "executor"
}

func runtimeRunsPhaseExecution(role string) bool {
	return role != "api"
}

func (m *Manager) autoscaleObservationLoop(observer DurableAutoscaleObserver) {
	interval := time.Duration(
		m.config.SchedulerAutoscaleIntervalSeconds,
	) * time.Second
	if interval < 5*time.Second {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	pools, err := m.resolveCapacityPools()
	if err != nil {
		fmt.Printf("Warning: scheduler capacity pools are invalid: %v\n", err)
		return
	}
	policy := SchedulerAutoscalePolicy{
		Mode:        m.config.SchedulerAutoscaleMode,
		MinSlots:    m.config.SchedulerAutoscaleMinSlots,
		MaxSlots:    m.config.SchedulerAutoscaleMaxSlots,
		TargetReady: m.config.SchedulerAutoscaleTargetReady,
		Cooldown: time.Duration(
			m.config.SchedulerAutoscaleCooldownSeconds,
		) * time.Second,
		ScaleDownDelay: time.Duration(
			m.config.SchedulerAutoscaleScaleDownSeconds,
		) * time.Second,
		Pools: pools, ProviderMaxSlots: maps.Clone(
			m.config.SchedulerAutoscaleProviderMaxSlots,
		),
	}
	if m.config.PhaseExecutorMode != "active" {
		// Shadow deployments still publish the catalog pool inventory and
		// observed capacity. Desired state is pinned to actual capacity and no
		// recommendation is emitted until the active executor cutover.
		policy.Mode = "off"
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := observer.ReconcileAutoscaling(ctx, policy)
		cancel()
		if err != nil {
			fmt.Printf("Warning: scheduler autoscale observation failed: %v\n", err)
		}
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
		}
	}
}

// GetJobsSnapshot returns a copy of all jobs for persistence.
func (m *Manager) GetJobsSnapshot() map[string]*BuildStatus {
	m.jobsMu.RLock()
	defer m.jobsMu.RUnlock()

	snapshot := make(map[string]*BuildStatus, len(m.jobs))
	for k, v := range m.jobs {
		statusCopy := *v
		snapshot[k] = &statusCopy
	}
	return snapshot
}

// LoadJobs loads a previously persisted set of jobs into the manager.
// Only non-terminal jobs that are not too old are marked as stale.
func (m *Manager) LoadJobs(jobs map[string]*BuildStatus) {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()

	for id, job := range jobs {
		if m.scheduler != nil {
			// PostgreSQL owns recovery in DB-2. Do not invent a terminal state
			// from a stale compatibility snapshot.
			m.jobs[id] = job
			continue
		}
		// Mark in-progress AND still-queued jobs as failed-on-restart: we cannot
		// resume in-progress work, and a persisted "queued" job cannot be
		// re-enqueued (BuildStatus does not retain the original request), so
		// leaving it "queued" would strand it forever and inflate QueuedBuilds.
		switch job.Status {
		case "queued", "claimed", "building", "provisioning", "deploying",
			"forwarding", "collecting", "verifying", "signing", "publishing":
			job.Status = "failed"
			job.Error = "server restarted before the job completed; please resubmit"
			job.UpdatedAt = time.Now()
		}
		m.jobs[id] = job
	}
}

// SyncLedgerJobs refreshes the in-memory read projection from PostgreSQL while
// preserving process-local logs and quarantine paths for the owning executor.
func (m *Manager) SyncLedgerJobs(durable map[string]*BuildStatus) {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	next := make(map[string]*BuildStatus, len(durable))
	for id, incoming := range durable {
		copyStatus := *incoming
		if local, ok := m.jobs[id]; ok {
			copyStatus.Log = local.Log
			copyStatus.ArtifactPaths = append([]string(nil), local.ArtifactPaths...)
			copyStatus.StagingRoot = local.StagingRoot
			copyStatus.VerificationToken = local.VerificationToken
			copyStatus.StagedArtifacts = append([]string(nil), local.StagedArtifacts...)
			copyStatus.StagedPrimary = local.StagedPrimary
			preserveLocalGenerationFacts(&copyStatus, local)
		}
		next[id] = &copyStatus
	}
	m.jobs = next
}

// preserveLocalGenerationFacts keeps the signing and promotion facts this
// replica established for the attempt the durable row still describes. Signing
// and promotion mutate memory first and become durable only on the next
// transition, so a reconciler tick landing inside the window between them
// would otherwise reinstate a pre-signing snapshot: the following verify would
// then run the install gate with signature checking off against a signed
// binhost, and a completed job could commit with an empty artifact URL. These
// facts only ever move forward, and a requeued attempt gets a new AttemptID,
// so the durable row wins again as soon as it carries them.
func preserveLocalGenerationFacts(durable *BuildStatus, local *BuildStatus) {
	if local.AttemptID == "" || local.AttemptID != durable.AttemptID {
		return
	}
	if local.Signed && !durable.Signed {
		durable.Signed = true
	}
	if durable.Signed && durable.SigningKeyID == "" {
		durable.SigningKeyID = local.SigningKeyID
	}
	if durable.ArtifactPath == "" {
		durable.ArtifactPath = local.ArtifactPath
	}
	if durable.ArtifactURL == "" {
		durable.ArtifactURL = local.ArtifactURL
	}
	if len(durable.Artifacts) == 0 {
		durable.Artifacts = append([]string(nil), local.Artifacts...)
	}
}

// Shutdown gracefully shuts down the manager.
// It closes the work queue and waits for IaC cleanup.
func (m *Manager) Shutdown() {
	m.shutdownOnce.Do(func() {
		m.submitMu.Lock()
		m.stopped = true
		close(m.stopCh)
		if m.scheduler == nil {
			close(m.workQueue)
		}
		m.submitMu.Unlock()
		// Give the IaC manager a chance to clean up
		m.iacMgr.StopCleanupRoutine()
	})
}

// SubmitBuild submits a new build request.
func (m *Manager) SubmitBuild(req *BuildRequest) (string, error) {
	if err := m.StartWorkers(); err != nil {
		return "", err
	}
	// Reject the complete request before it can allocate a job ID, provision a
	// VM, or be forwarded. Remote builders validate it again as defense in depth.
	if err := validateBuildRequest(req); err != nil {
		return "", &RequestError{err: fmt.Errorf("invalid build request: %w", err)}
	}
	if err := m.resolveBuildRequest(req); err != nil {
		return "", &RequestError{err: fmt.Errorf("resolve build environment: %w", err)}
	}

	m.submitMu.Lock()
	defer m.submitMu.Unlock()
	if m.stopped {
		return "", fmt.Errorf("builder manager is shutting down")
	}
	if m.scheduler == nil && len(m.workQueue) >= cap(m.workQueue) {
		return "", fmt.Errorf("work queue is full")
	}

	jobID := uuid.New().String()

	status := &BuildStatus{
		JobID:           jobID,
		ProjectID:       req.ProjectID,
		RequestedBy:     req.RequestedBy,
		Status:          "queued",
		PackageName:     req.PackageName,
		Version:         req.Version,
		Arch:            req.Arch,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
		ResolvedContext: req.ResolvedContext,
		Request:         req,
	}

	if m.jobLedger != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := m.jobLedger.CreateJob(ctx, req, status)
		cancel()
		if err != nil {
			return "", &LedgerError{err: fmt.Errorf("persist build request: %w", err)}
		}
		if !result.Created {
			m.jobsMu.RLock()
			_, inMemory := m.jobs[result.JobID]
			m.jobsMu.RUnlock()
			if inMemory {
				m.signalWorkers()
				m.emitStatus(result.Status)
				return result.JobID, nil
			}
			recovered := result.Status
			if recovered == nil {
				return "", fmt.Errorf("idempotent job %s has no ledger status", result.JobID)
			}
			// DB-1 cannot resume a ledger-only queued job because its channel
			// remains authoritative. DB-2 can reclaim it from PostgreSQL.
			if m.scheduler == nil && !terminalStatus(recovered.Status) {
				previous := *recovered
				recovered.Status = "failed"
				recovered.Error = "server restarted before the legacy queue snapshot was durable; please resubmit with a new idempotency key"
				recovered.UpdatedAt = time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := m.jobLedger.RecordTransition(ctx, &previous, recovered)
				cancel()
				if err != nil {
					return "", &LedgerError{err: fmt.Errorf("reconcile idempotent job: %w", err)}
				}
			}
			m.jobsMu.Lock()
			m.jobs[result.JobID] = recovered
			m.jobsMu.Unlock()
			m.signalWorkers()
			m.emitStatus(recovered)
			return result.JobID, nil
		}
	}

	m.jobsMu.Lock()
	m.jobs[jobID] = status
	m.jobsMu.Unlock()
	m.appendJobLog(jobID, fmt.Sprintf("[queued] request accepted for %s-%s (%s)", req.PackageName, req.Version, req.Arch))

	if m.scheduler != nil {
		m.signalWorkers()
		m.emitStatus(status)
		return jobID, nil
	}

	// Enqueue the job ID alongside the request so the worker processes exactly
	// this job. If the queue is full, remove the just-added job so it does not
	// linger as a permanently "queued" orphan.
	select {
	case m.workQueue <- &queuedJob{jobID: jobID, req: req}:
		return jobID, nil
	default:
		m.jobsMu.Lock()
		delete(m.jobs, jobID)
		m.jobsMu.Unlock()
		return "", fmt.Errorf("work queue is full")
	}
}

func (m *Manager) signalWorkers() {
	if m.scheduler == nil {
		return
	}
	select {
	case m.workQueue <- nil:
	default:
	}
	if m.wakeHook != nil {
		go m.wakeHook()
	}
}

// WakeWorkersLocal wakes this replica after a Redis notification. It does not
// publish another notification, preventing a cross-replica feedback loop.
func (m *Manager) WakeWorkersLocal() {
	m.submitMu.Lock()
	defer m.submitMu.Unlock()
	if m.stopped || m.scheduler == nil {
		return
	}
	select {
	case m.workQueue <- nil:
	default:
	}
}

func (m *Manager) emitStatus(status *BuildStatus) {
	if status == nil || m.eventHook == nil {
		return
	}
	copyStatus := *status
	go m.eventHook(copyStatus)
}

// versionRegex matches version patterns like "3.9.9" or "7.1.0-r1" in artifact filenames.
var (
	versionRegex         = regexp.MustCompile(`-(\d+\.\d+(?:\.\d+)?(?:-r\d+)?)-\d+\.gpkg\.tar$`)
	sha256DigestRegex    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	capabilityTokenRegex = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

const verificationCapabilityFile = signing.CapabilityMarkerName

// quarantineOrphanAge is how old every byte of a capability-less quarantine
// generation must be before cleanup may delete it. It is far larger than the
// signer lease and SIGNER_WAIT_TIMEOUT_SECONDS so an in-flight generation on
// any replica is never a candidate.
const quarantineOrphanAge = 24 * time.Hour

// extractVersionFromArtifact tries to extract version from artifact path.
// Example: "/var/tmp/portage-artifacts/screenfetch-3.9.9-1.gpkg.tar" -> "3.9.9"
func extractVersionFromArtifact(artifactPath string) string {
	if artifactPath == "" {
		return ""
	}

	filename := filepath.Base(artifactPath)
	matches := versionRegex.FindStringSubmatch(filename)
	if len(matches) >= 2 {
		return matches[1]
	}

	return ""
}

// GetStatus returns the status of a build job.
// It first checks local jobs, then queries remote builders if not found locally.
func (m *Manager) GetStatus(jobID string) (*BuildStatus, error) {
	// Check local jobs first. Return a copy taken under the lock so callers
	// (e.g. the JSON-encoding status handler) never read the live struct while
	// updateStatus is writing it.
	m.jobsMu.RLock()
	status, exists := m.jobs[jobID]
	if exists {
		statusCopy := *status
		statusCopy.Error = redactJobLogLine(statusCopy.Error)
		statusCopy.Log = redactJobLogLine(statusCopy.Log)
		m.jobsMu.RUnlock()
		return &statusCopy, nil
	}
	m.jobsMu.RUnlock()

	if m.scheduler != nil {
		return nil, fmt.Errorf("job not found in PostgreSQL projection: %s", jobID)
	}

	// Not found locally, try remote builders
	if len(m.remoteBuilders()) > 0 {
		remoteStatus := m.fetchRemoteJobStatus(jobID)
		if remoteStatus != nil {
			return remoteStatus, nil
		}
	}

	return nil, fmt.Errorf("job not found: %s", jobID)
}

// fetchRemoteJobStatus fetches a specific job's status from remote builders.
func (m *Manager) fetchRemoteJobStatus(jobID string) *BuildStatus {
	client := &http.Client{Timeout: 5 * time.Second}

	for _, builderAddr := range m.remoteBuilders() {
		baseURL := normalizeBuilderURL(builderAddr)
		url := fmt.Sprintf("%s/api/v1/jobs/%s", baseURL, jobID)
		resp, err := m.builderGet(client, url)
		if err != nil {
			continue
		}

		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			continue
		}

		var job struct {
			ID      string `json:"id"`
			Request struct {
				PackageName string `json:"package_name"`
				Version     string `json:"version"`
				Arch        string `json:"arch"`
			} `json:"request"`
			Status      string    `json:"status"`
			StartTime   time.Time `json:"start_time"`
			EndTime     time.Time `json:"end_time"`
			Log         string    `json:"log"`
			ArtifactURL string    `json:"artifact_url"`
			Error       string    `json:"error"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
			_ = resp.Body.Close()
			continue
		}
		_ = resp.Body.Close()

		arch := job.Request.Arch
		if arch == "" {
			arch = "amd64" // Default architecture
		}

		version := job.Request.Version
		if version == "" {
			version = extractVersionFromArtifact(job.ArtifactURL)
		}

		status := &BuildStatus{
			JobID:        job.ID,
			PackageName:  job.Request.PackageName,
			Version:      version,
			Arch:         arch,
			Status:       job.Status,
			CreatedAt:    job.StartTime,
			UpdatedAt:    job.EndTime,
			InstanceID:   builderAddr,
			ArtifactPath: job.ArtifactURL,
			Log:          redactJobLogLine(job.Log),
			Error:        redactJobLogLine(job.Error),
		}

		// Normalize status names
		if status.Status == "success" {
			status.Status = "completed"
		}

		return status
	}

	return nil
}

// claimJob atomically transitions a job from "queued" to "claimed" under the
// write lock. It returns true only for the caller that performed the
// transition; a job that is missing or already past "queued" returns false.
func (m *Manager) claimJob(jobID string) (bool, error) {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()

	job, ok := m.jobs[jobID]
	if !ok || job.Status != "queued" {
		return false, nil
	}
	previous := *job
	next := previous
	next.Status = "claimed"
	next.UpdatedAt = time.Now()
	if m.jobLedger != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := m.jobLedger.RecordTransition(ctx, &previous, &next)
		cancel()
		if err != nil {
			return false, err
		}
	}
	job.Status = next.Status
	job.UpdatedAt = next.UpdatedAt
	return true, nil
}

// worker processes legacy channel jobs or atomically claims PostgreSQL queue
// rows when DB-2 is enabled.
func (m *Manager) worker(slot int) {
	if m.scheduler != nil {
		m.durableWorker(slot)
		return
	}
	for qj := range m.workQueue {
		if qj == nil {
			continue
		}
		m.processBuild(qj)
	}
}

func (m *Manager) durableWorker(slot int) {
	workerName := fmt.Sprintf("%s/slot-%d", m.schedulerID, slot)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		claim, err := m.scheduler.ClaimNext(
			ctx, workerName, 30*time.Second,
			workerKindCapabilities(m.executorCaps, "legacy"),
		)
		cancel()
		if err == nil && claim != nil {
			m.jobsMu.Lock()
			m.jobs[claim.Status.JobID] = claim.Status
			m.jobsMu.Unlock()
			m.appendRuntimeBudgetReservationLog(
				claim.Status.JobID, claim.Request,
			)

			renewStop := make(chan struct{})
			go m.renewClaimLoop(claim.Status.JobID, renewStop)
			m.processClaimedBuild(&queuedJob{jobID: claim.Status.JobID, req: claim.Request})
			close(renewStop)
			continue
		}

		select {
		case <-m.stopCh:
			return
		case <-m.workQueue:
		case <-ticker.C:
		}
	}
}

func (m *Manager) appendRuntimeBudgetReservationLog(
	jobID string,
	request *BuildRequest,
) {
	minutes := 60
	rate := int64(1000)
	if request != nil && request.ResolvedContext != nil {
		if request.ResolvedContext.MaxRuntimeMinutes > 0 {
			minutes = request.ResolvedContext.MaxRuntimeMinutes
		}
		if request.ResolvedContext.CloudCostMicrounitsPerMinute > 0 {
			rate = request.ResolvedContext.CloudCostMicrounitsPerMinute
		}
	}
	m.appendJobLog(jobID, fmt.Sprintf(
		"[budget] reserved max_runtime=%dm cloud_cost=%d microunits rate=%d/min accounting_day=UTC",
		minutes, int64(minutes)*rate, rate,
	))
}

func (m *Manager) renewClaimLoop(jobID string, stop <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.jobsMu.RLock()
			status, ok := m.jobs[jobID]
			if ok {
				copyStatus := *status
				status = &copyStatus
			}
			m.jobsMu.RUnlock()
			if !ok || terminalStatus(status.Status) {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = m.scheduler.RenewClaim(ctx, status, 30*time.Second)
			cancel()
		}
	}
}

// infraCleanupLoop continuously claims abandoned/expired Terraform workspaces.
// It deliberately uses a separate lease from the build attempt: once an
// attempt fence expires, no retrying builder can race cleanup of its old VM.
func (m *Manager) infraCleanupLoop(ledger InfraCleanupLedger) {
	owner := m.schedulerID + "/infra-cleaner"
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		m.runOneInfraCleanup(ledger, owner)
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) runOneInfraCleanup(ledger InfraCleanupLedger, owner string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	claim, err := ledger.ClaimInfraCleanup(ctx, owner, 35*time.Minute)
	cancel()
	if err != nil || claim == nil {
		return
	}

	err = m.iacMgr.DestroyRecorded(claim.Provider, claim.ProviderInstanceID, claim.RemoteStateRef)
	resultCtx, resultCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resultCancel()
	if err != nil {
		_ = ledger.FailInfraCleanup(resultCtx, claim.ID, owner, claim.CleanupFence, redactJobLogLine(err.Error()))
		return
	}
	if err := ledger.CompleteInfraCleanup(resultCtx, claim.ID, owner, claim.CleanupFence); err == nil {
		_ = m.iacMgr.RemoveRecordedWorkspace(claim.RemoteStateRef)
	}
}

// processBuild processes a single queued job. It atomically claims the job
// (queued -> claimed) so that even if the same job were enqueued twice, only one
// worker proceeds — no scanning, no double-processing.
func (m *Manager) processBuild(qj *queuedJob) {
	jobID := qj.jobID

	for {
		claimed, err := m.claimJob(jobID)
		if err == nil {
			if !claimed {
				// Already claimed (or gone) — another worker owns it, or it was removed.
				return
			}
			break
		}
		select {
		case <-m.stopCh:
			return
		case <-time.After(2 * time.Second):
			// PostgreSQL is required for new claims in DB-1. Keep the legacy
			// queue item owned by this worker and retry after a short backoff.
		}
	}

	m.processClaimedBuild(qj)
}

func (m *Manager) processClaimedBuild(qj *queuedJob) {
	jobID := qj.jobID
	req := qj.req

	// Prefer a configured static remote builder.
	if len(m.remoteBuilders()) > 0 {
		m.submitToRemoteBuilder(jobID, req)
		return
	}

	// Otherwise provision a cloud builder on demand and run the build there.
	m.processCloudBuild(jobID, req)
}

// processCloudBuild provisions a cloud instance, deploys a builder on it, runs
// the build there, and tears the instance down afterward. It runs synchronously
// in the worker goroutine and always terminates the instance (via defer) so a
// build failure never leaks a billed VM.
func (m *Manager) processCloudBuild(jobID string, req *BuildRequest) {
	provReq, err := m.buildProvisionRequest(req)
	if err != nil {
		// Refuse rather than fabricate success when cloud builds are not
		// configured (previously this path faked a completed build).
		m.setFailedStage(jobID, "provision")
		m.appendJobLog(jobID, "[provision] configuration rejected: "+err.Error())
		m.updateStatus(jobID, "failed", "", err.Error())
		return
	}
	var pullIdentity *workergateway.Identity
	if m.config.WorkerGatewayEnabled {
		identity, prepareErr := m.prepareWorkerPull(jobID, provReq)
		if prepareErr != nil {
			m.setFailedStage(jobID, "deploy")
			m.appendJobLog(jobID, "[deploy] worker identity rejected: "+prepareErr.Error())
			m.updateStatus(jobID, "failed", "", prepareErr.Error())
			return
		}
		pullIdentity = identity
		defer m.workerBroker.Unregister(identity.WorkerID)
		m.appendJobLog(jobID, fmt.Sprintf(
			"[deploy] issued attempt-bound worker identity worker=%s attempt=%s fence=%d",
			identity.WorkerID, identity.AttemptID, identity.FenceToken))
	}

	var infraFence atomic.Pointer[BuildStatus]
	m.installProvisionCallbacks(jobID, provReq, &infraFence)

	// Every build receives a fresh native Gentoo instance/root. There is no
	// warm-pool path: emerge mutates VDB, installed files and arbitrary ebuild
	// post-install state.
	if !m.updateStatus(jobID, "provisioning", "", "") {
		return
	}
	m.appendJobLog(jobID, fmt.Sprintf("[provision] provisioning a fresh native %s instance for %s…", provReq.Provider, req.PackageName))
	instance, err := m.iacMgr.Provision(provReq)
	if err != nil {
		stage := "provision"
		if strings.Contains(err.Error(), "deployment failed") {
			stage = "deploy"
		}
		m.setFailedStage(jobID, stage)
		m.appendJobLog(jobID, fmt.Sprintf("[provision] provisioning failed: %v", err))
		m.updateStatus(jobID, "failed", "", fmt.Sprintf("provisioning failed: %v", err))
		return
	}
	m.iacMgr.SetInstanceActiveTasks(instance.ID, 1)
	if err := m.recordInfra(jobID, instance, "running", "", nil); err != nil {
		m.appendJobLog(jobID, "[provision] durable infra ownership failed: "+err.Error())
		_ = m.iacMgr.Terminate(instance.ID)
		m.setFailedStage(jobID, "provision")
		m.updateStatus(jobID, "failed", instance.ID, "persist infrastructure ownership: "+err.Error())
		return
	}
	cleanupFence, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		m.appendJobLog(jobID, "[provision] durable cleanup fence snapshot failed: "+err.Error())
		_ = m.iacMgr.Terminate(instance.ID)
		m.setFailedStage(jobID, "provision")
		m.updateStatus(jobID, "failed", instance.ID, "snapshot infrastructure cleanup fence: "+err.Error())
		return
	}
	infraFence.Store(cleanupFence)

	pipelineClean := true
	defer func() {
		m.cleanupCloudInstance(jobID, instance, cleanupFence, pipelineClean)
	}()

	if pullIdentity != nil {
		instance.BuilderEndpoint = "pull://" + pullIdentity.WorkerID
	}
	if instance.BuilderEndpoint == "" {
		pipelineClean = false
		m.updateStatus(jobID, "failed", instance.ID, "provisioned instance has no builder endpoint")
		return
	}

	if err := m.waitForProvisionedBuilder(
		jobID, instance, provReq, pullIdentity, cleanupFence,
	); err != nil {
		pipelineClean = false
		m.setFailedStage(jobID, "deploy")
		m.updateStatus(jobID, "failed", instance.ID, err.Error())
		return
	}

	if !m.updateStatus(jobID, "building", instance.ID, "") {
		pipelineClean = false
		return
	}
	m.appendJobLog(jobID, "[build] submitting build to the instance builder…")

	// Submit and wait for the build on the builder, then pull the resulting
	// artifact back to the server's binpkg dir.
	if pullIdentity != nil {
		err = m.runBuildOnPullWorker(jobID, instance, req, *pullIdentity)
	} else {
		err = m.runBuildOnInstance(jobID, instance, req)
	}
	if err != nil {
		pipelineClean = false
		stage := "build"
		if strings.Contains(err.Error(), "artifact retrieval failed") {
			stage = "collect"
		}
		m.setFailedStage(jobID, stage)
		m.updateStatus(jobID, "failed", instance.ID, err.Error())
		return
	}

	if stage, err := m.verifyAndPublish(jobID, instance.BuilderEndpoint, instance.ID, req); err != nil {
		pipelineClean = false
		m.setFailedStage(jobID, stage)
		m.cleanupArtifactQuarantine(jobID)
		m.updateStatus(jobID, "failed", instance.ID, err.Error())
		return
	}
}

func (m *Manager) installProvisionCallbacks(
	jobID string,
	request *iac.ProvisionRequest,
	infraFence *atomic.Pointer[BuildStatus],
) {
	var deployingOnce sync.Once
	request.LogSink = func(line string) {
		m.appendJobLog(jobID, line)
		if strings.HasPrefix(line, "[deploy]") {
			deployingOnce.Do(func() {
				m.updateStatus(jobID, "deploying", "", "")
			})
		}
	}
	request.Lifecycle = func(
		instance *iac.Instance,
		state, failure string,
		deletedAt *time.Time,
	) error {
		if captured := infraFence.Load(); captured != nil {
			return m.recordInfraWithStatus(
				captured, instance, state, failure, deletedAt,
			)
		}
		return m.recordInfra(jobID, instance, state, failure, deletedAt)
	}
}

func (m *Manager) cleanupCloudInstance(
	jobID string,
	instance *iac.Instance,
	cleanupFence *BuildStatus,
	pipelineClean bool,
) {
	label := "tainted"
	reason := "pipeline failure"
	if pipelineClean {
		label = "single-use native"
		reason = "successful publication"
	}
	m.appendJobLog(jobID, fmt.Sprintf(
		"[cleanup] destroying %s instance %s after %s",
		label, instance.ID, reason,
	))
	termErr := m.iacMgr.Terminate(instance.ID)
	if termErr != nil {
		m.appendJobLog(jobID, fmt.Sprintf(
			"[cleanup] destroy failed; cleanup routine will retry: %v",
			termErr,
		))
	}
	state := "destroyed"
	var deletedAt *time.Time
	if termErr != nil {
		state = "destroy_failed"
	} else {
		now := time.Now().UTC()
		deletedAt = &now
	}
	if err := m.recordInfraWithStatus(
		cleanupFence, instance, state, errorString(termErr), deletedAt,
	); err != nil {
		m.appendJobLog(
			jobID,
			"[cleanup] warning: durable infra result was not recorded: "+err.Error(),
		)
	}
}

func (m *Manager) waitForProvisionedBuilder(
	jobID string,
	instance *iac.Instance,
	provision *iac.ProvisionRequest,
	pullIdentity *workergateway.Identity,
	cleanupFence *BuildStatus,
) error {
	if pullIdentity == nil {
		if !m.waitForBuilderReady(jobID, instance) {
			return fmt.Errorf("builder did not become ready after deployment")
		}
		return nil
	}
	readyCtx, cancelReady := context.WithTimeout(
		context.Background(), 3*time.Minute,
	)
	err := m.workerBroker.WaitConnected(readyCtx, pullIdentity.WorkerID)
	cancelReady()
	if err != nil {
		return fmt.Errorf("outbound worker did not connect: %w", err)
	}
	m.appendJobLog(
		jobID,
		"[deploy] outbound worker mTLS identity and active attempt fence verified",
	)
	if instance.Provider != "pve" ||
		instance.Metadata["pe_egress_enforced"] != "true" {
		return nil
	}
	if err := m.closePVETransientInbound(
		instance, provision, cleanupFence,
	); err != nil {
		return err
	}
	m.appendJobLog(
		jobID,
		"[policy] transient SSH bootstrap window closed; VM policy_in=DROP readback verified",
	)
	return nil
}

func (m *Manager) closePVETransientInbound(
	instance *iac.Instance,
	provision *iac.ProvisionRequest,
	cleanupFence *BuildStatus,
) error {
	auth := iac.PVEAuth{Insecure: provision.Spec["insecure"] == "true"}
	if provision.Credentials != nil {
		auth.TokenID = provision.Credentials.PVETokenID
		auth.TokenSecret = provision.Credentials.PVETokenSecret
		auth.Username = provision.Credentials.PVEUsername
		auth.Password = provision.Credentials.PVEPassword
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err := iac.ClosePVEVMInbound(
		ctx, provision.Spec["endpoint"], auth,
		instance.Metadata["node"], instance.Metadata["vmid"],
	)
	cancel()
	if err != nil {
		return fmt.Errorf("close transient PVE inbound access: %w", err)
	}
	instance.Metadata["pe_inbound_closed"] = "true"
	if err := m.recordInfraWithStatus(
		cleanupFence, instance, "running", "", nil,
	); err != nil {
		return fmt.Errorf("persist closed PVE inbound boundary: %w", err)
	}
	return nil
}

// waitForBuilderReady polls the instance builder's /health until it responds
// or a timeout elapses (the builder binds its port a moment after the deploy
// script's systemctl restart returns). Returns false if it never comes up.
func (m *Manager) waitForBuilderReady(jobID string, instance *iac.Instance) bool {
	baseURL := normalizeBuilderURL(instance.BuilderEndpoint)
	for attempt := 0; attempt < 24; attempt++ {
		resp, err := builderHTTPClient.Get(baseURL + "/health")
		if err == nil {
			ok := resp.StatusCode == http.StatusOK
			_ = resp.Body.Close()
			if ok {
				if attempt > 0 {
					m.appendJobLog(jobID, "[deploy] builder is up")
				}
				return true
			}
		}
		if attempt == 0 {
			m.appendJobLog(jobID, "[deploy] waiting for the builder service to come up…")
		}
		time.Sleep(5 * time.Second)
	}
	return false
}

func (m *Manager) prepareWorkerPull(jobID string, provision *iac.ProvisionRequest) (*workergateway.Identity, error) {
	status, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return nil, err
	}
	identity := workergateway.Identity{
		WorkerID: uuid.NewString(), JobID: jobID,
		AttemptID: status.AttemptID, FenceToken: status.FenceToken,
	}
	return m.prepareWorkerPullIdentity(jobID, provision, identity)
}

func (m *Manager) prepareWorkerPullIdentity(
	jobID string,
	provision *iac.ProvisionRequest,
	identity workergateway.Identity,
) (*workergateway.Identity, error) {
	if m.scheduler == nil {
		return nil, fmt.Errorf("outbound worker gateway requires the durable PostgreSQL scheduler")
	}
	status, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return nil, err
	}
	expected := workergateway.Identity{
		WorkerID: identity.WorkerID, JobID: jobID,
		AttemptID: status.AttemptID, FenceToken: status.FenceToken,
	}
	if identity != expected {
		return nil, fmt.Errorf("worker identity does not match the active phase attempt")
	}
	gatewayCA, err := os.ReadFile(m.config.WorkerGatewayServerCA)
	if err != nil {
		return nil, fmt.Errorf("read worker gateway CA: %w", err)
	}
	issued, err := m.workerIssuer.Issue(
		context.Background(), identity,
		time.Duration(m.config.WorkerCertificateTTLMin)*time.Minute,
	)
	if err != nil {
		return nil, err
	}
	if err := m.workerBroker.RegisterCertificate(identity, issued.Record); err != nil {
		return nil, err
	}
	provision.WorkerPull = &iac.WorkerPullConfig{
		GatewayURL: m.config.WorkerGatewayAdvertiseURL,
		CertPEM:    issued.CertPEM, KeyPEM: issued.KeyPEM, CAPEM: gatewayCA,
	}
	provision.BuilderToken = ""
	return &identity, nil
}

func (m *Manager) runBuildOnPullWorker(
	jobID string,
	instance *iac.Instance,
	req *BuildRequest,
	identity workergateway.Identity,
) error {
	return m.runBuildOnPullWorkerStable(jobID, instance, req, identity, "")
}

func (m *Manager) runBuildOnPullWorkerStable(
	jobID string,
	instance *iac.Instance,
	req *BuildRequest,
	identity workergateway.Identity,
	commandNamespace string,
) error {
	return m.runBuildOnPullWorkerStableFenced(
		jobID, instance, req, identity, commandNamespace, nil,
	)
}

func (m *Manager) runBuildOnPullWorkerStableFenced(
	jobID string,
	instance *iac.Instance,
	req *BuildRequest,
	identity workergateway.Identity,
	commandNamespace string,
	resultFence func() error,
) error {
	compileLease, finishCompile, err := m.beginCompileLease(
		jobID, identity.WorkerID, "worker-cert:"+identity.WorkerID, req,
	)
	if err != nil {
		return err
	}
	compileFailure := ""
	defer func() { finishCompile(compileFailure) }()
	claimed, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return err
	}
	localReq := LocalBuildRequest{
		PackageName: req.PackageName, Version: req.Version, Arch: req.Arch,
		ProfileID: req.ProfileID, RepositoryIDs: append([]string(nil), req.RepositoryIDs...),
		ResourceClass: req.ResourceClass, UseFlags: make(map[string]string),
		Environment: make(map[string]string), ConfigBundle: req.ConfigBundle,
		ProjectID: claimed.ProjectID, AttemptID: claimed.AttemptID,
		AttemptFence: claimed.FenceToken,
		CompileLease: compileLease,
	}
	for _, flag := range req.UseFlags {
		if name, disabled := strings.CutPrefix(flag, "-"); disabled {
			localReq.UseFlags[name] = "disabled"
		} else {
			localReq.UseFlags[flag] = "enabled"
		}
	}
	timeout := 2 * time.Hour
	if instance.TTL > 0 && instance.TTL < timeout {
		timeout = instance.TTL
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var remote BuildJob
	dispatch := m.workerBroker.Dispatch
	if commandNamespace != "" {
		commandID := stablePhaseUUID(commandNamespace, "build")
		dispatch = func(
			ctx context.Context, identity workergateway.Identity,
			action string, request, response any,
		) error {
			return m.workerBroker.DispatchID(
				ctx, identity, commandID, action, request, response,
			)
		}
	}
	if err := dispatch(ctx, identity, workergateway.ActionBuild, localReq, &remote); err != nil {
		compileFailure = "connect"
		return fmt.Errorf("outbound worker build failed: %w", err)
	}
	m.recordCompileReport(jobID, compileLease, remote.Metadata)
	if remote.Status == "failed" {
		compileFailure = "remote-compile"
		return fmt.Errorf("outbound worker build failed: %s", remote.Error)
	}
	if reason, err := m.checkCompileOutput(jobID, compileLease); err != nil {
		compileFailure = reason
		return fmt.Errorf("distcc output fence rejected before collection: %w", err)
	} else if reason != "" {
		compileFailure = reason
	}
	if resultFence != nil {
		if err := resultFence(); err != nil {
			return fmt.Errorf("build result phase fence rejected: %w", err)
		}
	}
	lastLog := ""
	m.appendRemoteBuildLog(jobID, remote.Log, &lastLog)
	m.iacMgr.UpdateInstanceActivity(instance.ID)
	if commandNamespace == "" {
		if !m.updateStatus(jobID, "collecting", instance.ID, "") {
			return fmt.Errorf("durable collect phase transition was rejected")
		}
	} else {
		m.appendJobLog(jobID,
			"[collect] collection remains fenced inside the active build phase")
	}
	signed, _ := remote.Metadata["signed"].(bool)
	snapshot := &remoteJobSnapshot{
		Status: remote.Status, Error: remote.Error, Log: remote.Log,
		ArtifactURL: remote.ArtifactURL, Artifacts: append([]string(nil), remote.Artifacts...),
		Signed: signed, Terminal: true, Metadata: remote.Metadata,
	}
	if len(snapshot.Artifacts) == 0 {
		return fmt.Errorf("outbound worker completed without a recorded artifact list")
	}
	commitFence := func() error {
		reason, err := m.checkCompileOutput(jobID, compileLease)
		if reason != "" {
			compileFailure = reason
		}
		if err != nil {
			return err
		}
		if resultFence != nil {
			return resultFence()
		}
		return nil
	}
	if err := m.collectPullArtifacts(
		jobID, identity, remote.ID, req.PackageName, snapshot, commandNamespace,
		commitFence,
	); err != nil {
		return fmt.Errorf("artifact retrieval failed: %w", err)
	}
	return nil
}

// verifyAndPublish runs the mandatory unsigned install gate, delegates
// digest-bound GPKG signing to the isolated signer, verifies the signed result,
// and only then promotes the complete set into the public binhost.
func (m *Manager) verifyAndPublish(jobID, builderEndpoint, instanceID string, req *BuildRequest) (stage string, resultErr error) {
	defer func() {
		if resultErr != nil {
			m.cleanupArtifactQuarantine(jobID)
		}
	}()

	verifyBinhost, err := m.prepareVerificationBinhost(jobID, req.Arch)
	if err != nil {
		return "collect", fmt.Errorf("prepare quarantined verification binhost: %w", err)
	}
	if err := m.verifyOnBuilder(jobID, builderEndpoint, instanceID, req, verifyBinhost); err != nil {
		return "verify", fmt.Errorf("install verification failed: %w", err)
	}

	if m.config.GPGEnabled {
		if err := m.verifyUnsignedRejectedOnBuilder(jobID, builderEndpoint, req, verifyBinhost); err != nil {
			return "verify", fmt.Errorf("unsigned negative-control verification failed: %w", err)
		}
		if err := m.revokeArtifactCapability(jobID); err != nil {
			return "sign", fmt.Errorf("revoke unsigned verification capability: %w", err)
		}
		if !m.updateStatus(jobID, "signing", instanceID, "") {
			return "sign", fmt.Errorf("durable signing phase transition was rejected")
		}
		m.appendJobLog(jobID, "[sign] submitting verified unsigned artifact digests to the isolated signer…")
		if err := m.requireActiveClaim(jobID); err != nil {
			return "sign", fmt.Errorf("durable claim fence rejected signing: %w", err)
		}
		if err := m.signJobArtifacts(jobID); err != nil {
			return "sign", fmt.Errorf("isolated signing failed: %w", err)
		}
		m.appendJobLog(jobID, "[sign] signed GPKG generation passed signer self-verification")
		signedBinhost, err := m.prepareVerificationBinhost(jobID, req.Arch)
		if err != nil {
			return "sign", fmt.Errorf("prepare signed verification binhost: %w", err)
		}
		if err := m.verifyOnBuilder(jobID, builderEndpoint, instanceID, req, signedBinhost); err != nil {
			return "verify", fmt.Errorf("signed install verification failed: %w", err)
		}
	}

	if !m.updateStatus(jobID, "publishing", instanceID, "") {
		return "publish", fmt.Errorf("durable publish phase transition was rejected")
	}
	m.appendJobLog(jobID, "[publish] promoting verified artifacts into the public binhost…")
	if err := m.requireActiveClaim(jobID); err != nil {
		return "publish", fmt.Errorf("durable claim fence rejected publication: %w", err)
	}
	if err := m.promoteJobArtifacts(jobID); err != nil {
		return "publish", fmt.Errorf("artifact promotion failed: %w", err)
	}
	m.appendJobLog(jobID, "[publish] Packages index atomically updated")

	if up := newMirrorUploader(m.CloudSettings()); up != nil {
		m.appendJobLog(jobID, "[publish] uploading promoted artifacts to mirror "+up.base+"…")
		if err := m.uploadJobToMirror(jobID, up); err != nil {
			m.appendJobLog(jobID, "[publish] warning: secondary mirror update failed: "+err.Error())
		} else {
			m.appendJobLog(jobID, "[publish] secondary mirror updated: "+up.binhostURL())
		}
	}
	if !m.updateStatus(jobID, "completed", instanceID, "") {
		return "publish", fmt.Errorf("durable completion transition was rejected")
	}
	m.removeArtifactQuarantineFiles(jobID)
	return "", nil
}

func (m *Manager) revokeArtifactCapability(jobID string) error {
	m.jobsMu.RLock()
	job := m.jobs[jobID]
	root, token := "", ""
	if job != nil {
		root = job.StagingRoot
		token = job.VerificationToken
	}
	m.jobsMu.RUnlock()
	if m.objectQuarantineEnabled() && token != "" {
		key, err := artifactstorage.QuarantineCapabilityKey(token)
		if err != nil {
			return err
		}
		if err := m.artifactStore.Delete(key); err != nil {
			return fmt.Errorf("delete object capability marker: %w", err)
		}
		return nil
	}
	if root != "" {
		err := os.Remove(filepath.Join(root, verificationCapabilityFile))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (m *Manager) signJobArtifacts(jobID string) error {
	if m.signing == nil {
		return fmt.Errorf("GPG_ENABLED requires PostgreSQL signing queue and an independent portage-signer")
	}
	m.jobsMu.RLock()
	job, ok := m.jobs[jobID]
	if !ok {
		m.jobsMu.RUnlock()
		return fmt.Errorf("job %s not found", jobID)
	}
	status := *job
	sourceRoot := job.StagingRoot
	sourceToken := job.VerificationToken
	rels := append([]string(nil), job.StagedArtifacts...)
	m.jobsMu.RUnlock()
	if (!m.objectQuarantineEnabled() && sourceRoot == "") ||
		!capabilityTokenRegex.MatchString(sourceToken) || len(rels) == 0 {
		return fmt.Errorf("job quarantine is incomplete")
	}
	if m.objectQuarantineEnabled() {
		materialized, materializeErr := m.ensureObjectJobQuarantine(jobID)
		if materializeErr != nil {
			return fmt.Errorf("materialize signing inputs: %w", materializeErr)
		}
		sourceRoot = materialized
	}

	records, err := artifactMetadata(sourceRoot, rels, &status, "verified_unsigned")
	if err != nil {
		return err
	}
	inputs := make([]signing.Artifact, len(records))
	for index := range records {
		records[index].Location = "quarantine:" + sourceToken + "/" + filepath.ToSlash(rels[index])
		inputs[index] = signing.Artifact{
			RelativePath: filepath.ToSlash(rels[index]),
			InputDigest:  records[index].Digest,
			InputSize:    records[index].SizeBytes,
		}
	}
	if m.metadata == nil {
		return fmt.Errorf("isolated signing requires durable artifact metadata")
	}
	maxOutputBytes, err := m.artifactBudgetRemaining(jobID)
	if err != nil {
		return fmt.Errorf("reserve signed artifact generation: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = m.metadata.RecordArtifacts(ctx, &status, records)
	cancel()
	if err != nil {
		return fmt.Errorf("persist verified unsigned artifact metadata: %w", err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	task, err := m.signing.EnqueueSigning(ctx, signing.Request{
		JobID: status.JobID, AttemptID: status.AttemptID,
		AttemptFence: status.FenceToken, LeaseOwner: status.LeaseOwner,
		SourceToken: sourceToken, Architecture: status.Arch,
		MaxOutputBytes: maxOutputBytes, Artifacts: inputs,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("enqueue signing task: %w", err)
	}
	if task == nil || task.ID == "" {
		return fmt.Errorf("signing queue returned an empty task")
	}

	waitTimeout := time.Duration(m.config.SignerWaitTimeoutSeconds) * time.Second
	if waitTimeout <= 0 {
		waitTimeout = 10 * time.Minute
	}
	return m.waitForSigningTask(jobID, sourceRoot, sourceToken, task, waitTimeout)
}

func (m *Manager) waitForSigningTask(
	jobID, sourceRoot, sourceToken string,
	task *signing.Task,
	waitTimeout time.Duration,
) error {
	deadline := time.Now().Add(waitTimeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("signing task %s did not finish within %s", task.ID, waitTimeout)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		current, getErr := m.signing.GetSigningTask(ctx, task.ID)
		cancel()
		if getErr != nil {
			return getErr
		}
		switch current.State {
		case "completed":
			return m.adoptSignedArtifacts(jobID, sourceRoot, sourceToken, current)
		case "failed", "canceled":
			if current.Error == "" {
				current.Error = "signing task did not complete"
			}
			return fmt.Errorf("%s", current.Error)
		}
		select {
		case <-m.stopCh:
			return fmt.Errorf("control plane stopped while waiting for signer")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (m *Manager) adoptSignedArtifacts(
	jobID, sourceRoot, sourceToken string,
	task *signing.Task,
) error {
	outputToken, err := signing.OutputToken(task.ID)
	if err != nil {
		return err
	}
	if len(task.Artifacts) == 0 {
		return fmt.Errorf("signer returned no artifacts")
	}
	rels := make([]string, len(task.Artifacts))
	for index, artifact := range task.Artifacts {
		if err := signing.ValidateArtifact(artifact, true); err != nil {
			return err
		}
		rels[index] = artifact.RelativePath
	}
	outputRoot := ""
	if m.objectQuarantineEnabled() {
		outputRoot, err = m.materializeObjectQuarantine(outputToken, rels)
	} else {
		outputRoot, err = signing.TokenRoot(m.config.BinpkgPath, outputToken)
	}
	if err != nil {
		return err
	}
	for _, artifact := range task.Artifacts {
		records, err := artifactMetadata(outputRoot, []string{artifact.RelativePath}, &BuildStatus{}, "signed")
		if err != nil {
			return err
		}
		if records[0].Digest != artifact.OutputDigest || records[0].SizeBytes != artifact.OutputSize {
			return fmt.Errorf("signed artifact %q does not match signer output digest", artifact.RelativePath)
		}
	}
	budget, err := m.setArtifactGeneration(jobID, "signed", outputRoot, rels)
	if err != nil {
		_ = os.RemoveAll(outputRoot)
		return fmt.Errorf("account signed artifact generation: %w", err)
	}
	if err := m.releaseArtifactGeneration(
		jobID, "collected", "replaced_by_signed_generation",
	); err != nil {
		_ = os.RemoveAll(outputRoot)
		return fmt.Errorf("release unsigned artifact generation: %w", err)
	}
	m.appendJobLog(jobID, fmt.Sprintf(
		"[sign] signed quarantine generation budget=%d/%d peak=%d",
		budget.ActiveBytes, budget.LimitBytes, budget.PeakBytes,
	))
	signingKeyID := strings.TrimSpace(task.SigningKeyID)
	if signingKeyID == "" {
		return fmt.Errorf("signer completed task %s without reporting its key ID", task.ID)
	}
	m.jobsMu.Lock()
	job, ok := m.jobs[jobID]
	if ok {
		job.StagingRoot = outputRoot
		job.VerificationToken = outputToken
		job.StagedArtifacts = rels
		job.Signed = true
		job.SigningKeyID = signingKeyID
	}
	m.jobsMu.Unlock()
	if !ok {
		return fmt.Errorf("job %s disappeared while adopting signer output", jobID)
	}
	if err := m.persistJobSnapshot(jobID); err != nil {
		return fmt.Errorf("persist adopted signed generation: %w", err)
	}
	if sourceRoot != outputRoot && m.config.PhaseExecutorMode != "active" {
		_ = os.RemoveAll(sourceRoot)
	}
	if m.objectQuarantineEnabled() && sourceToken != outputToken {
		if err := m.deleteObjectQuarantine(sourceToken); err != nil {
			return fmt.Errorf("delete unsigned quarantine generation: %w", err)
		}
	}
	return nil
}

func (m *Manager) requireActiveClaim(jobID string) error {
	if m.scheduler == nil {
		return nil
	}
	m.jobsMu.RLock()
	status, ok := m.jobs[jobID]
	if ok {
		copyStatus := *status
		status = &copyStatus
	}
	m.jobsMu.RUnlock()
	if !ok {
		return fmt.Errorf("job is missing from the local execution projection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease := 30 * time.Second
	if m.config.PhaseExecutorMode == "active" {
		lease = phaseAttemptLease
	}
	if err := m.scheduler.RenewClaim(ctx, status, lease); err != nil {
		return err
	}
	return m.scheduler.CheckClaim(ctx, status)
}

// verifyOnBuilder asks a builder to install the just-built package from the
// job-private verification binhost in a throwaway native Portage root.
func (m *Manager) verifyOnBuilder(jobID, builderEndpoint, instanceID string, req *BuildRequest, binhostURL string) error {
	return m.verifyOnBuilderStable(
		jobID, builderEndpoint, instanceID, req, binhostURL, "",
	)
}

func (m *Manager) verifyOnBuilderStable(
	jobID, builderEndpoint, instanceID string,
	req *BuildRequest,
	binhostURL, commandID string,
) error {
	return m.verifyOnBuilderStableFenced(
		jobID, builderEndpoint, instanceID, req, binhostURL, commandID, nil,
	)
}

func (m *Manager) verifyOnBuilderStableFenced(
	jobID, builderEndpoint, instanceID string,
	req *BuildRequest,
	binhostURL, commandID string,
	resultFence func() error,
) error {
	if !m.updateStatus(jobID, "verifying", instanceID, "") {
		return fmt.Errorf("durable verify phase transition was rejected")
	}

	signed, stagingRoot, rels, builtPackages, err :=
		m.verificationGeneration(jobID)
	if err != nil {
		return err
	}

	generation := "unsigned"
	if signed {
		generation = "signed"
	}
	artifacts, err := verificationArtifacts(
		stagingRoot, rels, "verifying_"+generation,
	)
	if err != nil {
		return fmt.Errorf("bind %s verification generation: %w", generation, err)
	}

	keyID, pubkey := "", ""
	if signed {
		var public []byte
		keyID, public, err = m.verificationPublicKey(
			"signed verification", "refusing unsigned downgrade",
		)
		if err != nil {
			return err
		}
		pubkey = string(public)
	}
	generationLog := fmt.Sprintf(
		"[verify] generation=%s signature_required=%t key_id=%s artifacts=%d digests=%s",
		generation, signed, keyID, len(artifacts), verificationDigestSummary(artifacts))
	installLog := fmt.Sprintf(
		"[verify] installing %s from the digest-bound %s job-private generation in a fresh PKGDIR and throwaway native root…",
		req.PackageName, generation)
	if resultFence == nil {
		m.appendJobLog(jobID, generationLog)
		m.appendJobLog(jobID, installLog)
	}

	verifyRequest := VerifyInstallRequest{
		PackageName:      req.PackageName,
		BinhostURL:       binhostURL,
		Generation:       generation,
		GPGPubkey:        pubkey,
		ExpectedKeyID:    keyID,
		BuiltPackages:    builtPackages,
		Artifacts:        artifacts,
		RequireSignature: signed,
	}
	if strings.HasPrefix(builderEndpoint, "pull://") {
		result, err := m.dispatchPullVerify(
			jobID, builderEndpoint, verifyRequest, commandID,
		)
		if err != nil {
			return err
		}
		return m.acceptVerificationResult(
			jobID, result, generationLog, installLog, resultFence,
		)
	}
	baseURL := normalizeBuilderURL(builderEndpoint)
	body, _ := json.Marshal(verifyRequest)
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/verify", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setBuilderAuth(httpReq, m.config.BuilderToken)

	resp, err := artifactHTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("verification request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result pullVerifyResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("verification response invalid (status %d): %w", resp.StatusCode, err)
	}
	return m.acceptVerificationResult(
		jobID, &result, generationLog, installLog, resultFence,
	)
}

func (m *Manager) verificationGeneration(
	jobID string,
) (bool, string, []string, []string, error) {
	m.jobsMu.RLock()
	defer m.jobsMu.RUnlock()
	job, ok := m.jobs[jobID]
	if !ok {
		return false, "", nil, nil, fmt.Errorf("job %s not found", jobID)
	}
	rels := append([]string(nil), job.StagedArtifacts...)
	if job.StagingRoot == "" || len(rels) == 0 {
		return false, "", nil, nil,
			fmt.Errorf("verification generation is missing staged artifacts")
	}
	return job.Signed, job.StagingRoot, rels, artifactCPVs(rels), nil
}

func artifactCPVs(rels []string) []string {
	var packages []string
	for _, rel := range rels {
		if cpv := artifactRelCPV(rel); cpv != "" &&
			!slices.Contains(packages, cpv) {
			packages = append(packages, cpv)
		}
	}
	return packages
}

func verificationArtifacts(
	root string,
	rels []string,
	stage string,
) ([]VerificationArtifact, error) {
	records, err := artifactMetadata(root, rels, &BuildStatus{}, stage)
	if err != nil {
		return nil, err
	}
	artifacts := make([]VerificationArtifact, len(records))
	for index := range records {
		artifacts[index] = VerificationArtifact{
			RelativePath: filepath.ToSlash(rels[index]),
			SHA256:       records[index].Digest,
			Size:         records[index].SizeBytes,
		}
	}
	return artifacts, nil
}

func (m *Manager) verificationPublicKey(
	purpose, suffix string,
) (string, []byte, error) {
	if m.gpgKeyProvider == nil {
		return "", nil, fmt.Errorf(
			"%s requires an isolated-signer public-key provider; %s",
			purpose, suffix,
		)
	}
	keyID, public, _ := m.gpgKeyProvider()
	if keyID == "" || len(public) == 0 {
		return "", nil, fmt.Errorf(
			"%s requires a ready signer public key; %s",
			purpose, suffix,
		)
	}
	return keyID, public, nil
}

func (m *Manager) acceptVerificationResult(
	jobID string,
	result *pullVerifyResult,
	generationLog, installLog string,
	resultFence func() error,
) error {
	if resultFence != nil {
		if err := resultFence(); err != nil {
			return fmt.Errorf("verification result phase fence rejected: %w", err)
		}
		m.appendJobLog(jobID, generationLog)
		m.appendJobLog(jobID, installLog)
	}
	if result.Log != "" {
		m.appendJobLog(jobID, "[verify] "+strings.ReplaceAll(strings.TrimSpace(result.Log), "\n", "\n[verify] "))
	}
	if !result.OK {
		return fmt.Errorf("%s", result.Error)
	}
	m.appendJobLog(jobID, "[verify] install verification passed")
	return nil
}

type pullVerifyResult struct {
	OK    bool   `json:"ok"`
	Log   string `json:"log"`
	Error string `json:"error"`
}

func (m *Manager) dispatchPullVerify(
	jobID, endpoint string,
	request VerifyInstallRequest,
	commandID string,
) (*pullVerifyResult, error) {
	identity, err := m.pullIdentityFor(jobID, endpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	var result pullVerifyResult
	var dispatchErr error
	if commandID == "" {
		dispatchErr = m.workerBroker.Dispatch(
			ctx, identity, workergateway.ActionVerify, request, &result,
		)
	} else {
		dispatchErr = m.workerBroker.DispatchID(
			ctx, identity, commandID, workergateway.ActionVerify, request, &result,
		)
	}
	if dispatchErr != nil {
		return nil, fmt.Errorf("outbound verification request failed: %w", dispatchErr)
	}
	return &result, nil
}

func (m *Manager) pullIdentityFor(jobID, endpoint string) (workergateway.Identity, error) {
	workerID := strings.TrimPrefix(endpoint, "pull://")
	if workerID == endpoint || workerID == "" || strings.Contains(workerID, "/") {
		return workergateway.Identity{}, fmt.Errorf("invalid outbound worker endpoint")
	}
	status, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return workergateway.Identity{}, err
	}
	identity := workergateway.Identity{
		WorkerID: workerID, JobID: jobID,
		AttemptID: status.AttemptID, FenceToken: status.FenceToken,
	}
	return identity, identity.Validate()
}

// verifyUnsignedRejectedOnBuilder is a mandatory negative control for signed
// pipelines. It submits the exact unsigned generation under the signed policy
// and requires the builder's independent GPKG verifier to reject it before the
// isolated signer is allowed to run.
func (m *Manager) verifyUnsignedRejectedOnBuilder(jobID, builderEndpoint string, req *BuildRequest, binhostURL string) error {
	return m.verifyUnsignedRejectedOnBuilderStable(
		jobID, builderEndpoint, req, binhostURL, "",
	)
}

func (m *Manager) verifyUnsignedRejectedOnBuilderStable(
	jobID, builderEndpoint string,
	req *BuildRequest,
	binhostURL, commandID string,
) error {
	return m.verifyUnsignedRejectedOnBuilderStableFenced(
		jobID, builderEndpoint, req, binhostURL, commandID, nil,
	)
}

func (m *Manager) verifyUnsignedRejectedOnBuilderStableFenced(
	jobID, builderEndpoint string,
	req *BuildRequest,
	binhostURL, commandID string,
	resultFence func() error,
) error {
	signed, root, rels, builtPackages, err := m.verificationGeneration(jobID)
	if err != nil {
		return err
	}
	if signed {
		return fmt.Errorf("negative control requires an unsigned generation")
	}
	keyID, public, err := m.verificationPublicKey(
		"negative control", "signed-policy proof is unavailable",
	)
	if err != nil {
		return err
	}
	artifacts, err := verificationArtifacts(root, rels, "negative_control")
	if err != nil {
		return err
	}
	verifyRequest := VerifyInstallRequest{
		PackageName: req.PackageName, BinhostURL: binhostURL,
		Generation: "signed", GPGPubkey: string(public), ExpectedKeyID: keyID,
		BuiltPackages: builtPackages, Artifacts: artifacts, RequireSignature: true,
	}
	if strings.HasPrefix(builderEndpoint, "pull://") {
		result, err := m.dispatchPullVerify(
			jobID, builderEndpoint, verifyRequest, commandID,
		)
		if err != nil {
			return err
		}
		if resultFence != nil {
			if err := resultFence(); err != nil {
				return fmt.Errorf("negative-control result phase fence rejected: %w", err)
			}
		}
		if result.OK {
			return fmt.Errorf("unsigned GPKG was accepted under the signed verification policy")
		}
		if result.Error == "" {
			return fmt.Errorf("builder rejected the negative control without an auditable reason")
		}
		m.appendJobLog(jobID, fmt.Sprintf(
			"[verify-negative] unsigned generation rejected under signer key %s before signing (artifacts=%d digests=%s)",
			keyID, len(artifacts), verificationDigestSummary(artifacts)))
		return nil
	}
	body, _ := json.Marshal(verifyRequest)
	httpReq, err := http.NewRequest(http.MethodPost,
		normalizeBuilderURL(builderEndpoint)+"/api/v1/verify", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setBuilderAuth(httpReq, m.config.BuilderToken)
	response, err := artifactHTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("negative-control request failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return fmt.Errorf("negative-control response invalid (status %d): %w", response.StatusCode, err)
	}
	if result.OK {
		return fmt.Errorf("unsigned GPKG was accepted under the signed verification policy")
	}
	if result.Error == "" {
		return fmt.Errorf("builder rejected the negative control without an auditable reason")
	}
	m.appendJobLog(jobID, fmt.Sprintf(
		"[verify-negative] unsigned generation rejected under signer key %s before signing (artifacts=%d digests=%s)",
		keyID, len(artifacts), verificationDigestSummary(artifacts)))
	return nil
}

// uploadJobToMirror pushes every stored artifact of a job (plus the Packages
// index and the signing pubkey) to the configured mirror.
func (m *Manager) uploadJobToMirror(jobID string, up *mirrorUploader) error {
	m.jobsMu.RLock()
	var locals, rels []string
	var status *BuildStatus
	if job, ok := m.jobs[jobID]; ok {
		snapshot := *job
		status = &snapshot
		if len(job.ArtifactPaths) == len(job.Artifacts) {
			locals = append(locals, job.ArtifactPaths...)
			for _, w := range job.Artifacts {
				rels = append(rels, strings.TrimPrefix(w, "/binpkgs/"))
			}
		}
		if len(locals) == 0 && job.ArtifactPath != "" {
			locals = append(locals, job.ArtifactPath)
			rels = append(rels, strings.TrimPrefix(job.ArtifactURL, "/binpkgs/"))
		}
	}
	m.jobsMu.RUnlock()
	if len(locals) == 0 {
		return fmt.Errorf("no artifacts recorded for upload")
	}
	binhostPath, err := buildStatusBinhostPath(status)
	if err != nil {
		return err
	}
	if err := up.login(); err != nil {
		return err
	}
	for i, local := range locals {
		uploadSource := local
		cleanupSource := ""
		if m.objectQuarantineEnabled() {
			temp, tempErr := os.CreateTemp(
				m.artifactQuarantineBase(), ".mirror-artifact-*",
			)
			if tempErr != nil {
				return tempErr
			}
			cleanupSource = temp.Name()
			if closeErr := temp.Close(); closeErr != nil {
				_ = os.Remove(cleanupSource)
				return closeErr
			}
			if downloadErr := m.artifactStore.Download(local, cleanupSource); downloadErr != nil {
				_ = os.Remove(cleanupSource)
				return fmt.Errorf("materialize mirror artifact: %w", downloadErr)
			}
			uploadSource = cleanupSource
		}
		sub := ""
		if j := strings.LastIndex(rels[i], "/"); j >= 0 {
			sub = rels[i][:j]
		}
		url, err := up.uploadLocalFile(uploadSource, sub)
		if cleanupSource != "" {
			_ = os.Remove(cleanupSource)
		}
		if err != nil {
			return fmt.Errorf("upload %s: %w", rels[i], err)
		}
		m.appendJobLog(jobID, "[collect] uploaded to mirror: "+url)
	}
	if m.objectQuarantineEnabled() {
		channelKey, keyErr := artifactstorage.StableChannelKey(binhostPath, status.Arch)
		if keyErr != nil {
			return keyErr
		}
		pointerDocument, downloadErr := artifactstorage.DownloadBytes(
			m.artifactStore, channelKey, m.artifactQuarantineBase(), 1<<20,
		)
		if downloadErr != nil {
			return downloadErr
		}
		pointer, parseErr := artifactstorage.ParseChannelPointer(pointerDocument)
		if parseErr != nil {
			return parseErr
		}
		packagesKey, keyErr := artifactstorage.PublishedGenerationKey(
			binhostPath, status.Arch, pointer.GenerationID, "Packages",
		)
		if keyErr != nil {
			return keyErr
		}
		temp, tempErr := os.CreateTemp(
			m.artifactQuarantineBase(), "mirror-"+pointer.GenerationID+"-Packages-*",
		)
		if tempErr != nil {
			return tempErr
		}
		packagesFile := temp.Name()
		if closeErr := temp.Close(); closeErr != nil {
			_ = os.Remove(packagesFile)
			return closeErr
		}
		if downloadErr := m.artifactStore.Download(packagesKey, packagesFile); downloadErr != nil {
			return downloadErr
		}
		_, uploadErr := up.uploadLocalFile(packagesFile, binhostPath)
		_ = os.Remove(packagesFile)
		if uploadErr != nil {
			return fmt.Errorf("upload Packages index: %w", uploadErr)
		}
		m.appendJobLog(jobID,
			"[collect] uploaded to mirror: "+up.binhostURL()+"/"+binhostPath+"/Packages")
	} else if m.config.BinpkgPath != "" {
		idx := filepath.Join(m.config.BinpkgPath, filepath.FromSlash(binhostPath), "Packages")
		if _, err := os.Stat(idx); err == nil {
			if _, err := up.uploadLocalFile(idx, binhostPath); err != nil {
				return fmt.Errorf("upload Packages index: %w", err)
			}
			m.appendJobLog(jobID, "[collect] uploaded to mirror: "+up.binhostURL()+"/"+binhostPath+"/Packages")
		}
	}
	if m.gpgKeyProvider != nil {
		if keyID, pub, _ := m.gpgKeyProvider(); keyID != "" && len(pub) > 0 {
			if _, err := up.uploadBytes("portage-engine.asc", pub, ""); err != nil {
				m.appendJobLog(jobID, "[collect] warning: pubkey upload failed: "+err.Error())
			} else {
				m.appendJobLog(jobID, "[collect] signing pubkey uploaded: "+up.binhostURL()+"/portage-engine.asc")
			}
		}
	}
	return nil
}

// buildProvisionRequest assembles a ProvisionRequest from server config, or
// returns an error explaining what is missing (so the caller can fail loudly
// instead of provisioning a VM that can never build anything).
func (m *Manager) buildProvisionRequest(req *BuildRequest) (*iac.ProvisionRequest, error) {
	provider := req.CloudProvider
	cs := m.CloudSettings()
	if provider == "" {
		provider = cs.Provider
	}
	if err := m.validateCloudProvisionConfig(provider, cs); err != nil {
		return nil, err
	}

	ttl := time.Duration(cs.InstanceTTLMinutes) * time.Minute
	binhost, err := cloudBuildBinhost(cs, req)
	if err != nil {
		return nil, err
	}

	spec, buildMode, err := cloudBuildSpec(provider, cs, req)
	if err != nil {
		return nil, err
	}
	if req.ResolvedContext != nil {
		if err := m.bindResolvedProvisionContext(
			provider, cs, req.ResolvedContext, spec, binhost,
		); err != nil {
			return nil, err
		}
	}

	return &iac.ProvisionRequest{
		Provider:    provider,
		Arch:        req.Arch,
		Spec:        spec,
		Credentials: m.cloudCredentials(cs),
		SSH: &iac.SSHConfig{
			KeyPath:         cs.SSHKeyPath,
			User:            cs.SSHUser,
			KnownHostsPath:  cs.SSHKnownHosts,
			InsecureHostKey: cs.SSHInsecureHostKey,
		},
		ServerCallback:      cs.ServerCallbackURL,
		BuilderPort:         9090,
		BuilderToken:        m.config.BuilderToken,
		BinpkgHost:          binhost,
		EgressPolicy:        resolvedEgressPolicy(req.ResolvedContext),
		TTL:                 ttl,
		BuilderBinaryPath:   cs.BuilderBinaryPath,
		BuilderBinaryURL:    cs.BuilderBinaryURL,
		BuilderBinarySHA256: cs.BuilderBinarySHA256,

		GentooMirror:      cs.GentooMirror,
		PortageSyncURI:    cs.PortageSyncURI,
		PortageSyncMethod: cs.PortageSyncMethod,
		MakeConfExtra:     cs.MakeConfExtra,
		BuildFeatures:     cs.BuildFeatures,
		BuildMode:         buildMode,
	}, nil
}

func (m *Manager) validateCloudProvisionConfig(
	provider string,
	settings *config.CloudSettings,
) error {
	if provider == "" {
		return fmt.Errorf("no remote builders configured and no cloud provider set (set REMOTE_BUILDERS or CLOUD_DEFAULT_PROVIDER)")
	}
	if settings.SSHKeyPath == "" {
		return fmt.Errorf("cloud build requires CLOUD_SSH_KEY_PATH so the builder can be deployed to the instance")
	}
	if settings.ServerCallbackURL == "" {
		return fmt.Errorf("cloud build requires SERVER_CALLBACK_URL so the deployed builder can reach this server")
	}
	if m.config.WorkerGatewayEnabled {
		if err := validateHTTPSOrigin(
			m.config.WorkerGatewayAdvertiseURL,
			"WORKER_GATEWAY_ADVERTISE_URL",
		); err != nil {
			return err
		}
	}
	if settings.BuilderBinaryPath == "" && settings.BuilderBinaryURL == "" {
		fmt.Println("Warning: neither CLOUD_BUILDER_BINARY_PATH nor CLOUD_BUILDER_BINARY_URL is set; " +
			"the instance can only build if its image ships /opt/portage-builder/portage-builder")
	}
	if settings.BuilderBinaryPath != "" || settings.BuilderBinaryURL == "" {
		return nil
	}
	builderURL, err := neturl.Parse(settings.BuilderBinaryURL)
	if err != nil || (builderURL.Scheme != "http" &&
		builderURL.Scheme != "https") || builderURL.Host == "" {
		return fmt.Errorf("CLOUD_BUILDER_BINARY_URL must be an absolute http or https URL")
	}
	if builderURL.User != nil || builderURL.RawQuery != "" ||
		builderURL.Fragment != "" {
		return fmt.Errorf("CLOUD_BUILDER_BINARY_URL must not contain credentials, query parameters, or a fragment")
	}
	if !sha256DigestRegex.MatchString(settings.BuilderBinarySHA256) {
		return fmt.Errorf("CLOUD_BUILDER_BINARY_SHA256 must be exactly 64 lowercase hexadecimal characters when downloading the builder")
	}
	return nil
}

func validateHTTPSOrigin(rawURL, name string) error {
	parsed, err := neturl.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("%s must be an HTTPS origin without credentials, path, query, or fragment", name)
	}
	return nil
}

func cloudBuildBinhost(
	settings *config.CloudSettings,
	req *BuildRequest,
) (string, error) {
	binhostPath, err := buildBinhostPath(req)
	if err != nil {
		return "", err
	}
	binhost := strings.TrimRight(settings.ServerCallbackURL, "/") +
		"/binpkgs/" + binhostPath
	if settings.UploadURL == "" {
		return binhost, nil
	}
	dir := strings.Trim(settings.UploadDir, "/")
	if dir == "" {
		dir = "portage-engine"
	}
	return strings.TrimRight(settings.UploadURL, "/") +
		"/local/" + dir + "/" + binhostPath, nil
}

func cloudBuildSpec(
	provider string,
	settings *config.CloudSettings,
	req *BuildRequest,
) (map[string]string, string, error) {
	buildMode := "native-gentoo"
	if req.ResolvedContext != nil && req.ResolvedContext.BuildMode != "" {
		buildMode = req.ResolvedContext.BuildMode
	}
	if buildMode != "native-gentoo" {
		return nil, "", fmt.Errorf(
			"build mode %q is no longer supported; only native-gentoo is available",
			buildMode,
		)
	}
	spec := req.MachineSpec
	switch provider {
	case "pve":
		spec = pveSpecWithDefaults(settings, req.MachineSpec, buildMode)
		if req.ResolvedContext != nil && req.ResolvedContext.Template != "" {
			spec["template"] = req.ResolvedContext.Template
		}
	case "gcp":
		spec = gcpSpecWithDefaults(settings, req.MachineSpec)
	case "aws":
		spec = awsSpecWithDefaults(settings, req.MachineSpec)
	}
	return spec, buildMode, nil
}

func (m *Manager) bindResolvedProvisionContext(
	provider string,
	settings *config.CloudSettings,
	resolved *catalog.ResolvedBuildContext,
	spec map[string]string,
	binhost string,
) error {
	spec["pe_catalog_version"] = strconv.Itoa(resolved.CatalogVersion)
	spec["pe_profile_id"] = resolved.ProfileID
	spec["pe_image_id"] = resolved.ImageID
	spec["pe_image_generation"] = resolved.ImageGeneration
	spec["pe_image_digest"] = resolved.ImageDigest
	spec["pe_mirror_bundle_id"] = resolved.MirrorBundleID
	spec["pe_mirror_bundle_digest"] = resolved.MirrorBundleDigest
	spec["pe_egress_policy_id"] = resolved.EgressPolicy.ID
	spec["pe_egress_policy_digest"] = resolved.EgressPolicyDigest
	builderBinaryEndpoint := settings.BuilderBinaryURL
	if settings.BuilderBinaryPath != "" {
		builderBinaryEndpoint = ""
	}
	if err := validateResolvedEgress(provider, resolved, map[string]string{
		"server callback": settings.ServerCallbackURL,
		"worker gateway":  m.config.WorkerGatewayAdvertiseURL,
		"binhost":         binhost,
		"builder binary":  builderBinaryEndpoint,
		"Gentoo mirror":   settings.GentooMirror,
		"Portage sync":    settings.PortageSyncURI,
	}); err != nil {
		return err
	}
	if resolved.EgressPolicy.Mode == catalog.EgressModeEnforce {
		spec["start_stopped"] = "true"
	}
	return nil
}

func resolvedEgressPolicy(context *catalog.ResolvedBuildContext) *catalog.EgressPolicy {
	if context == nil {
		return nil
	}
	policy := context.EgressPolicy
	return &policy
}

func validateResolvedEgress(provider string, context *catalog.ResolvedBuildContext, endpoints map[string]string) error {
	if context.EgressPolicy.Mode == catalog.EgressModeDisabled {
		return nil
	}
	if context.EgressPolicy.Mode != catalog.EgressModeEnforce {
		return fmt.Errorf("resolved egress policy %q has unsupported mode %q", context.EgressPolicy.ID, context.EgressPolicy.Mode)
	}
	if provider != "pve" {
		return fmt.Errorf("enforced egress policy %q is not implemented for provider %q", context.EgressPolicy.ID, provider)
	}
	digest, err := context.EgressPolicy.Digest()
	if err != nil {
		return fmt.Errorf("resolved egress policy %q is invalid: %w", context.EgressPolicy.ID, err)
	}
	if digest != context.EgressPolicyDigest {
		return fmt.Errorf("resolved egress policy %q digest changed after catalog resolution", context.EgressPolicy.ID)
	}
	for purpose, endpoint := range endpoints {
		if strings.TrimSpace(endpoint) == "" {
			continue
		}
		if err := context.EgressPolicy.CoversURL(endpoint, purpose); err != nil {
			return err
		}
	}
	return nil
}

// CloudSettings returns the current cloud provisioning settings snapshot.
// Safe for concurrent use; treat the result as read-only. Falls back to
// deriving the snapshot from the static config for Managers constructed
// without NewManager (tests).
func (m *Manager) CloudSettings() *config.CloudSettings {
	if cs := m.cloudSettings.Load(); cs != nil {
		return cs
	}
	cs := config.CloudSettingsFromServerConfig(m.config)
	m.cloudSettings.Store(cs)
	return cs
}

// SetBuildCatalog atomically replaces the immutable server-owned catalog used
// for subsequent submissions. In-flight jobs retain their resolved context.
func (m *Manager) SetBuildCatalog(c *catalog.Catalog) {
	if c != nil {
		m.buildCatalog.Store(c)
	}
}

// BuildCatalog returns the currently active catalog.
func (m *Manager) BuildCatalog() *catalog.Catalog {
	return m.buildCatalog.Load()
}

// resolveBuildRequest converts client identifiers and legacy metadata into an
// authoritative catalog context before a job ID or infrastructure is created.
func (m *Manager) resolveBuildRequest(req *BuildRequest) error {
	c := m.BuildCatalog()
	if c == nil {
		return fmt.Errorf("build catalog is unavailable")
	}

	profileID := strings.TrimSpace(req.ProfileID)
	legacyProfile := ""
	repositoryIDs := append([]string(nil), req.RepositoryIDs...)
	if req.ConfigBundle != nil {
		if profileID == "" {
			profileID = strings.TrimSpace(req.ConfigBundle.Metadata.ProfileID)
		}
		legacyProfile = strings.TrimSpace(req.ConfigBundle.Metadata.Profile)
		if req.ConfigBundle.Config != nil {
			for _, repo := range req.ConfigBundle.Config.Repos {
				id := strings.TrimSpace(repo.RegistryID)
				if id == "" {
					id = strings.TrimSpace(repo.Name)
				}
				if id != "" {
					repositoryIDs = append(repositoryIDs, id)
				}
			}
		}
	}

	resolved, err := c.Resolve(catalog.ResolveRequest{
		ProfileID: profileID, LegacyProfile: legacyProfile, Arch: req.Arch,
		RepositoryIDs: repositoryIDs, ResourceClass: req.ResourceClass,
		CloudProvider: req.CloudProvider,
	})
	if err != nil {
		return err
	}

	// A compatibility image explicitly inherits the dashboard-managed current
	// template. Candidate/stable catalogs must name their own immutable one.
	if resolved.Template == "" && resolved.ImageChannel == "compatibility" {
		resolved.Template = m.CloudSettings().PVETemplate
	}
	if len(req.MachineSpec) > 0 && resolved.ProfileChannel != "compatibility" {
		return fmt.Errorf("machine_spec overrides are disabled for catalog profiles; select a resource_class")
	}
	for key, value := range req.MachineSpec {
		resolved.MachineSpec[key] = value
	}

	req.ProfileID = resolved.ProfileID
	req.RepositoryIDs = make([]string, 0, len(resolved.Repositories))
	for _, repo := range resolved.Repositories {
		req.RepositoryIDs = append(req.RepositoryIDs, repo.ID)
	}
	req.ResourceClass = resolved.ResourceClass
	req.Arch = resolved.Arch
	req.CloudProvider = resolved.Provider
	req.MachineSpec = maps.Clone(resolved.MachineSpec)
	req.ResolvedContext = resolved

	// Every execution path receives the same resolved bundle. This is important
	// for the simple request endpoint: without a bundle, a remote builder would
	// otherwise recreate default repos and lose the catalog revision pin.
	if req.ConfigBundle == nil {
		req.ConfigBundle = &ConfigBundle{
			Config: &PortageConfig{},
			Packages: &BuildPackageSpec{Packages: []PackageSpec{{
				Atom: req.PackageName, Version: req.Version,
				UseFlags: append([]string(nil), req.UseFlags...),
			}}},
			Metadata: BundleMetadata{CreatedAt: time.Now().UTC().Format(time.RFC3339)},
		}
	}
	req.ConfigBundle.Metadata.ProfileID = resolved.ProfileID
	req.ConfigBundle.Metadata.Profile = resolved.ProfilePath
	req.ConfigBundle.Metadata.ProfileRepositoryID = resolved.ProfileRepositoryID
	req.ConfigBundle.Metadata.ProfileRepositoryName = resolved.ProfileRepositoryName
	req.ConfigBundle.Metadata.ProfileParents = make([]ProfileParentRef, 0, len(resolved.ProfileParents))
	for _, parent := range resolved.ProfileParents {
		req.ConfigBundle.Metadata.ProfileParents = append(req.ConfigBundle.Metadata.ProfileParents, ProfileParentRef{
			RepositoryID: parent.RepositoryID, RepositoryName: parent.RepositoryName, ProfilePath: parent.ProfilePath,
		})
	}
	req.ConfigBundle.Metadata.TargetArch = resolved.Arch
	req.ConfigBundle.Metadata.RequiredFeatures = append(
		[]string(nil), resolved.RequiredFeatures...,
	)
	if req.ConfigBundle.Config != nil {
		repos := make([]RepoConfig, 0, len(resolved.Repositories))
		for _, repo := range resolved.Repositories {
			repos = append(repos, RepoConfig{
				RegistryID: repo.ID, Name: repo.Name, Location: repo.Location,
				SyncType: repo.SyncType, SyncURI: repo.SyncURI,
				Revision: repo.Revision, Digest: repo.Digest, Priority: repo.Priority,
			})
		}
		req.ConfigBundle.Config.Repos = repos
	}
	if err := validateBundle(req.ConfigBundle); err != nil {
		return fmt.Errorf("resolved configuration is invalid: %w", err)
	}
	return nil
}

// UpdateCloudSettings atomically replaces the cloud provisioning settings.
// In-flight builds keep the snapshot they started with; subsequent builds use
// the new values. Used by the server's settings API (dashboard-managed config).
func (m *Manager) UpdateCloudSettings(s *config.CloudSettings) {
	updated := s.Clone()
	updated.SkipVerifyInstall = false
	updated.BuildMode = "native-gentoo"
	m.cloudSettings.Store(updated)
}

// remoteBuilders returns the current static builder list (runtime-adjustable
// via the settings API).
func (m *Manager) remoteBuilders() []string {
	return m.CloudSettings().RemoteBuilders
}

// pveSpecWithDefaults merges the runtime PVE settings into the per-request
// machine spec (request values win), so a plain build request can provision on
// PVE without the client re-sending endpoint/template/storage every time.
// Without this merge the PVE settings (other than the token credentials) would
// never reach the IaC layer.
func pveSpecWithDefaults(cs *config.CloudSettings, reqSpec map[string]string, buildMode string) map[string]string {
	spec := make(map[string]string, len(reqSpec)+6)
	set := func(key, value string) {
		if value != "" {
			spec[key] = value
		}
	}
	set("endpoint", cs.PVEEndpoint)
	set("node", cs.PVENode)
	set("nodes", strings.Join(cs.PVENodes, ","))
	set("storage", cs.PVEStorage)
	set("network", cs.PVENetwork)
	set("template", cs.PVETemplate)
	set("cicustom", cs.PVECICustom)
	set("nameserver", cs.PVENameserver)
	set("ip_config", cs.PVEIPConfig)
	set("gateway", cs.PVEGateway)
	if cs.PVEInsecure {
		spec["insecure"] = "true"
	}
	// A native Gentoo template is a UEFI (ovmf/q35) image; the boot disk clones
	// onto scsi0. Set these unless the request overrides them.
	if buildMode == "native-gentoo" {
		set("bios", "ovmf")
		set("machine", "q35")
		set("disk_type", "scsi")
	}
	maps.Copy(spec, reqSpec)
	return spec
}

// gcpSpecWithDefaults merges the runtime GCP settings into the per-request
// machine spec (request values win).
func gcpSpecWithDefaults(cs *config.CloudSettings, reqSpec map[string]string) map[string]string {
	spec := make(map[string]string, len(reqSpec)+3)
	set := func(key, value string) {
		if value != "" {
			spec[key] = value
		}
	}
	set("project", cs.GCPProject)
	set("region", cs.GCPRegion)
	set("zone", cs.GCPZone)
	maps.Copy(spec, reqSpec)
	return spec
}

// awsSpecWithDefaults merges the runtime AWS settings into the per-request
// machine spec (request values win).
func awsSpecWithDefaults(cs *config.CloudSettings, reqSpec map[string]string) map[string]string {
	spec := make(map[string]string, len(reqSpec)+2)
	set := func(key, value string) {
		if value != "" {
			spec[key] = value
		}
	}
	set("region", cs.AWSRegion)
	set("zone", cs.AWSZone)
	maps.Copy(spec, reqSpec)
	return spec
}

// cloudCredentials maps configuration into IaC cloud credentials. PVE, GCP,
// and AWS credentials come from the runtime-adjustable settings; Aliyun (a
// non-functional stub provider) remains conf/env-only.
func (m *Manager) cloudCredentials(cs *config.CloudSettings) *iac.CloudCredentials {
	return &iac.CloudCredentials{
		AliyunAccessKey: m.config.CloudAliyunAK,
		AliyunSecretKey: m.config.CloudAliyunSK,
		GCPKeyFile:      cs.GCPKeyFile,
		AWSAccessKey:    cs.AWSAccessKey,
		AWSSecretKey:    cs.AWSSecretKey,
		PVETokenID:      cs.PVETokenID,
		PVETokenSecret:  cs.PVETokenSecret,
		PVEUsername:     cs.PVEUsername,
		PVEPassword:     cs.PVEPassword,
	}
}

// runBuildOnInstance submits the build to a provisioned instance's builder and
// blocks until it reaches a terminal state, updating the local job as it goes.
func (m *Manager) runBuildOnInstance(jobID string, instance *iac.Instance, req *BuildRequest) error {
	baseURL := normalizeBuilderURL(instance.BuilderEndpoint)
	compileLease, finishCompile, err := m.beginCompileLease(
		jobID, instance.ID, instance.PrivateIP, req,
	)
	if err != nil {
		return err
	}
	compileFailure := ""
	defer func() { finishCompile(compileFailure) }()
	req.CompileLease = compileLease
	defer func() { req.CompileLease = nil }()

	remoteJobID, err := m.postBuildToBuilder(baseURL, req)
	if err != nil {
		compileFailure = "connect"
		return fmt.Errorf("failed to submit build to instance: %w", err)
	}

	// Poll until terminal, with an overall timeout tied to the instance TTL.
	deadline := time.Now().Add(2 * time.Hour)
	if instance.TTL > 0 {
		deadline = time.Now().Add(instance.TTL)
	}
	statusURL := fmt.Sprintf("%s/api/v1/jobs/%s", baseURL, remoteJobID)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	lastLogSnapshot := ""
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("build timed out on instance %s", instance.ID)
		}
		<-ticker.C

		snap, err := m.fetchInstanceJob(statusURL)
		if err != nil {
			return fmt.Errorf("failed to poll instance builder: %w", err)
		}
		// Forward the remote build log incrementally into the local job log.
		m.appendRemoteBuildLog(jobID, snap.Log, &lastLogSnapshot)
		m.iacMgr.UpdateInstanceActivity(instance.ID)
		localStatus := snap.Status
		if snap.Terminal && snap.Status != "failed" {
			localStatus = "collecting"
		}
		if !m.updateStatus(jobID, localStatus, instance.ID, snap.Error) {
			return fmt.Errorf("durable remote-build phase transition was rejected")
		}

		if snap.Terminal {
			m.recordCompileReport(jobID, compileLease, snap.Metadata)
			if snap.Status == "failed" {
				compileFailure = "remote-compile"
				return fmt.Errorf("remote build failed: %s", snap.Error)
			}
			if reason, err := m.checkCompileOutput(jobID, compileLease); err != nil {
				compileFailure = reason
				return fmt.Errorf("distcc output fence rejected before collection: %w", err)
			} else if reason != "" {
				compileFailure = reason
			}
			// Success: pull every produced package off the instance into the
			// central binhost BEFORE the VM can go away. The requested
			// package's own file becomes the job's primary artifact;
			// dependencies land alongside it (category preserved).
			commitFence := func() error {
				reason, err := m.checkCompileOutput(jobID, compileLease)
				if reason != "" {
					compileFailure = reason
				}
				return err
			}
			if err := m.collectInstanceArtifacts(
				jobID, baseURL, remoteJobID, req.PackageName, snap, commitFence,
			); err != nil {
				return fmt.Errorf("build succeeded on instance but artifact retrieval failed: %w", err)
			}
			return nil
		}
	}
}

// postBuildToBuilder submits a build to a builder base URL and returns its job ID.
func (m *Manager) postBuildToBuilder(baseURL string, req *BuildRequest) (string, error) {
	localReq := LocalBuildRequest{
		PackageName:   req.PackageName,
		Version:       req.Version,
		Arch:          req.Arch,
		ProfileID:     req.ProfileID,
		RepositoryIDs: append([]string(nil), req.RepositoryIDs...),
		ResourceClass: req.ResourceClass,
		ProjectID:     req.ProjectID,
		UseFlags:      make(map[string]string),
		Environment:   make(map[string]string),
		ConfigBundle:  req.ConfigBundle,
		CompileLease:  req.CompileLease,
	}
	if req.CompileLease != nil {
		localReq.AttemptID = req.CompileLease.AttemptID
		localReq.AttemptFence = req.CompileLease.AttemptFence
	}
	for _, flag := range req.UseFlags {
		if name, found := strings.CutPrefix(flag, "-"); found {
			localReq.UseFlags[name] = "disabled"
		} else {
			localReq.UseFlags[flag] = "enabled"
		}
	}

	data, err := json.Marshal(localReq)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/build", bytes.NewBuffer(data))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setBuilderAuth(httpReq, m.config.BuilderToken)

	resp, err := builderHTTPClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("builder returned %d: %s", resp.StatusCode, string(body))
	}

	var buildResp BuildResponse
	if err := json.NewDecoder(resp.Body).Decode(&buildResp); err != nil {
		return "", err
	}
	if buildResp.JobID == "" {
		return "", fmt.Errorf("builder did not return a job id")
	}
	return buildResp.JobID, nil
}

// fetchInstanceJob queries a builder job's status once. buildLog is the remote
// job's full build log so far (streamed into the local job log as a delta).
// remoteJobSnapshot is one poll of a builder-side job.
type remoteJobSnapshot struct {
	Status      string
	Error       string
	Log         string
	ArtifactURL string
	Artifacts   []string
	Signed      bool
	Terminal    bool
	Metadata    map[string]interface{}
}

func (m *Manager) fetchInstanceJob(statusURL string) (*remoteJobSnapshot, error) {
	resp, err := m.getFromBuilder(statusURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var job struct {
		Status      string         `json:"status"`
		Error       string         `json:"error"`
		Log         string         `json:"log"`
		ArtifactURL string         `json:"artifact_url"`
		Artifacts   []string       `json:"artifacts"`
		Metadata    map[string]any `json:"metadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, err
	}

	signed, _ := job.Metadata["signed"].(bool)
	return &remoteJobSnapshot{
		Status:      job.Status,
		Error:       job.Error,
		Log:         job.Log,
		ArtifactURL: job.ArtifactURL,
		Artifacts:   job.Artifacts,
		Signed:      signed,
		Terminal:    job.Status == "completed" || job.Status == "failed" || job.Status == "success",
		Metadata:    job.Metadata,
	}, nil
}

// submitToRemoteBuilder forwards a build request to a configured static remote
// builder. Builders are tried round-robin starting from a rotating cursor
// (previously every job went to RemoteBuilders[0], so additional builders
// never received work); if submission to one fails, the next is tried before
// the job is marked failed.
func (m *Manager) submitToRemoteBuilder(jobID string, req *BuildRequest) {
	builders := m.remoteBuilders()
	if len(builders) == 0 {
		m.updateStatus(jobID, "failed", "", "no remote builders configured")
		return
	}

	start := int(m.rrNext.Add(1)-1) % len(builders)
	var lastErr error
	for i := 0; i < len(builders); i++ {
		builderURL := normalizeBuilderURL(builders[(start+i)%len(builders)])
		if err := m.submitToBuilderAt(jobID, "", builderURL, req); err != nil {
			lastErr = err
			fmt.Printf("Warning: build %s submission to builder %s failed: %v\n", jobID, builderURL, err)
			continue
		}
		return
	}
	m.updateStatus(jobID, "failed", "", fmt.Sprintf("all %d remote builder(s) rejected the build, last error: %v", len(builders), lastErr))
}

// submitToBuilderAt forwards a build request to a specific builder base URL and
// starts polling it. builderAddr is the address recorded for status polling; if
// empty, baseURL is used. Submission failures are returned (not written to the
// job) so the caller can try another builder before giving up.
func (m *Manager) submitToBuilderAt(jobID, builderAddr, baseURL string, req *BuildRequest) error {
	if builderAddr == "" {
		builderAddr = baseURL
	}
	builderURL := fmt.Sprintf("%s/api/v1/build", baseURL)

	if !m.updateStatus(jobID, "forwarding", "", "") {
		return fmt.Errorf("durable forwarding phase transition was rejected")
	}

	// Convert BuildRequest to LocalBuildRequest format
	localReq := LocalBuildRequest{
		PackageName:   req.PackageName, // Already in category/package format from server
		Version:       req.Version,
		Arch:          req.Arch,
		ProfileID:     req.ProfileID,
		RepositoryIDs: append([]string(nil), req.RepositoryIDs...),
		ResourceClass: req.ResourceClass,
		ProjectID:     req.ProjectID,
		UseFlags:      make(map[string]string),
		Environment:   make(map[string]string),
		ConfigBundle:  req.ConfigBundle, // Forward the full config bundle when present.
	}

	// Convert UseFlags from []string to map[string]string
	for _, flag := range req.UseFlags {
		if name, found := strings.CutPrefix(flag, "-"); found {
			localReq.UseFlags[name] = "disabled"
		} else {
			localReq.UseFlags[flag] = "enabled"
		}
	}

	// Marshal request
	data, err := json.Marshal(localReq)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	// Forward to remote builder with the shared token and a bounded timeout.
	httpReq, err := http.NewRequest(http.MethodPost, builderURL, bytes.NewBuffer(data))
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setBuilderAuth(httpReq, m.config.BuilderToken)

	resp, err := builderHTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to forward to builder: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("builder returned error: %s", string(body))
	}

	// Parse response to get remote job ID
	var buildResp BuildResponse
	if err := json.NewDecoder(resp.Body).Decode(&buildResp); err != nil {
		return fmt.Errorf("failed to parse builder response: %w", err)
	}

	// Track remote job ID
	m.jobsMu.Lock()
	m.remoteBuilds[jobID] = buildResp.JobID
	m.jobsMu.Unlock()

	// Start polling remote builder for status
	go m.pollRemoteBuilder(jobID, builderAddr, buildResp.JobID, req)
	return nil
}

// maxConsecutivePollFailures is how many poll attempts in a row may fail
// before a remote build is declared lost. A single transient network blip or
// builder restart must not permanently fail a build that is still running.
const maxConsecutivePollFailures = 6

// pollRemoteBuilder polls remote builder for job status.
func (m *Manager) pollRemoteBuilder(localJobID, builderAddr, remoteJobID string, req *BuildRequest) {
	baseURL := normalizeBuilderURL(builderAddr)
	statusURL := fmt.Sprintf("%s/api/v1/jobs/%s", baseURL, remoteJobID)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	failures := 0
	lastLogSnapshot := ""
	for range ticker.C {
		resp, err := m.getFromBuilder(statusURL)
		if err != nil {
			failures++
			if failures >= maxConsecutivePollFailures {
				m.updateStatus(localJobID, "failed", "", fmt.Sprintf("failed to poll builder %d times in a row: %v", failures, err))
				return
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			failures++
			if failures >= maxConsecutivePollFailures {
				m.updateStatus(localJobID, "failed", "", fmt.Sprintf("builder returned status %d while polling (%d consecutive failures)", resp.StatusCode, failures))
				return
			}
			continue
		}
		failures = 0

		var remoteJob struct {
			ID          string                 `json:"id"`
			Status      string                 `json:"status"`
			Error       string                 `json:"error,omitempty"`
			Log         string                 `json:"log"`
			ArtifactURL string                 `json:"artifact_url"`
			Artifacts   []string               `json:"artifacts,omitempty"`
			Metadata    map[string]interface{} `json:"metadata,omitempty"`
			StartTime   time.Time              `json:"start_time"`
			EndTime     time.Time              `json:"end_time"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&remoteJob); err != nil {
			_ = resp.Body.Close()
			m.updateStatus(localJobID, "failed", "", fmt.Sprintf("failed to parse status: %v", err))
			return
		}
		_ = resp.Body.Close()

		// Logs and errors are distinct surfaces. Keeping the entire remote log
		// in Error made successful static-builder jobs look failed and left the
		// actual logs page with only its initial queued marker.
		m.appendRemoteBuildLog(localJobID, remoteJob.Log, &lastLogSnapshot)
		localStatus := remoteJob.Status
		if (remoteJob.Status == "completed" || remoteJob.Status == "success") && remoteJob.Error == "" {
			localStatus = "collecting"
		}
		if !m.updateStatus(localJobID, localStatus, "", remoteJob.Error) {
			return
		}

		// Stop polling if terminal state reached
		if remoteJob.Status == "completed" || remoteJob.Status == "failed" || remoteJob.Status == "success" {
			if remoteJob.Status != "failed" {
				if remoteJob.ArtifactURL == "" {
					m.setFailedStage(localJobID, "collect")
					m.updateStatus(localJobID, "failed", "", "remote build completed without an artifact")
				} else {
					signed, _ := remoteJob.Metadata["signed"].(bool)
					snap := &remoteJobSnapshot{
						ArtifactURL: remoteJob.ArtifactURL,
						Artifacts:   append([]string(nil), remoteJob.Artifacts...),
						Signed:      signed,
					}
					if err := m.collectInstanceArtifacts(
						localJobID, baseURL, remoteJobID, req.PackageName, snap, nil,
					); err != nil {
						m.setFailedStage(localJobID, "collect")
						m.updateStatus(localJobID, "failed", "", "artifact collection failed: "+err.Error())
					} else if stage, err := m.verifyAndPublish(localJobID, baseURL, baseURL, req); err != nil {
						m.setFailedStage(localJobID, stage)
						m.cleanupArtifactQuarantine(localJobID)
						m.updateStatus(localJobID, "failed", "", err.Error())
					}
				}
			}
			m.jobsMu.Lock()
			delete(m.remoteBuilds, localJobID)
			m.jobsMu.Unlock()
			return
		}
	}
}

// maxJobLogBytes caps a job's live log so a chatty provision/build cannot grow
// server memory without bound.
const maxJobLogBytes = 2 * 1024 * 1024

// ansiEscapes matches ANSI color/cursor escape sequences so raw tool output
// (terraform, apt, emerge) stays readable in the web UI.
var ansiEscapes = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

var (
	logAuthorizationSecret = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer\s+|pveapitoken=))\S+`)
	logKeyValueSecret      = regexp.MustCompile(`(?i)\b(password|passwd|token_secret|api_key|pm_pass|secret)\s*([=:])\s*([^\s,;]+)`)
	logQuerySecret         = regexp.MustCompile(`(?i)([?&](?:token|secret|password|api_key)=)[^&#\s]+`)
	logURLUserInfo         = regexp.MustCompile(`(https?://)[^/\s:@]+:[^/@\s]+@`)
	logVerifyCapability    = regexp.MustCompile(`(/verify-binhost/)[^/\s?]+`)
)

func redactJobLogLine(line string) string {
	line = logAuthorizationSecret.ReplaceAllString(line, `${1}<redacted>`)
	line = logKeyValueSecret.ReplaceAllString(line, `${1}${2}<redacted>`)
	line = logQuerySecret.ReplaceAllString(line, `${1}<redacted>`)
	line = logVerifyCapability.ReplaceAllString(line, `${1}<redacted>`)
	return logURLUserInfo.ReplaceAllString(line, `${1}<redacted>@`)
}

// appendJobLog appends a line to a job's live log (served by the logs API and
// shown on the dashboard's logs page while provisioning/building runs).
//
// Truncation keeps the HEAD and the TAIL, dropping the middle: earlier stages'
// logs (provision/deploy) must stay visible even when a later stage floods the
// log (a full portage tree sync prints tens of thousands of lines).
func (m *Manager) appendJobLog(jobID, line string) {
	m.jobsMu.Lock()
	job, ok := m.jobs[jobID]
	if !ok {
		m.jobsMu.Unlock()
		return
	}
	line = ansiEscapes.ReplaceAllString(line, "")
	line = strings.ReplaceAll(line, "\r", "")
	line = redactJobLogLine(line)
	recordedAt := time.Now().UTC()
	records := make([]LogRecord, 0, strings.Count(line, "\n")+1)
	for _, part := range strings.Split(strings.TrimRight(line, "\n"), "\n") {
		job.Log += recordedAt.Format(time.RFC3339Nano) + " " + part + "\n"
		records = append(records, LogRecord{OccurredAt: recordedAt, Message: part})
	}
	if len(job.Log) > maxJobLogBytes {
		head := job.Log[:maxJobLogBytes/4]
		if i := strings.LastIndexByte(head, '\n'); i > 0 {
			head = head[:i+1]
		}
		tail := job.Log[len(job.Log)-maxJobLogBytes/2:]
		if i := strings.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		}
		job.Log = head + "[... log truncated: middle omitted ...]\n" + tail
	}
	job.UpdatedAt = time.Now()
	status := *job
	m.jobsMu.Unlock()
	if m.logLedger != nil && len(records) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = m.logLedger.AppendLogs(ctx, &status, records)
		cancel()
	}
}

// ListInstances returns the current cloud instances (provisioning, running,
// or pending destroy-retry) for the dashboard's Build Nodes page.
func (m *Manager) ListInstances() []*iac.Instance {
	return m.iacMgr.ListInstances()
}

// terminalStatus reports whether a job status is final.
func terminalStatus(s string) bool {
	return s == "failed" || s == "completed" || s == "success" || s == "canceled"
}

// DeleteJob removes a terminal job record. In-flight jobs are refused so a
// running build cannot lose its bookkeeping.
func (m *Manager) DeleteJob(jobID string) error {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	job, ok := m.jobs[jobID]
	if !ok {
		return fmt.Errorf("job not found: %s", jobID)
	}
	if !terminalStatus(job.Status) {
		return fmt.Errorf("job %s is %s; only finished jobs can be deleted", jobID, job.Status)
	}
	if m.jobLedger != nil {
		snapshot := *job
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := m.jobLedger.HideJob(ctx, &snapshot, "operator_delete")
		cancel()
		if err != nil {
			return fmt.Errorf("hide job in ledger: %w", err)
		}
	}
	delete(m.jobs, jobID)
	return nil
}

// CleanupFailedJobs removes all failed job records and returns the count.
func (m *Manager) CleanupFailedJobs() int {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	n := 0
	for id, job := range m.jobs {
		if job.Status == "failed" {
			if m.jobLedger != nil {
				snapshot := *job
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := m.jobLedger.HideJob(ctx, &snapshot, "cleanup_failed")
				cancel()
				if err != nil {
					continue
				}
			}
			delete(m.jobs, id)
			n++
		}
	}
	return n
}

// CancelJob fences any active executor before updating the local projection.
func (m *Manager) CancelJob(jobID, reason string) error {
	if m.scheduler == nil {
		return fmt.Errorf("durable scheduler is not enabled")
	}
	if strings.TrimSpace(reason) == "" {
		reason = "canceled by operator"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	status, err := m.scheduler.CancelJob(ctx, jobID, reason)
	cancel()
	if err != nil {
		return err
	}
	m.jobsMu.Lock()
	if local, ok := m.jobs[jobID]; ok {
		status.Request = local.Request
		status.Log = local.Log
	}
	m.jobs[jobID] = status
	m.jobsMu.Unlock()
	m.emitStatus(status)
	return nil
}

// RetryJob queues a new fenced attempt for a failed/canceled job.
func (m *Manager) RetryJob(jobID string) error {
	if m.scheduler == nil {
		return fmt.Errorf("durable scheduler is not enabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	status, err := m.scheduler.RetryJob(ctx, jobID)
	cancel()
	if err != nil {
		return err
	}
	m.jobsMu.Lock()
	if local, ok := m.jobs[jobID]; ok {
		status.Log = local.Log
	}
	m.jobs[jobID] = status
	m.jobsMu.Unlock()
	m.signalWorkers()
	m.emitStatus(status)
	return nil
}

// setFailedStage records which pipeline stage a job failed in.
func (m *Manager) setFailedStage(jobID, stage string) {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	if job, ok := m.jobs[jobID]; ok {
		job.FailedStage = stage
	}
}

// updateStatus updates the status of a build job.
func (m *Manager) updateStatus(jobID, status, instanceID, errorMsg string) bool {
	m.jobsMu.Lock()
	job, exists := m.jobs[jobID]
	if !exists {
		m.jobsMu.Unlock()
		return false
	}
	previous := *job
	current := previous
	current.Status = status
	current.UpdatedAt = time.Now()
	if instanceID != "" {
		current.InstanceID = instanceID
	}
	if errorMsg != "" {
		current.Error = redactJobLogLine(errorMsg)
	} else if status != "failed" {
		// A successful/non-failed transition must not retain a stale error
		// from a previous polling or infrastructure state.
		current.Error = ""
	}
	if m.scheduler != nil && current.AttemptID != "" {
		m.jobsMu.Unlock()
		err := m.recordDurableTransition(&previous, &current)
		if err != nil {
			m.appendJobLog(jobID, "[scheduler] durable transition rejected: "+err.Error())
			return false
		}
		m.jobsMu.Lock()
		if live, ok := m.jobs[jobID]; ok &&
			live.AttemptID == previous.AttemptID &&
			live.FenceToken == previous.FenceToken &&
			live.Status == previous.Status {
			*live = current
		}
		m.jobsMu.Unlock()
		recordCompletedBuildMetric(previous.Status, current.Status)
		recordFailedBuildMetric(previous.Status, current.Status)
		m.emitStatus(&current)
		return true
	}
	*job = current
	m.jobsMu.Unlock()
	recordCompletedBuildMetric(previous.Status, current.Status)
	recordFailedBuildMetric(previous.Status, current.Status)

	if m.jobLedger != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = m.jobLedger.RecordTransition(ctx, &previous, &current)
		cancel()
	}
	m.emitStatus(&current)
	return true
}

// persistJobSnapshot flushes a projection change that carries no state change
// — the adopted signing result, the promoted artifact locations — into the
// durable status snapshot immediately. Both are established in memory and
// would otherwise stay memory-only until the next phase transition, which is a
// network upload or a multi-second binhost regeneration away; the 5-second
// ledger reconciler runs inside that window and reinstates the older snapshot.
func (m *Manager) persistJobSnapshot(jobID string) error {
	current, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return err
	}
	previous := *current
	if m.scheduler != nil && current.AttemptID != "" {
		return m.recordDurableTransition(&previous, current)
	}
	if m.jobLedger == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.jobLedger.RecordTransition(ctx, &previous, current)
}

func (m *Manager) recordDurableTransition(previous, current *BuildStatus) error {
	lastWait := ""
	attempts := 0
	backoff := 200 * time.Millisecond
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := m.scheduler.RecordTransition(ctx, previous, current)
		cancel()
		capacity, waiting := AsPhaseCapacityError(err)
		if !waiting {
			if !retryableLedgerError(err) || attempts >= maxLedgerTransitionRetries {
				return err
			}
			// The caller turns a rejected transition into a terminal phase
			// failure that also drops the worker lease, so lease recovery can
			// no longer retry the job. A serialization failure, deadlock, lock
			// timeout, or the five-second budget this loop imposed on itself
			// must not cost the build.
			attempts++
			m.appendJobLog(current.JobID, fmt.Sprintf(
				"[scheduler] retrying transient ledger transition (%d/%d): %v",
				attempts, maxLedgerTransitionRetries, err,
			))
			select {
			case <-m.stopCh:
				return err
			case <-time.After(backoff):
			}
			backoff *= 2
			continue
		}
		message := fmt.Sprintf(
			"[scheduler] waiting for project %s phase capacity (%d/%d)",
			capacity.Phase, capacity.Used, capacity.Limit,
		)
		if message != lastWait {
			m.appendJobLog(current.JobID, message)
			lastWait = message
		}
		select {
		case <-m.stopCh:
			return fmt.Errorf("control plane stopped while waiting for %s capacity", capacity.Phase)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// recordCompletedBuildMetric counts a build that reached the terminal
// completed state. The success-rate panel divides this counter by
// builds_total, which the HTTP admission path already increments, so leaving
// it unwired rendered a red 0% on a cluster where every build had succeeded.
func recordCompletedBuildMetric(previous, current string) {
	if current == "completed" && previous != "completed" {
		metrics.Default().IncBuildsSucceeded()
	}
}

// recordFailedBuildMetric is its terminal-failure sibling. The counter's only
// other writer is the legacy remote-builder HTTP client, which StartWorkers
// refuses to run at all in the topology the public boundary requires, so the
// "Builds Failed" panel and the failed series of the build-rate chart read a
// flat zero however many builds break. The previous→current guard is the
// success counter's: whichever of the phase path and updateStatus commits the
// transition first counts it, and the other sees previous == "failed".
func recordFailedBuildMetric(previous, current string) {
	if current == "failed" && previous != "failed" {
		metrics.Default().IncBuildsFailed()
	}
}

// maxLedgerTransitionRetries bounds transient-failure retries so a database
// that is genuinely down still fails the job instead of holding a phase lease
// forever.
const maxLedgerTransitionRetries = 4

// retryableLedgerError separates transient PostgreSQL and transport conditions
// from the ledger's own verdicts. A stale fence or a state mismatch is a real
// answer and must reach the caller unchanged; a serialization failure
// (40001), deadlock (40P01), lock-not-available (55P03), a timed-out attempt,
// or a dropped connection says nothing about the job and is safe to repeat
// because RecordTransition is idempotent on an unchanged status digest.
func retryableLedgerError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", "40P01", "55P03":
			return true
		default:
			return false
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// appendRemoteBuildLog appends only the newly observed suffix of a builder
// log. The server owns the ingestion timestamp and stage marker, so both cloud
// and static builders produce the same structured log surface. If a builder
// restarts, truncates, or replaces its log, record that discontinuity and
// ingest the replacement from the beginning.
func (m *Manager) appendRemoteBuildLog(jobID, current string, previous *string) {
	if previous == nil || current == "" {
		return
	}
	start := len(*previous)
	if !strings.HasPrefix(current, *previous) {
		m.appendJobLog(jobID, "[build] remote log restarted or was truncated")
		start = 0
	}
	if start == len(current) {
		return
	}
	chunk := strings.TrimRight(current[start:], "\r\n")
	*previous = current
	if chunk == "" {
		return
	}
	m.appendJobLog(jobID, "[build] "+strings.ReplaceAll(chunk, "\n", "\n[build] "))
}

// ListAllBuilds returns all build jobs, including those from remote builders.
func (m *Manager) ListAllBuilds() []*BuildStatus {
	m.jobsMu.RLock()
	localBuilds := make([]*BuildStatus, 0, len(m.jobs))
	for _, job := range m.jobs {
		// Copy under the lock so concurrent updateStatus writes don't race the
		// caller's reads / JSON encoding.
		jobCopy := *job
		localBuilds = append(localBuilds, &jobCopy)
	}
	m.jobsMu.RUnlock()
	if m.scheduler != nil {
		return localBuilds
	}

	// Aggregate builds from remote builders
	remoteBuilds := m.fetchRemoteBuilderJobs()

	// Merge local and remote builds, avoiding duplicates
	allBuilds := localBuilds
	localJobIDs := make(map[string]bool)
	for _, job := range localBuilds {
		localJobIDs[job.JobID] = true
	}
	for _, job := range remoteBuilds {
		if !localJobIDs[job.JobID] {
			allBuilds = append(allBuilds, job)
		}
	}

	return allBuilds
}

// fetchRemoteBuilderJobs fetches jobs from all configured remote builders.
func (m *Manager) fetchRemoteBuilderJobs() []*BuildStatus {
	if len(m.remoteBuilders()) == 0 {
		return nil
	}

	var allJobs []*BuildStatus
	var mu sync.Mutex
	var wg sync.WaitGroup

	client := &http.Client{Timeout: 5 * time.Second}

	for _, builder := range m.remoteBuilders() {
		wg.Add(1)
		go func(builderAddr string) {
			defer wg.Done()

			baseURL := normalizeBuilderURL(builderAddr)
			url := fmt.Sprintf("%s/api/v1/jobs", baseURL)
			resp, err := m.builderGet(client, url)
			if err != nil {
				return
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				return
			}

			var jobs []struct {
				ID      string `json:"id"`
				Request struct {
					PackageName string `json:"package_name"`
					Version     string `json:"version"`
					Arch        string `json:"arch"`
				} `json:"request"`
				Status      string    `json:"status"`
				StartTime   time.Time `json:"start_time"`
				EndTime     time.Time `json:"end_time"`
				Log         string    `json:"log"`
				ArtifactURL string    `json:"artifact_url"`
				Error       string    `json:"error"`
			}

			if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
				return
			}

			mu.Lock()
			for _, job := range jobs {
				arch := job.Request.Arch
				if arch == "" {
					arch = "amd64"
				}
				version := job.Request.Version
				if version == "" {
					version = extractVersionFromArtifact(job.ArtifactURL)
				}
				status := &BuildStatus{
					JobID:        job.ID,
					PackageName:  job.Request.PackageName,
					Version:      version,
					Arch:         arch,
					Status:       job.Status,
					CreatedAt:    job.StartTime,
					UpdatedAt:    job.EndTime,
					InstanceID:   builderAddr,
					ArtifactPath: job.ArtifactURL,
					Log:          job.Log,
					Error:        job.Error,
				}
				// Normalize status names
				if status.Status == "success" {
					status.Status = "completed"
				}
				allJobs = append(allJobs, status)
			}
			mu.Unlock()
		}(builder)
	}

	wg.Wait()
	return allJobs
}

// ClusterStatus represents the overall cluster status.
type ClusterStatus struct {
	ActiveBuilds    int       `json:"active_builds"`
	QueuedBuilds    int       `json:"queued_builds"`
	ActiveInstances int       `json:"active_instances"`
	TotalBuilds     int       `json:"total_builds"`
	CompletedBuilds int       `json:"completed_builds"`
	FailedBuilds    int       `json:"failed_builds"`
	SuccessRate     float64   `json:"success_rate"`
	LastUpdated     time.Time `json:"last_updated"`
}

// GetClusterStatus returns the current cluster status.
// It aggregates status from local jobs and remote builders.
func (m *Manager) GetClusterStatus() *ClusterStatus {
	status := &ClusterStatus{
		LastUpdated: time.Now(),
	}

	// Count local jobs
	m.jobsMu.RLock()
	for _, job := range m.jobs {
		status.TotalBuilds++
		switch job.Status {
		case "claimed", "building", "provisioning", "forwarding":
			status.ActiveBuilds++
		case "queued":
			status.QueuedBuilds++
		case "completed":
			status.CompletedBuilds++
		case "failed":
			status.FailedBuilds++
		}
	}
	m.jobsMu.RUnlock()

	// Aggregate stats from remote builders
	remoteStats := m.fetchRemoteBuilderStats()
	status.TotalBuilds += remoteStats.TotalBuilds
	status.ActiveBuilds += remoteStats.ActiveBuilds
	status.QueuedBuilds += remoteStats.QueuedBuilds
	status.CompletedBuilds += remoteStats.CompletedBuilds
	status.FailedBuilds += remoteStats.FailedBuilds
	status.ActiveInstances += remoteStats.ActiveInstances

	// Get active instances count from IaC manager
	status.ActiveInstances += len(m.iacMgr.ListInstances())

	// Calculate success rate
	if status.CompletedBuilds+status.FailedBuilds > 0 {
		status.SuccessRate = float64(status.CompletedBuilds) / float64(status.CompletedBuilds+status.FailedBuilds) * 100
	}

	return status
}

// fetchRemoteBuilderStats fetches aggregated stats from all remote builders.
func (m *Manager) fetchRemoteBuilderStats() *ClusterStatus {
	stats := &ClusterStatus{}

	if len(m.remoteBuilders()) == 0 {
		return stats
	}

	var mu sync.Mutex
	var wg sync.WaitGroup

	client := &http.Client{Timeout: 5 * time.Second}

	for _, builderAddr := range m.remoteBuilders() {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()

			baseURL := normalizeBuilderURL(addr)
			url := fmt.Sprintf("%s/api/v1/status", baseURL)
			resp, err := m.builderGet(client, url)
			if err != nil {
				return
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				return
			}

			var builderStatus struct {
				Workers   int `json:"workers"`
				Queued    int `json:"queued"`
				Building  int `json:"building"`
				Completed int `json:"completed"`
				Failed    int `json:"failed"`
				Total     int `json:"total"`
			}

			if err := json.NewDecoder(resp.Body).Decode(&builderStatus); err != nil {
				return
			}

			mu.Lock()
			stats.TotalBuilds += builderStatus.Total
			stats.QueuedBuilds += builderStatus.Queued
			stats.ActiveBuilds += builderStatus.Building
			stats.CompletedBuilds += builderStatus.Completed
			stats.FailedBuilds += builderStatus.Failed
			// Count each remote builder with workers as an active instance
			if builderStatus.Workers > 0 {
				stats.ActiveInstances++
			}
			mu.Unlock()
		}(builderAddr)
	}

	wg.Wait()
	return stats
}

// GetBuildLogs returns logs for a specific build job.
func (m *Manager) GetBuildLogs(jobID string) (string, error) {
	// First check local jobs. Copy under the lock so formatLocalLogs never reads
	// the live struct while updateStatus is writing it.
	m.jobsMu.RLock()
	status, exists := m.jobs[jobID]
	if exists {
		statusCopy := *status
		m.jobsMu.RUnlock()
		if m.logLedger != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			durableLog, err := m.logLedger.LoadLogs(ctx, jobID)
			cancel()
			if err == nil {
				statusCopy.Log = durableLog
			}
		}
		return m.formatLocalLogs(&statusCopy), nil
	}
	m.jobsMu.RUnlock()
	if m.scheduler != nil {
		return "", fmt.Errorf("job not found in PostgreSQL projection: %s", jobID)
	}

	// Try to fetch from remote builders
	logs, err := m.fetchRemoteBuilderLogs(jobID)
	if err == nil {
		return logs, nil
	}

	return "", fmt.Errorf("job not found: %s", jobID)
}

// formatLocalLogs formats logs for a local job.
func (m *Manager) formatLocalLogs(status *BuildStatus) string {
	logs := fmt.Sprintf("Build Job: %s\n", status.JobID)
	logs += fmt.Sprintf("Package: %s-%s\n", status.PackageName, status.Version)
	logs += fmt.Sprintf("Architecture: %s\n", status.Arch)
	logs += fmt.Sprintf("Status: %s\n", status.Status)
	logs += fmt.Sprintf("Created: %s\n", status.CreatedAt.Format(time.RFC3339))
	logs += fmt.Sprintf("Updated: %s\n", status.UpdatedAt.Format(time.RFC3339))
	if status.InstanceID != "" {
		logs += fmt.Sprintf("Builder Instance: %s\n", status.InstanceID)
	}
	logs += "\n--- Build Output ---\n"

	// Include actual log if available
	if status.Log != "" {
		logs += redactJobLogLine(status.Log)
	} else {
		logs += "(no build output recorded)\n"
	}

	switch status.Status {
	case "completed", "success":
		logs += "\nBuild completed successfully\n"
		if status.ArtifactPath != "" {
			logs += fmt.Sprintf("Artifact: %s\n", status.ArtifactPath)
		}
	case "failed":
		logs += fmt.Sprintf("\nBuild failed: %s\n", redactJobLogLine(status.Error))
	case "building":
		logs += "\nBuild in progress...\n"
	}

	return logs
}

// fetchRemoteBuilderLogs fetches logs for a job from remote builders.
func (m *Manager) fetchRemoteBuilderLogs(jobID string) (string, error) {
	if len(m.remoteBuilders()) == 0 {
		return "", fmt.Errorf("no remote builders configured")
	}

	client := &http.Client{Timeout: 10 * time.Second}

	for _, builder := range m.remoteBuilders() {
		baseURL := normalizeBuilderURL(builder)
		url := fmt.Sprintf("%s/api/v1/jobs/%s", baseURL, jobID)
		resp, err := m.builderGet(client, url)
		if err != nil {
			continue
		}

		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			continue
		}

		var job struct {
			ID      string `json:"id"`
			Request struct {
				PackageName string `json:"package_name"`
				Version     string `json:"version"`
			} `json:"request"`
			Status      string    `json:"status"`
			StartTime   time.Time `json:"start_time"`
			EndTime     time.Time `json:"end_time"`
			Log         string    `json:"log"`
			ArtifactURL string    `json:"artifact_url"`
			Error       string    `json:"error"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
			_ = resp.Body.Close()
			continue
		}
		_ = resp.Body.Close()

		// Format the logs
		logs := fmt.Sprintf("Build Job: %s\n", job.ID)
		logs += fmt.Sprintf("Package: %s\n", job.Request.PackageName)
		logs += fmt.Sprintf("Version: %s\n", job.Request.Version)
		logs += fmt.Sprintf("Status: %s\n", job.Status)
		logs += fmt.Sprintf("Builder: %s\n", builder)
		logs += fmt.Sprintf("Started: %s\n", job.StartTime.Format(time.RFC3339))
		if !job.EndTime.IsZero() {
			logs += fmt.Sprintf("Finished: %s\n", job.EndTime.Format(time.RFC3339))
		}
		logs += "\n--- Build Output ---\n"
		if job.Log != "" {
			logs += redactJobLogLine(job.Log)
		}
		if job.Error != "" {
			logs += fmt.Sprintf("\nError: %s\n", job.Error)
		}
		if job.ArtifactURL != "" {
			logs += fmt.Sprintf("\nArtifact: %s\n", job.ArtifactURL)
		}

		return logs, nil
	}

	return "", fmt.Errorf("job not found on any remote builder")
}

// GetSchedulerStatus returns scheduler status with task assignments.
func (m *Manager) GetSchedulerStatus() map[string]interface{} {
	m.jobsMu.RLock()
	defer m.jobsMu.RUnlock()

	runningTasks := 0
	queuedTasks := 0
	tasksByBuilder := make(map[string][]string)

	for jobID, job := range m.jobs {
		switch job.Status {
		case "claimed", "building", "provisioning", "forwarding":
			runningTasks++
			if job.InstanceID != "" {
				tasksByBuilder[job.InstanceID] = append(tasksByBuilder[job.InstanceID], jobID)
			}
		case "queued":
			queuedTasks++
		}
	}

	builders := make([]map[string]interface{}, 0, len(tasksByBuilder))
	for builderID, tasks := range tasksByBuilder {
		builders = append(builders, map[string]interface{}{
			"id":           builderID,
			"capacity":     4,
			"current_load": len(tasks),
			"enabled":      true,
			"healthy":      true,
			"tasks":        tasks,
		})
	}

	status := map[string]interface{}{
		"authority":     "memory",
		"builders":      builders,
		"queued_tasks":  queuedTasks,
		"running_tasks": runningTasks,
	}
	if m.scheduler != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		durable, err := m.scheduler.RuntimeStatus(ctx)
		cancel()
		if err != nil {
			status["authority"] = "postgresql"
			status["healthy"] = false
			status["error"] = err.Error()
			return status
		}
		data, err := json.Marshal(durable)
		if err == nil {
			_ = json.Unmarshal(data, &status)
		}
		if m.config.PhaseExecutorMode != "active" {
			if autoscaler, ok := status["autoscaler"].(map[string]interface{}); ok {
				autoscaler["mode"] = "off"
				autoscaler["recommendation"] = "off"
				autoscaler["reason"] = "phase executor mode is shadow; capacity inventory only"
				if pools, ok := autoscaler["pools"].([]interface{}); ok {
					for _, item := range pools {
						if pool, ok := item.(map[string]interface{}); ok {
							pool["mode"] = "off"
							pool["recommendation"] = "off"
							pool["reason"] = "phase executor mode is shadow; capacity inventory only"
						}
					}
				}
			}
		}
		status["healthy"] = true
	}
	return status
}

// UpdateBuilderHeartbeat updates builder heartbeat information.
func (m *Manager) UpdateBuilderHeartbeat(req *HeartbeatRequest) error {
	if req.BuilderID == "" {
		return fmt.Errorf("builder_id is required")
	}

	// For now, just validate the request
	// In the future, this could update scheduler state
	if req.Status == "" {
		return fmt.Errorf("status is required")
	}

	return nil
}

// normalizeBuilderURL ensures the builder address has the correct URL format.
// It handles cases where the address may or may not include the http:// prefix.
func normalizeBuilderURL(address string) string {
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		return address
	}
	return fmt.Sprintf("http://%s", address)
}

// builderHTTPClient is used for all server→builder calls. Unlike http.DefaultClient
// it has a timeout, so a hung builder cannot block a worker goroutine forever.
var builderHTTPClient = &http.Client{Timeout: 30 * time.Second}

// artifactHTTPClient downloads build artifacts from builders. Binary packages
// are routinely tens to hundreds of MB, so the timeout is much larger than the
// control-plane client's.
var artifactHTTPClient = &http.Client{Timeout: 15 * time.Minute}

func (m *Manager) beginArtifactQuarantine(jobID string) (string, error) {
	return m.beginArtifactQuarantineToken(
		jobID, strings.ReplaceAll(uuid.NewString(), "-", ""),
	)
}

func (m *Manager) beginArtifactQuarantineToken(jobID, token string) (string, error) {
	if m.config.BinpkgPath == "" {
		return "", fmt.Errorf("BINPKG_PATH is not configured")
	}
	if !capabilityTokenRegex.MatchString(token) {
		return "", fmt.Errorf("invalid artifact quarantine token")
	}
	if _, err := m.artifactBudgetRemaining(jobID); err != nil {
		return "", err
	}
	base := m.artifactQuarantineBase()
	root := filepath.Join(base, token)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create artifact quarantine: %w", err)
	}

	m.jobsMu.Lock()
	if job, ok := m.jobs[jobID]; ok {
		job.StagingRoot = root
		job.VerificationToken = token
		job.StagedArtifacts = nil
		job.StagedPrimary = ""
	}
	m.jobsMu.Unlock()
	return root, nil
}

func (m *Manager) artifactQuarantineBase() string {
	if m.objectQuarantineEnabled() {
		base := strings.TrimSpace(m.config.DataDir)
		if base == "" {
			base = os.TempDir()
		}
		return filepath.Join(base, "quarantine-cache")
	}
	return filepath.Join(filepath.Dir(filepath.Clean(m.config.BinpkgPath)), ".portage-engine-quarantine")
}

func (m *Manager) objectQuarantineEnabled() bool {
	return m.artifactStore != nil && strings.EqualFold(m.config.StorageType, "s3")
}

func (m *Manager) persistJobQuarantine(
	jobID, root string,
	rels []string,
) error {
	if !m.objectQuarantineEnabled() {
		return nil
	}
	m.jobsMu.RLock()
	job := m.jobs[jobID]
	token := ""
	if job != nil {
		token = job.VerificationToken
	}
	m.jobsMu.RUnlock()
	if !capabilityTokenRegex.MatchString(token) {
		return fmt.Errorf("job %s has no valid object quarantine token", jobID)
	}
	for _, rel := range rels {
		key, err := artifactstorage.QuarantineGenerationKey(token, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		local := filepath.Join(root, filepath.FromSlash(rel))
		if err := m.artifactStore.Upload(local, key); err != nil {
			return fmt.Errorf("upload quarantine artifact %q: %w", rel, err)
		}
	}
	return nil
}

// gatewayQuarantineKey converts the Broker's portable root-relative
// destination (token/artifact) into the private immutable object key. The
// exported storage helper independently validates both the capability token
// and the worker-selected artifact path.
func gatewayQuarantineKey(relative string) (string, error) {
	portable := filepath.ToSlash(relative)
	token, artifact, ok := strings.Cut(portable, "/")
	if !ok || artifact == "" {
		return "", fmt.Errorf("invalid gateway quarantine destination")
	}
	return artifactstorage.QuarantineGenerationKey(token, artifact)
}

// storeGatewayUpload copies a Broker-owned fenced generation into a private
// regular temporary file before handing it to Storage.Upload. This preserves
// the Broker's os.Root confinement across the path-based storage interface and
// makes the shared object durable before the PostgreSQL upload row completes.
func (m *Manager) storeGatewayUpload(
	relative string,
	source io.Reader,
	expectedSize int64,
	expectedDigest string,
) error {
	if !m.objectQuarantineEnabled() {
		return fmt.Errorf("object quarantine is not configured")
	}
	if expectedSize < 0 || !sha256DigestRegex.MatchString(expectedDigest) {
		return fmt.Errorf("invalid gateway artifact attestation")
	}
	key, err := gatewayQuarantineKey(relative)
	if err != nil {
		return err
	}
	scratch := filepath.Join(m.artifactQuarantineBase(), ".object-upload")
	if err := os.MkdirAll(scratch, 0o750); err != nil {
		return fmt.Errorf("create gateway object scratch: %w", err)
	}
	temporary, err := os.CreateTemp(scratch, ".generation-*")
	if err != nil {
		return fmt.Errorf("create gateway object generation: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	hash := sha256.New()
	written, copyErr := io.Copy(
		io.MultiWriter(temporary, hash), io.LimitReader(source, expectedSize+1),
	)
	if copyErr != nil {
		return fmt.Errorf("copy gateway object generation: %w", copyErr)
	}
	if written != expectedSize {
		return fmt.Errorf("copy gateway object generation: size=%d expected=%d",
			written, expectedSize)
	}
	actualDigest := hex.EncodeToString(hash.Sum(nil))
	if actualDigest != strings.ToLower(expectedDigest) {
		return fmt.Errorf("gateway object generation digest mismatch")
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync gateway object generation: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close gateway object generation: %w", err)
	}
	if err := m.artifactStore.Upload(temporaryPath, key); err != nil {
		return fmt.Errorf("upload gateway object %q: %w", key, err)
	}
	return nil
}

// materializeGatewayArtifact restores the object committed by the API-side
// Broker into the active phase executor's private quarantine. The subsequent
// size/digest check remains the final attestation before the artifact enters a
// job generation.
func (m *Manager) materializeGatewayArtifact(
	jobID, relative, destination string,
) error {
	if !m.objectQuarantineEnabled() {
		return nil
	}
	m.jobsMu.RLock()
	job := m.jobs[jobID]
	token := ""
	if job != nil {
		token = job.VerificationToken
	}
	m.jobsMu.RUnlock()
	key, err := artifactstorage.QuarantineGenerationKey(token, filepath.ToSlash(relative))
	if err != nil {
		return fmt.Errorf("resolve collected artifact object: %w", err)
	}
	if err := m.artifactStore.Download(key, destination); err != nil {
		return fmt.Errorf("materialize collected artifact %q: %w", relative, err)
	}
	return nil
}

func (m *Manager) materializeObjectQuarantine(
	token string,
	rels []string,
) (string, error) {
	if !m.objectQuarantineEnabled() {
		return "", fmt.Errorf("object quarantine is not configured")
	}
	if !capabilityTokenRegex.MatchString(token) || len(rels) == 0 {
		return "", fmt.Errorf("invalid object quarantine selection")
	}
	base := m.artifactQuarantineBase()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	root, err := os.MkdirTemp(base, token+"-*")
	if err != nil {
		return "", fmt.Errorf("create quarantine scratch: %w", err)
	}
	clean := false
	defer func() {
		if !clean {
			_ = os.RemoveAll(root)
		}
	}()
	for _, rel := range rels {
		key, keyErr := artifactstorage.QuarantineGenerationKey(token, filepath.ToSlash(rel))
		if keyErr != nil {
			return "", keyErr
		}
		destination := filepath.Join(root, filepath.FromSlash(rel))
		if err := m.artifactStore.Download(key, destination); err != nil {
			return "", fmt.Errorf("materialize quarantine artifact %q: %w", rel, err)
		}
	}
	clean = true
	return root, nil
}

func (m *Manager) ensureObjectJobQuarantine(jobID string) (string, error) {
	m.jobsMu.RLock()
	job := m.jobs[jobID]
	root, token := "", ""
	var rels []string
	if job != nil {
		root = job.StagingRoot
		token = job.VerificationToken
		rels = append([]string(nil), job.StagedArtifacts...)
	}
	m.jobsMu.RUnlock()
	if job == nil {
		return "", fmt.Errorf("job %s not found", jobID)
	}
	if !m.objectQuarantineEnabled() {
		return root, nil
	}
	materialized, err := m.materializeObjectQuarantine(token, rels)
	if err != nil {
		return "", err
	}
	m.jobsMu.Lock()
	if current := m.jobs[jobID]; current != nil &&
		current.VerificationToken == token {
		current.StagingRoot = materialized
	}
	m.jobsMu.Unlock()
	if root != materialized {
		_ = os.RemoveAll(root)
	}
	return materialized, nil
}

func (m *Manager) deleteObjectQuarantine(token string) error {
	if !m.objectQuarantineEnabled() || token == "" {
		return nil
	}
	prefix, err := artifactstorage.QuarantineGenerationPrefix(token)
	if err != nil {
		return err
	}
	keys, err := m.artifactStore.List(prefix)
	if err != nil {
		return fmt.Errorf("list quarantine generation: %w", err)
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			return fmt.Errorf("object store returned key outside quarantine prefix")
		}
		if err := m.artifactStore.Delete(key); err != nil {
			return fmt.Errorf("delete quarantine object %q: %w", key, err)
		}
	}
	return nil
}

func generationArtifactsFromRecords(
	rels []string,
	records []ArtifactRecord,
) []artifactstorage.GenerationArtifact {
	artifacts := make([]artifactstorage.GenerationArtifact, len(records))
	for index, record := range records {
		artifacts[index] = artifactstorage.GenerationArtifact{
			RelativePath: filepath.ToSlash(rels[index]),
			SHA256:       record.Digest,
			Size:         record.SizeBytes,
		}
	}
	return artifacts
}

func (m *Manager) prepareVerificationBinhost(jobID, arch string) (string, error) {
	m.jobsMu.RLock()
	job, ok := m.jobs[jobID]
	if !ok {
		m.jobsMu.RUnlock()
		return "", fmt.Errorf("job %s not found", jobID)
	}
	root, token, signed := job.StagingRoot, job.VerificationToken, job.Signed
	rels := append([]string(nil), job.StagedArtifacts...)
	m.jobsMu.RUnlock()
	if (!m.objectQuarantineEnabled() && root == "") ||
		token == "" || len(rels) == 0 {
		return "", fmt.Errorf("job %s has no staged artifacts", jobID)
	}
	if arch == "" {
		return "", fmt.Errorf("job %s has no architecture for its verification index", jobID)
	}
	if m.objectQuarantineEnabled() {
		materialized, err := m.materializeObjectQuarantine(token, rels)
		if err != nil {
			return "", err
		}
		if root != materialized {
			_ = os.RemoveAll(root)
		}
		root = materialized
		m.jobsMu.Lock()
		if current := m.jobs[jobID]; current != nil &&
			current.VerificationToken == token {
			current.StagingRoot = root
		}
		m.jobsMu.Unlock()
	}
	if _, err := binpkg.NewStore(root).RegenerateIndex(arch); err != nil {
		return "", fmt.Errorf("generate quarantine Packages index: %w", err)
	}
	expiresAt := time.Now().UTC().Add(30 * time.Minute)
	if m.objectQuarantineEnabled() {
		generation := "unsigned"
		if signed {
			generation = "signed"
		}
		if err := m.activateObjectCapability(
			root, token, generation, arch, rels, expiresAt,
		); err != nil {
			return "", err
		}
	} else {
		// The marker is shared, revocable capability state in compatibility
		// mode. Public deployments use the object-backed manifest below.
		if err := os.WriteFile(filepath.Join(root, verificationCapabilityFile),
			[]byte(strconv.FormatInt(expiresAt.Unix(), 10)), 0o600); err != nil {
			return "", fmt.Errorf("activate quarantine capability: %w", err)
		}
	}

	callback := strings.TrimRight(m.CloudSettings().ServerCallbackURL, "/")
	parsed, err := neturl.Parse(callback)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("SERVER_CALLBACK_URL must be an absolute http or https URL for quarantined verification")
	}
	return callback + "/verify-binhost/" + token, nil
}

func (m *Manager) activateObjectCapability(
	root, token, generation, arch string,
	rels []string,
	expiresAt time.Time,
) error {
	records, err := artifactMetadata(root, rels, &BuildStatus{}, "quarantine")
	if err != nil {
		return err
	}
	artifacts := generationArtifactsFromRecords(rels, records)
	capabilityKey, err := artifactstorage.QuarantineCapabilityKey(token)
	if err != nil {
		return err
	}
	exists, err := m.artifactStore.Exists(capabilityKey)
	if err != nil {
		return fmt.Errorf("inspect quarantine capability: %w", err)
	}
	if exists {
		current, downloadErr := artifactstorage.DownloadBytes(
			m.artifactStore, capabilityKey, m.artifactQuarantineBase(), 1<<20,
		)
		if downloadErr == nil {
			parsed, parseErr := artifactstorage.ParseQuarantineManifest(current, time.Now())
			if parseErr == nil && parsed.Token == token &&
				parsed.Generation == generation &&
				parsed.Architecture == arch &&
				slices.Equal(parsed.Artifacts, artifacts) {
				return nil
			}
		}
		if err := m.artifactStore.Delete(capabilityKey); err != nil {
			return fmt.Errorf("replace expired quarantine capability: %w", err)
		}
	}
	packagesPath := filepath.Join(root, "Packages")
	packagesDigest, _, err := digestRegularFileAt(root, "Packages")
	if err != nil {
		return fmt.Errorf("digest quarantine Packages index: %w", err)
	}
	packagesKey, err := artifactstorage.QuarantineGenerationKey(token, "Packages")
	if err != nil {
		return err
	}
	if packagesExists, existsErr := m.artifactStore.Exists(packagesKey); existsErr != nil {
		return fmt.Errorf("inspect quarantine Packages index: %w", existsErr)
	} else if packagesExists {
		if err := m.artifactStore.Delete(packagesKey); err != nil {
			return fmt.Errorf("replace quarantine Packages index: %w", err)
		}
	}
	if err := m.artifactStore.Upload(packagesPath, packagesKey); err != nil {
		return fmt.Errorf("upload quarantine Packages index: %w", err)
	}
	manifest := artifactstorage.QuarantineManifest{
		SchemaVersion:  artifactstorage.QuarantineManifestSchema,
		Token:          token,
		Generation:     generation,
		Architecture:   arch,
		PackagesSHA256: packagesDigest,
		Artifacts:      artifacts,
		ExpiresAt:      expiresAt,
	}
	document, err := manifest.Marshal()
	if err != nil {
		return err
	}
	if err := artifactstorage.UploadBytes(
		m.artifactStore, capabilityKey, document, m.artifactQuarantineBase(),
	); err != nil {
		return fmt.Errorf("activate object quarantine capability: %w", err)
	}
	return nil
}

// ServeVerificationBinhost exposes one active job quarantine through an
// unguessable capability URL. It permits only regular-file GET/HEAD requests,
// disables caching, and is revoked before the job reaches a terminal state.
func (m *Manager) ServeVerificationBinhost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/verify-binhost/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	token := parts[0]
	if !capabilityTokenRegex.MatchString(token) {
		http.NotFound(w, r)
		return
	}
	if m.objectQuarantineEnabled() {
		m.serveObjectVerificationBinhost(w, r, token, parts[1])
		return
	}
	// Compatibility mode puts the quarantine on a filesystem the signer also
	// mounts, so the tree this reads is writable by another trust domain. A
	// cleaned relative path only proves the request did not spell an escape;
	// it says nothing about what the directory entries themselves point at.
	// Everything below is therefore addressed through the quarantine root,
	// which refuses to traverse a component that links out of the quarantine.
	// Without that, a planted directory symlink would redirect an
	// unsigned-artifact read anywhere the server can reach, including the
	// published binhost this capability exists to keep artifacts out of.
	//
	// The root is anchored on the base and the token opened beneath it,
	// because os.OpenRoot resolves its own argument by name: opening
	// base/<token> in one call would let the same writer who can plant an
	// entry inside the token directory replace the token directory itself
	// with a symlink, and that entry decides where the confinement points
	// before there is any confinement to stop it. Addressed as a component,
	// the token is resolved under the base root like every path below it.
	base, err := os.OpenRoot(m.artifactQuarantineBase())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = base.Close() }()
	quarantineRoot, err := base.OpenRoot(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = quarantineRoot.Close() }()
	marker, err := quarantineRoot.ReadFile(verificationCapabilityFile)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	expiry, err := strconv.ParseInt(strings.TrimSpace(string(marker)), 10, 64)
	if err != nil || time.Now().Unix() >= expiry {
		http.NotFound(w, r)
		return
	}

	rel := path.Clean(strings.ReplaceAll(parts[1], "\\", "/"))
	if rel == verificationCapabilityFile || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		http.NotFound(w, r)
		return
	}
	local := filepath.FromSlash(rel)
	if entry, err := quarantineRoot.Lstat(local); err != nil || !entry.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	file, err := quarantineRoot.Open(local)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The open descriptor is served rather than the name it was opened by, so
	// no second resolution can land on a different file than the one checked.
	http.ServeContent(w, r, filepath.Base(rel), info.ModTime(), file)
}

func (m *Manager) serveObjectVerificationBinhost(
	w http.ResponseWriter,
	r *http.Request,
	token, requested string,
) {
	rel := path.Clean(strings.ReplaceAll(requested, "\\", "/"))
	relNative := filepath.FromSlash(rel)
	if rel == verificationCapabilityFile || !filepath.IsLocal(relNative) {
		http.NotFound(w, r)
		return
	}
	capabilityKey, err := artifactstorage.QuarantineCapabilityKey(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	document, err := artifactstorage.DownloadBytes(
		m.artifactStore, capabilityKey, m.artifactQuarantineBase(), 1<<20,
	)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	manifest, err := artifactstorage.ParseQuarantineManifest(document, time.Now())
	if err != nil || manifest.Token != token || !manifest.Allows(rel) {
		http.NotFound(w, r)
		return
	}
	key, err := artifactstorage.QuarantineGenerationKey(token, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	quarantineRoot, err := os.OpenRoot(m.artifactQuarantineBase())
	if err != nil {
		http.Error(w, "Verification storage unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = quarantineRoot.Close() }()
	scratch, err := os.MkdirTemp(m.artifactQuarantineBase(), "serve-"+token+"-*")
	if err != nil {
		http.Error(w, "Verification storage unavailable", http.StatusServiceUnavailable)
		return
	}
	scratchName := filepath.Base(scratch)
	defer func() { _ = quarantineRoot.RemoveAll(scratchName) }()
	scratchRoot, err := quarantineRoot.OpenRoot(scratchName)
	if err != nil {
		http.Error(w, "Verification storage unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = scratchRoot.Close() }()
	localName := filepath.Base(relNative)
	filePath := filepath.Join(scratch, localName)
	if err := m.artifactStore.Download(key, filePath); err != nil {
		http.NotFound(w, r)
		return
	}
	file, err := scratchRoot.Open(localName)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = file.Close() }()
	digest, size, err := digestRegularOpenFile(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	expectedDigest := manifest.PackagesSHA256
	expectedSize := int64(-1)
	if rel != "Packages" {
		found := false
		for _, artifact := range manifest.Artifacts {
			if artifact.RelativePath == rel {
				expectedDigest = artifact.SHA256
				expectedSize = artifact.Size
				found = true
				break
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
	}
	if digest != expectedDigest || (expectedSize >= 0 && size != expectedSize) {
		http.NotFound(w, r)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		http.NotFound(w, r)
		return
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(rel), info.ModTime(), file)
}

func (m *Manager) cleanupArtifactQuarantine(jobID string) {
	m.jobsMu.Lock()
	job, ok := m.jobs[jobID]
	if !ok {
		m.jobsMu.Unlock()
		return
	}
	root := job.StagingRoot
	token := job.VerificationToken
	status := *job
	job.StagingRoot = ""
	job.VerificationToken = ""
	job.StagedArtifacts = nil
	job.StagedPrimary = ""
	m.jobsMu.Unlock()

	if root != "" {
		_ = os.RemoveAll(root)
	}
	if err := m.deleteObjectQuarantine(token); err != nil {
		m.appendJobLog(jobID, "[collect] warning: delete object quarantine: "+err.Error())
	}
	if m.artifactDB != nil && status.AttemptID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := m.artifactDB.ReleaseArtifactBudget(ctx, &status, "quarantine_cleanup")
		cancel()
		if err != nil {
			m.appendJobLog(jobID, "[collect] warning: release artifact budget: "+err.Error())
		}
	}
}

// removeArtifactQuarantineFiles is used only after the terminal database
// commit has already released the generation budget. It must not attempt a
// fenced metadata write with an intentionally closed attempt lease.
func (m *Manager) removeArtifactQuarantineFiles(jobID string) {
	m.jobsMu.Lock()
	job := m.jobs[jobID]
	root, token := "", ""
	if job != nil {
		root = job.StagingRoot
		token = job.VerificationToken
		job.StagingRoot = ""
		job.VerificationToken = ""
		job.StagedArtifacts = nil
		job.StagedPrimary = ""
	}
	m.jobsMu.Unlock()
	if root != "" {
		_ = os.RemoveAll(root)
	}
	if err := m.deleteObjectQuarantine(token); err != nil {
		m.appendJobLog(jobID, "[collect] warning: delete object quarantine: "+err.Error())
	}
}

func (m *Manager) artifactBudgetRemaining(jobID string) (int64, error) {
	if m.artifactDB == nil {
		return maxPullArtifactBytes, nil
	}
	status, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	budget, err := m.artifactDB.ArtifactBudget(ctx, status)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("read artifact quarantine budget: %w", err)
	}
	remaining := budget.LimitBytes - budget.ActiveBytes
	if remaining <= 0 {
		return 0, fmt.Errorf(
			"artifact quarantine budget exhausted: active=%d limit=%d",
			budget.ActiveBytes, budget.LimitBytes,
		)
	}
	return remaining, nil
}

func (m *Manager) artifactBudgetLimit(jobID string) (int64, error) {
	if m.artifactDB == nil {
		return maxPullArtifactBytes, nil
	}
	status, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	budget, err := m.artifactDB.ArtifactBudget(ctx, status)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("read artifact quarantine budget: %w", err)
	}
	if budget.LimitBytes <= 0 {
		return 0, fmt.Errorf("artifact quarantine budget is not positive")
	}
	return budget.LimitBytes, nil
}

func stablePhaseUUID(parts ...string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(strings.Join(parts, "\x00"))).String()
}

func stablePhaseToken(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func (m *Manager) setArtifactGeneration(
	jobID, generation, root string,
	rels []string,
) (ArtifactBudget, error) {
	var size int64
	for _, rel := range rels {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || !info.Mode().IsRegular() {
			return ArtifactBudget{}, fmt.Errorf("artifact %q is not a regular file", rel)
		}
		if info.Size() > math.MaxInt64-size {
			return ArtifactBudget{}, fmt.Errorf("artifact generation size overflow")
		}
		size += info.Size()
	}
	if m.artifactDB == nil {
		if size > maxPullArtifactBytes {
			return ArtifactBudget{}, fmt.Errorf(
				"artifact quarantine budget exceeded: active=%d limit=%d",
				size, maxPullArtifactBytes,
			)
		}
		return ArtifactBudget{
			LimitBytes: maxPullArtifactBytes, ActiveBytes: size, PeakBytes: size,
		}, nil
	}
	status, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return ArtifactBudget{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	budget, err := m.artifactDB.SetArtifactGenerationBytes(
		ctx, status, generation, size,
	)
	cancel()
	if err != nil {
		return ArtifactBudget{}, err
	}
	return budget, nil
}

func (m *Manager) releaseArtifactGeneration(jobID, generation, reason string) error {
	if m.artifactDB == nil {
		return nil
	}
	status, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = m.artifactDB.ReleaseArtifactGeneration(ctx, status, generation, reason)
	cancel()
	return err
}

func (m *Manager) promoteJobArtifacts(jobID string) error {
	if m.promoteArtifacts == nil && !m.objectQuarantineEnabled() {
		return fmt.Errorf("artifact promotion is not configured")
	}
	m.jobsMu.RLock()
	job, ok := m.jobs[jobID]
	if !ok {
		m.jobsMu.RUnlock()
		return fmt.Errorf("job %s not found", jobID)
	}
	root := job.StagingRoot
	rels := append([]string(nil), job.StagedArtifacts...)
	primary := job.StagedPrimary
	token := job.VerificationToken
	arch := job.Arch
	status := *job
	m.jobsMu.RUnlock()
	if m.objectQuarantineEnabled() {
		materialized, materializeErr := m.ensureObjectJobQuarantine(jobID)
		if materializeErr != nil {
			return fmt.Errorf("materialize promotion inputs: %w", materializeErr)
		}
		root = materialized
	}
	binhostPath, err := buildStatusBinhostPath(&status)
	if err != nil {
		return err
	}
	if err := m.checkPromotionArtifactBudget(jobID, root, rels, status.Signed); err != nil {
		return err
	}

	var releasePromotion func() error
	if m.promotionDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		release, err := m.promotionDB.AcquireArtifactPromotion(ctx, &status)
		cancel()
		if err != nil {
			return fmt.Errorf("acquire cross-replica publication lock: %w", err)
		}
		releasePromotion = release
		defer func() { _ = releasePromotion() }()
	}

	records, err := artifactMetadata(root, rels, &status, "staged")
	if err != nil {
		return err
	}
	if m.metadata != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = m.metadata.RecordArtifacts(ctx, &status, records)
		cancel()
		if err != nil {
			return fmt.Errorf("persist staged artifact metadata: %w", err)
		}
	}
	var paths []string
	if m.objectQuarantineEnabled() {
		paths, err = m.publishObjectGeneration(
			&status, root, token, rels, arch, binhostPath,
		)
	} else {
		paths, err = m.promoteArtifacts(root, rels, arch, binhostPath)
	}
	if err != nil {
		return err
	}
	if len(paths) != len(rels) {
		return fmt.Errorf("promotion returned %d paths for %d artifacts", len(paths), len(rels))
	}
	if m.objectQuarantineEnabled() && m.onArtifactStored != nil {
		m.onArtifactStored()
	}
	// Promotion is the only moment a package becomes publicly consumable, so
	// it is where the stored-package counter belongs.
	for range paths {
		metrics.Default().IncPackagesStored()
	}
	pathByRel := make(map[string]string, len(rels))
	webs := make([]string, 0, len(rels))
	for i, rel := range rels {
		pathByRel[rel] = paths[i]
		webs = append(webs, "/binpkgs/"+binhostPath+"/"+filepath.ToSlash(rel))
	}
	if primary == "" && len(rels) > 0 {
		primary = rels[0]
	}

	m.jobsMu.Lock()
	if job, ok := m.jobs[jobID]; ok {
		job.ArtifactPath = pathByRel[primary]
		job.ArtifactURL = "/binpkgs/" + binhostPath + "/" + filepath.ToSlash(primary)
		job.ArtifactPaths = append([]string(nil), paths...)
		job.Artifacts = webs
	}
	m.jobsMu.Unlock()
	// Publication is complete and irreversible above this line: the packages
	// are in the public binhost and the Packages index names them. Both writes
	// below are projections of that fact, and neither is the fence that
	// authorized it — the publish phase's FinalizePhaseWork is, and it
	// adjudicates on the phase claim and attempt fence rather than on the
	// worker lease persistJobSnapshot additionally requires. Reporting a
	// projection failure as a promotion failure sends the caller to
	// failActivePhase and marks a job failed whose packages the world can
	// already install, so both are logged and the phase commit decides the
	// terminal state. A replica that genuinely lost its fence cannot commit
	// either verdict, and its successor republishes nothing: the object
	// generation ID is derived from the attempt, so publishObjectGeneration
	// returns the existing keys untouched.
	if err := m.persistJobSnapshot(jobID); err != nil {
		m.appendJobLog(jobID,
			"[publish] warning: artifacts are public but their locations were not projected: "+err.Error())
	}
	if m.metadata != nil {
		publishedAt := time.Now().UTC()
		for i := range records {
			records[i].State = "published"
			records[i].Published = &publishedAt
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = m.metadata.RecordArtifacts(ctx, &status, records)
		cancel()
		if err != nil {
			m.appendJobLog(jobID,
				"[publish] warning: published artifact metadata was not recorded: "+err.Error())
		}
	}
	m.cleanupArtifactQuarantine(jobID)
	return nil
}

func (m *Manager) publishObjectGeneration(
	status *BuildStatus,
	root, token string,
	rels []string,
	arch, binhostPath string,
) ([]string, error) {
	versioned, ok := m.artifactStore.(artifactstorage.VersionedStorage)
	if !ok {
		return nil, fmt.Errorf("S3 publication requires versioned compare-and-swap storage")
	}
	newArtifacts, currentPackages, err := m.loadVerifiedPublicationInput(
		status, root, token, rels, arch,
	)
	if err != nil {
		return nil, err
	}
	channelKey, err := artifactstorage.StableChannelKey(binhostPath, arch)
	if err != nil {
		return nil, err
	}
	previous, err := m.loadPreviousPublication(
		versioned, channelKey, binhostPath, arch,
	)
	if err != nil {
		return nil, err
	}

	attemptID := stablePublicationAttemptID(status)
	generationID := uuid.NewSHA1(
		uuid.NameSpaceURL,
		[]byte(binhostPath+"\x00"+attemptID),
	).String()
	if previous.pointer != nil && previous.pointer.GenerationID == generationID {
		return publishedArtifactKeysForRels(previous.manifest.Artifacts, rels)
	}
	createdAt := status.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Unix(1, 0).UTC()
	}
	mergedPackages, err := binpkg.MergePackagesIndexes(
		previous.packages, currentPackages, arch, createdAt,
	)
	if err != nil {
		return nil, fmt.Errorf("merge publication Packages index: %w", err)
	}
	artifacts, err := m.uploadPublishedGenerationArtifacts(
		root, rels, newArtifacts, previous.manifest,
		binhostPath, arch, generationID,
	)
	if err != nil {
		return nil, err
	}
	manifest, manifestKey, manifestDocument, err := m.uploadPublicationMetadata(
		status, artifacts, mergedPackages, binhostPath, arch,
		generationID, attemptID, createdAt,
	)
	if err != nil {
		return nil, err
	}
	pointer := artifactstorage.ChannelPointer{
		SchemaVersion: artifactstorage.ChannelPointerSchema,
		Channel:       "stable", BinhostPath: binhostPath, Architecture: arch,
		GenerationID: generationID, ManifestKey: manifestKey,
		ManifestSHA256: digestBytes(manifestDocument),
		PackagesSHA256: manifest.PackagesSHA256,
		SelectedAt:     time.Now().UTC(),
	}
	if previous.pointer != nil {
		pointer.PreviousGenerationID = previous.pointer.GenerationID
	}
	pointerDocument, err := pointer.Marshal()
	if err != nil {
		return nil, err
	}
	if _, err := artifactstorage.CompareAndSwapBytes(
		versioned, channelKey, pointerDocument, previous.version,
		m.artifactQuarantineBase(),
	); err != nil {
		return nil, fmt.Errorf("commit stable channel pointer: %w", err)
	}
	return publishedArtifactKeysForRels(artifacts, rels)
}

type previousPublication struct {
	pointer  *artifactstorage.ChannelPointer
	manifest *artifactstorage.GenerationManifest
	packages []byte
	version  string
}

func (m *Manager) loadVerifiedPublicationInput(
	status *BuildStatus,
	root, token string,
	rels []string,
	arch string,
) ([]artifactstorage.GenerationArtifact, []byte, error) {
	capabilityKey, err := artifactstorage.QuarantineCapabilityKey(token)
	if err != nil {
		return nil, nil, err
	}
	capabilityDocument, err := artifactstorage.DownloadBytes(
		m.artifactStore, capabilityKey, m.artifactQuarantineBase(), 1<<20,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("read publication capability: %w", err)
	}
	capability, err := artifactstorage.ParseQuarantineManifest(
		capabilityDocument, time.Now(),
	)
	if err != nil || capability.Token != token || capability.Architecture != arch {
		return nil, nil, fmt.Errorf("publication capability is invalid or expired")
	}
	records, err := artifactMetadata(root, rels, status, "publishing")
	if err != nil {
		return nil, nil, err
	}
	artifacts := generationArtifactsFromRecords(rels, records)
	if !slices.Equal(capability.Artifacts, artifacts) {
		return nil, nil, fmt.Errorf("publication artifacts differ from verified capability")
	}
	packagesKey, err := artifactstorage.QuarantineGenerationKey(token, "Packages")
	if err != nil {
		return nil, nil, err
	}
	packages, err := artifactstorage.DownloadBytes(
		m.artifactStore, packagesKey, m.artifactQuarantineBase(), 64<<20,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("read verified Packages index: %w", err)
	}
	if digestBytes(packages) != capability.PackagesSHA256 {
		return nil, nil, fmt.Errorf("verified Packages digest changed before publication")
	}
	return artifacts, packages, nil
}

func (m *Manager) loadPreviousPublication(
	versioned artifactstorage.VersionedStorage,
	channelKey, binhostPath, arch string,
) (previousPublication, error) {
	var previous previousPublication
	exists, err := m.artifactStore.Exists(channelKey)
	if err != nil {
		return previous, fmt.Errorf("inspect stable channel: %w", err)
	}
	if !exists {
		return previous, nil
	}
	document, version, err := artifactstorage.DownloadVersionBytes(
		versioned, channelKey, m.artifactQuarantineBase(), 1<<20,
	)
	if err != nil {
		return previous, fmt.Errorf("read stable channel: %w", err)
	}
	pointer, err := artifactstorage.ParseChannelPointer(document)
	if err != nil || pointer.BinhostPath != binhostPath ||
		pointer.Architecture != arch {
		return previous, fmt.Errorf("stable channel pointer failed validation")
	}
	manifestDocument, err := artifactstorage.DownloadBytes(
		m.artifactStore, pointer.ManifestKey,
		m.artifactQuarantineBase(), 4<<20,
	)
	if err != nil || digestBytes(manifestDocument) != pointer.ManifestSHA256 {
		return previous, fmt.Errorf("stable channel manifest failed digest validation")
	}
	manifest, err := artifactstorage.ParseGenerationManifest(manifestDocument)
	if err != nil || manifest.GenerationID != pointer.GenerationID ||
		manifest.PackagesSHA256 != pointer.PackagesSHA256 {
		return previous, fmt.Errorf("stable channel manifest failed identity validation")
	}
	packagesKey, err := artifactstorage.PublishedGenerationKey(
		binhostPath, arch, pointer.GenerationID, "Packages",
	)
	if err != nil {
		return previous, err
	}
	packages, err := artifactstorage.DownloadBytes(
		m.artifactStore, packagesKey, m.artifactQuarantineBase(), 64<<20,
	)
	if err != nil || digestBytes(packages) != pointer.PackagesSHA256 {
		return previous, fmt.Errorf("stable Packages object failed digest validation")
	}
	previous.pointer = &pointer
	previous.manifest = &manifest
	previous.packages = packages
	previous.version = version
	return previous, nil
}

func (m *Manager) uploadPublishedGenerationArtifacts(
	root string,
	rels []string,
	newArtifacts []artifactstorage.GenerationArtifact,
	previous *artifactstorage.GenerationManifest,
	binhostPath, arch, generationID string,
) ([]artifactstorage.GenerationArtifact, error) {
	artifactByPath := make(map[string]artifactstorage.GenerationArtifact)
	if previous != nil {
		for _, artifact := range previous.Artifacts {
			artifactByPath[artifact.RelativePath] = artifact
		}
	}
	for index, artifact := range newArtifacts {
		key, err := artifactstorage.PublishedGenerationKey(
			binhostPath, arch, generationID, artifact.RelativePath,
		)
		if err != nil {
			return nil, err
		}
		if err := m.artifactStore.Upload(
			filepath.Join(root, filepath.FromSlash(rels[index])), key,
		); err != nil {
			return nil, fmt.Errorf(
				"upload published artifact %q: %w", artifact.RelativePath, err,
			)
		}
		artifact.ObjectKey = key
		artifactByPath[artifact.RelativePath] = artifact
	}
	artifacts := make([]artifactstorage.GenerationArtifact, 0, len(artifactByPath))
	for _, artifact := range artifactByPath {
		artifacts = append(artifacts, artifact)
	}
	slices.SortFunc(artifacts, func(left, right artifactstorage.GenerationArtifact) int {
		return strings.Compare(left.RelativePath, right.RelativePath)
	})
	return artifacts, nil
}

func (m *Manager) uploadPublicationMetadata(
	status *BuildStatus,
	artifacts []artifactstorage.GenerationArtifact,
	packages []byte,
	binhostPath, arch, generationID, attemptID string,
	createdAt time.Time,
) (artifactstorage.GenerationManifest, string, []byte, error) {
	packagesKey, err := artifactstorage.PublishedGenerationKey(
		binhostPath, arch, generationID, "Packages",
	)
	if err != nil {
		return artifactstorage.GenerationManifest{}, "", nil, err
	}
	if err := artifactstorage.UploadBytes(
		m.artifactStore, packagesKey, packages, m.artifactQuarantineBase(),
	); err != nil {
		return artifactstorage.GenerationManifest{}, "", nil,
			fmt.Errorf("upload published Packages: %w", err)
	}
	profileID := "compatibility"
	if status.ResolvedContext != nil && status.ResolvedContext.ProfileID != "" {
		profileID = status.ResolvedContext.ProfileID
	}
	// The manifest is immutable and every GPKG in this generation carries the
	// key the signer actually used. GPGPublicKeyPath is rewritten on every
	// signer start, so re-reading it here would stamp a rotated key ID onto an
	// older generation; only the signer-reported value is authoritative.
	signingKeyID := "unsigned"
	if status.Signed {
		signingKeyID = strings.TrimSpace(status.SigningKeyID)
		if signingKeyID == "" {
			return artifactstorage.GenerationManifest{}, "", nil, fmt.Errorf(
				"signed generation has no signer-reported key ID to record",
			)
		}
	}
	manifest := artifactstorage.GenerationManifest{
		SchemaVersion: artifactstorage.GenerationManifestSchema,
		GenerationID:  generationID, BinhostPath: binhostPath,
		Architecture: arch, ProfileID: profileID, AttemptID: attemptID,
		SigningKeyID: signingKeyID, PackagesSHA256: digestBytes(packages),
		Provenance: generationProvenance(status),
		Artifacts:  artifacts, CreatedAt: createdAt,
	}
	manifestDocument, err := manifest.Marshal()
	if err != nil {
		return artifactstorage.GenerationManifest{}, "", nil, err
	}
	manifestKey, err := artifactstorage.PublishedGenerationKey(
		binhostPath, arch, generationID, artifactstorage.GenerationManifestName,
	)
	if err != nil {
		return artifactstorage.GenerationManifest{}, "", nil, err
	}
	if err := artifactstorage.UploadBytes(
		m.artifactStore, manifestKey, manifestDocument, m.artifactQuarantineBase(),
	); err != nil {
		return artifactstorage.GenerationManifest{}, "", nil,
			fmt.Errorf("upload generation manifest: %w", err)
	}
	return manifest, manifestKey, manifestDocument, nil
}

func generationProvenance(status *BuildStatus) artifactstorage.GenerationProvenance {
	provenance := artifactstorage.GenerationProvenance{
		PackageAtom: buildStatusPackageAtom(status),
		BuildMode:   "native-gentoo",
	}
	input := map[string]any{
		"package_atom": provenance.PackageAtom,
		"attempt_id":   stablePublicationAttemptID(status),
	}
	if status != nil {
		input["architecture"] = status.Arch
	}
	if status != nil && status.Request != nil {
		input["use_flags"] = append([]string(nil), status.Request.UseFlags...)
		input["machine_spec"] = maps.Clone(status.Request.MachineSpec)
		input["config_bundle"] = status.Request.ConfigBundle
	}
	if status != nil && status.ResolvedContext != nil {
		resolved := status.ResolvedContext
		provenance.CatalogVersion = resolved.CatalogVersion
		provenance.BuildMode = resolved.BuildMode
		provenance.ImageID = resolved.ImageID
		provenance.ImageGeneration = resolved.ImageGeneration
		provenance.ImageDigest = resolved.ImageDigest
		provenance.MirrorBundleID = resolved.MirrorBundleID
		provenance.MirrorBundleDigest = resolved.MirrorBundleDigest
		provenance.EgressPolicyDigest = resolved.EgressPolicyDigest
		provenance.PackageSetIDs = append([]string(nil), resolved.PackageSetIDs...)
		provenance.PackageSetCatalogDigest = resolved.PackageSetCatalogDigest
		for _, repository := range resolved.Repositories {
			provenance.Repositories = append(
				provenance.Repositories,
				artifactstorage.GenerationRepository{
					ID: repository.ID, Revision: repository.Revision,
					Digest: repository.Digest,
				},
			)
		}
		input["resolved_context"] = resolved
	}
	document, _ := json.Marshal(input)
	provenance.BuildInputSHA256 = digestBytes(document)
	return provenance
}

func buildStatusPackageAtom(status *BuildStatus) string {
	if status == nil {
		return "unknown/unknown"
	}
	atom := strings.TrimSpace(status.PackageName)
	if version := strings.TrimSpace(status.Version); version != "" {
		atom += "-" + version
	}
	if atom == "" {
		return "unknown/unknown"
	}
	return atom
}

func stablePublicationAttemptID(status *BuildStatus) string {
	if status != nil {
		if _, err := uuid.Parse(status.AttemptID); err == nil {
			return status.AttemptID
		}
		if _, err := uuid.Parse(status.JobID); err == nil {
			return status.JobID
		}
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte(status.JobID)).String()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("missing-status")).String()
}

func publishedArtifactKeysForRels(
	artifacts []artifactstorage.GenerationArtifact,
	rels []string,
) ([]string, error) {
	byPath := make(map[string]string, len(artifacts))
	for _, artifact := range artifacts {
		byPath[artifact.RelativePath] = artifact.ObjectKey
	}
	keys := make([]string, len(rels))
	for index, rel := range rels {
		key := byPath[filepath.ToSlash(rel)]
		if key == "" {
			return nil, fmt.Errorf("published generation omitted artifact %q", rel)
		}
		keys[index] = key
	}
	return keys, nil
}

func digestBytes(document []byte) string {
	digest := sha256.Sum256(document)
	return hex.EncodeToString(digest[:])
}

func (m *Manager) checkPromotionArtifactBudget(
	jobID, root string,
	rels []string,
	signed bool,
) error {
	generation := "collected"
	if signed {
		generation = "signed"
	}
	budget, err := m.setArtifactGeneration(jobID, generation, root, rels)
	if err != nil {
		return fmt.Errorf("artifact promotion byte gate: %w", err)
	}
	if budget.ActiveBytes > budget.LimitBytes {
		return fmt.Errorf(
			"artifact promotion byte gate exceeded: active=%d limit=%d",
			budget.ActiveBytes, budget.LimitBytes,
		)
	}
	m.appendJobLog(jobID, fmt.Sprintf(
		"[publish] artifact byte gate passed: active=%d limit=%d peak=%d",
		budget.ActiveBytes, budget.LimitBytes, budget.PeakBytes,
	))
	return nil
}

func artifactMetadata(root string, rels []string, status *BuildStatus, state string) ([]ArtifactRecord, error) {
	binhostPrefix := "/binpkgs"
	if status != nil && (status.ResolvedContext != nil || status.Arch != "") {
		if resolved, err := buildStatusBinhostPath(status); err == nil {
			binhostPrefix += "/" + resolved
		}
	}
	records := make([]ArtifactRecord, 0, len(rels))
	for _, rel := range rels {
		file := filepath.Join(root, filepath.FromSlash(rel))
		handle, err := os.Open(file) // #nosec G304 -- rel is validated as a safe artifact-relative path before joining it to root.
		if err != nil {
			return nil, fmt.Errorf("open artifact for digest %s: %w", rel, err)
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, handle)
		closeErr := handle.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("digest artifact %s: %w", rel, copyErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close artifact %s: %w", rel, closeErr)
		}
		lineage := map[string]any{
			"package": status.PackageName,
			"version": status.Version,
			"arch":    status.Arch,
		}
		if status.ResolvedContext != nil {
			lineage["profile_id"] = status.ResolvedContext.ProfileID
			lineage["image_id"] = status.ResolvedContext.ImageID
			lineage["image_generation"] = status.ResolvedContext.ImageGeneration
			lineage["image_digest"] = status.ResolvedContext.ImageDigest
			lineage["mirror_bundle_id"] = status.ResolvedContext.MirrorBundleID
			lineage["mirror_bundle_digest"] = status.ResolvedContext.MirrorBundleDigest
			lineage["egress_policy_id"] = status.ResolvedContext.EgressPolicy.ID
			lineage["egress_policy_digest"] = status.ResolvedContext.EgressPolicyDigest
		}
		records = append(records, ArtifactRecord{
			Kind: "gentoo-binpkg", State: state,
			Digest: hex.EncodeToString(hash.Sum(nil)), SizeBytes: size,
			MediaType: "application/vnd.gentoo.gpkg", Location: binhostPrefix + "/" + filepath.ToSlash(rel),
			Lineage: lineage,
		})
	}
	return records, nil
}

func digestRegularFileAt(rootDir, name string) (string, int64, error) {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = file.Close() }()
	return digestRegularOpenFile(file)
}

func digestRegularOpenFile(file *os.File) (string, int64, error) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("path is not a regular file")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func buildBinhostPath(req *BuildRequest) (string, error) {
	if req == nil {
		return "", fmt.Errorf("build request is required for binhost routing")
	}
	status := &BuildStatus{Arch: req.Arch, ResolvedContext: req.ResolvedContext}
	return buildStatusBinhostPath(status)
}

func buildStatusBinhostPath(status *BuildStatus) (string, error) {
	if status == nil {
		return "", fmt.Errorf("build status is required for binhost routing")
	}
	arch := status.Arch
	if status.ResolvedContext != nil {
		if status.ResolvedContext.Arch != "" {
			arch = status.ResolvedContext.Arch
		}
		if status.ResolvedContext.BinhostPath != "" {
			if err := catalog.ValidateBinhostPath(status.ResolvedContext.BinhostPath, arch); err != nil {
				return "", fmt.Errorf("resolved binhost path: %w", err)
			}
			return status.ResolvedContext.BinhostPath, nil
		}
		if status.ResolvedContext.ProfileChannel != "" &&
			status.ResolvedContext.ProfileChannel != "compatibility" {
			return "", fmt.Errorf("resolved profile %q has no binhost path", status.ResolvedContext.ProfileID)
		}
	}
	// Compatibility for jobs created before binhost_path entered the resolved
	// context. Non-compatibility catalog jobs fail closed above.
	if arch == "" {
		arch = "amd64"
	}
	if arch != "amd64" {
		return "", fmt.Errorf("legacy compatibility binhost path is unavailable for architecture %q", arch)
	}
	return "releases/amd64/binpackages/23.0/x86-64", nil
}

func (m *Manager) recordInfra(jobID string, instance *iac.Instance, state, failure string, deletedAt *time.Time) error {
	if m.metadata == nil || instance == nil {
		return nil
	}
	job, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return err
	}
	return m.recordInfraWithStatus(job, instance, state, failure, deletedAt)
}

func (m *Manager) jobStatusSnapshot(jobID string) (*BuildStatus, error) {
	m.jobsMu.RLock()
	job, ok := m.jobs[jobID]
	if ok {
		copyStatus := *job
		job = &copyStatus
	}
	m.jobsMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("job %s is missing from runtime projection", jobID)
	}
	return job, nil
}

// recordInfraWithStatus accepts a fence snapshot captured while the build
// attempt is active. Terminal ledger transitions release the worker lease and
// projection refreshes therefore omit the live attempt fields; cleanup still
// needs the originating attempt/fence to mark its exact VM as destroyed.
func (m *Manager) recordInfraWithStatus(job *BuildStatus, instance *iac.Instance, state, failure string, deletedAt *time.Time) error {
	if m.metadata == nil || instance == nil {
		return nil
	}
	if job == nil {
		return fmt.Errorf("runtime metadata requires a build status")
	}
	attributes := map[string]string{
		"arch": instance.Arch, "ip_address": instance.IPAddress,
		"builder_endpoint": instance.BuilderEndpoint,
	}
	for _, key := range []string{
		"resource_name", "node",
		"pe_catalog_version", "pe_profile_id", "pe_image_id", "pe_image_generation",
		"pe_image_digest", "pe_mirror_bundle_id", "pe_mirror_bundle_digest",
		"pe_egress_policy_id", "pe_egress_policy_digest", "pe_egress_enforced",
		"pe_inbound_closed",
	} {
		if value := instance.Metadata[key]; value != "" {
			attributes[key] = value
		}
	}
	var cleanupAfter *time.Time
	if instance.TTL > 0 {
		value := instance.CreatedAt.Add(instance.TTL)
		cleanupAfter = &value
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.metadata.RecordInfra(ctx, job, InfraRecord{
		Provider: instance.Provider, ProviderInstanceID: instance.ID, State: state,
		RemoteStateRef: instance.TerraformDir, Attributes: attributes,
		FailureDetail: failure, CleanupAfter: cleanupAfter, DeletedAt: deletedAt,
	})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// collectInstanceArtifacts pulls every artifact the remote job produced into
// a job-private quarantine and records the unpublished relative paths. It
// falls back to the legacy single-artifact download when the builder predates
// artifact lists.
func (m *Manager) collectInstanceArtifacts(
	jobID, baseURL, remoteJobID, packageName string,
	snap *remoteJobSnapshot,
	commitFence func() error,
) error {
	stagingRoot, err := m.beginArtifactQuarantine(jobID)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			m.cleanupArtifactQuarantine(jobID)
		}
	}()

	if len(snap.Artifacts) == 0 {
		if snap.ArtifactURL == "" {
			return nil
		}
		remaining, err := m.artifactBudgetRemaining(jobID)
		if err != nil {
			return err
		}
		m.appendJobLog(jobID, "[collect] fetching artifact into job quarantine...")
		_, rel, err := m.downloadArtifactToRoot(
			stagingRoot, baseURL, remoteJobID, packageName,
			snap.ArtifactURL, 0o600, remaining,
		)
		if err != nil {
			return err
		}
		budget, err := m.setArtifactGeneration(
			jobID, "collected", stagingRoot, []string{rel},
		)
		if err != nil {
			return err
		}
		if err := checkArtifactCommitFence(commitFence); err != nil {
			return err
		}
		m.appendJobLog(jobID, "[collect] quarantined artifact: "+rel)
		m.appendJobLog(jobID, fmt.Sprintf(
			"[collect] quarantine budget active=%d limit=%d",
			budget.ActiveBytes, budget.LimitBytes,
		))
		m.jobsMu.Lock()
		if job, ok := m.jobs[jobID]; ok {
			job.StagedPrimary = rel
			job.StagedArtifacts = []string{rel}
			job.Signed = snap.Signed
		}
		m.jobsMu.Unlock()
		if err := m.persistJobQuarantine(jobID, stagingRoot, []string{rel}); err != nil {
			return err
		}
		if err := checkArtifactCommitFence(commitFence); err != nil {
			return err
		}
		success = true
		return nil
	}

	m.appendJobLog(jobID, fmt.Sprintf("[collect] fetching %d artifact(s) into job quarantine...", len(snap.Artifacts)))
	rels := make([]string, 0, len(snap.Artifacts))
	primary := ""
	for _, rel := range snap.Artifacts {
		remaining, err := m.artifactBudgetRemaining(jobID)
		if err != nil {
			return err
		}
		_, clean, err := m.downloadArtifactRelToRoot(
			stagingRoot, baseURL, remoteJobID, rel, 0o600, remaining,
		)
		if err != nil {
			return err
		}
		rels = append(rels, clean)
		budget, err := m.setArtifactGeneration(jobID, "collected", stagingRoot, rels)
		if err != nil {
			return err
		}
		m.appendJobLog(jobID, fmt.Sprintf(
			"[collect] quarantined artifact: %s · budget=%d/%d",
			clean, budget.ActiveBytes, budget.LimitBytes,
		))
		if artifactMatchesPackage(rel, packageName) {
			primary = clean
		}
	}
	if primary == "" && len(rels) > 0 {
		primary = rels[0]
	}
	if err := checkArtifactCommitFence(commitFence); err != nil {
		return err
	}
	m.jobsMu.Lock()
	if job, ok := m.jobs[jobID]; ok {
		job.StagedPrimary = primary
		job.StagedArtifacts = rels
		job.Signed = snap.Signed
	}
	m.jobsMu.Unlock()
	if err := m.persistJobQuarantine(jobID, stagingRoot, rels); err != nil {
		return err
	}
	if err := checkArtifactCommitFence(commitFence); err != nil {
		return err
	}
	success = true
	return nil
}

const maxPullArtifactBytes int64 = 32 << 30

// ExecutorProtocolVersion fences security-relevant durable BuildRequests from
// older control-plane binaries that do not understand their full contract.
const ExecutorProtocolVersion = 5

func (m *Manager) collectPullArtifacts(
	jobID string,
	identity workergateway.Identity,
	remoteJobID, packageName string,
	snapshot *remoteJobSnapshot,
	commandNamespace string,
	commitFence func() error,
) error {
	var stagingRoot string
	var err error
	if commandNamespace == "" {
		stagingRoot, err = m.beginArtifactQuarantine(jobID)
	} else {
		stagingRoot, err = m.beginArtifactQuarantineToken(
			jobID, stablePhaseToken(commandNamespace, "collected"),
		)
	}
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			m.cleanupArtifactQuarantine(jobID)
		}
	}()

	m.appendJobLog(jobID, fmt.Sprintf(
		"[collect] worker is streaming %d artifact(s) outbound into job quarantine...",
		len(snapshot.Artifacts)))
	rels := make([]string, 0, len(snapshot.Artifacts))
	primary := ""
	for _, rel := range snapshot.Artifacts {
		clean, result, budget, err := m.collectOnePullArtifact(
			jobID, identity, remoteJobID, stagingRoot, rel, rels,
			commandNamespace,
		)
		if err != nil {
			return err
		}
		rels = append(rels, clean)
		m.appendJobLog(jobID, fmt.Sprintf(
			"[collect] quarantined outbound artifact: %s size=%d sha256=%s budget=%d/%d",
			clean, result.Size, result.SHA256,
			budget.ActiveBytes, budget.LimitBytes,
		))
		if artifactMatchesPackage(rel, packageName) {
			primary = clean
		}
	}
	if primary == "" && len(rels) > 0 {
		primary = rels[0]
	}
	if err := checkArtifactCommitFence(commitFence); err != nil {
		return err
	}
	m.jobsMu.Lock()
	if job := m.jobs[jobID]; job != nil {
		job.StagedPrimary = primary
		job.StagedArtifacts = rels
		job.Signed = snapshot.Signed
	}
	m.jobsMu.Unlock()
	if err := m.persistJobQuarantine(jobID, stagingRoot, rels); err != nil {
		return err
	}
	if err := checkArtifactCommitFence(commitFence); err != nil {
		return err
	}
	success = true
	return nil
}

func checkArtifactCommitFence(commitFence func() error) error {
	if commitFence == nil {
		return nil
	}
	if err := commitFence(); err != nil {
		return fmt.Errorf("artifact commit fence rejected: %w", err)
	}
	return nil
}

func (m *Manager) collectOnePullArtifact(
	jobID string,
	identity workergateway.Identity,
	remoteJobID, stagingRoot, rel string,
	collected []string,
	commandNamespace string,
) (string, workergateway.CollectResult, ArtifactBudget, error) {
	clean := path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if clean == "." || strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") {
		return "", workergateway.CollectResult{}, ArtifactBudget{},
			fmt.Errorf("invalid artifact path %q", rel)
	}
	destination := filepath.Join(stagingRoot, filepath.FromSlash(clean))
	remaining, err := m.artifactBudgetRemaining(jobID)
	if err != nil {
		return "", workergateway.CollectResult{}, ArtifactBudget{}, err
	}
	uploadID := ""
	if commandNamespace == "" {
		uploadID, err = m.workerBroker.PrepareUpload(identity, destination, remaining)
	} else {
		maxBytes, limitErr := m.artifactBudgetLimit(jobID)
		if limitErr != nil {
			return "", workergateway.CollectResult{}, ArtifactBudget{}, limitErr
		}
		uploadID, err = m.workerBroker.PrepareUploadID(
			identity, stablePhaseUUID(commandNamespace, "upload", clean),
			destination, maxBytes,
		)
	}
	if err != nil {
		return "", workergateway.CollectResult{}, ArtifactBudget{}, err
	}
	request := workergateway.CollectRequest{
		LocalJobID: remoteJobID, Relative: rel, UploadID: uploadID,
	}
	var result workergateway.CollectResult
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	if commandNamespace == "" {
		err = m.workerBroker.Dispatch(
			ctx, identity, workergateway.ActionCollect, request, &result,
		)
	} else {
		err = m.workerBroker.DispatchID(
			ctx, identity, stablePhaseUUID(commandNamespace, "collect", clean),
			workergateway.ActionCollect, request, &result,
		)
	}
	cancel()
	if err != nil {
		return "", result, ArtifactBudget{}, fmt.Errorf("stream artifact %q: %w", rel, err)
	}
	if err := m.materializeGatewayArtifact(jobID, clean, destination); err != nil {
		return "", result, ArtifactBudget{}, err
	}
	info, err := os.Stat(destination)
	if err != nil || info.Size() != result.Size || !sha256DigestRegex.MatchString(result.SHA256) {
		return "", result, ArtifactBudget{},
			fmt.Errorf("streamed artifact %q failed size/digest attestation", rel)
	}
	records, err := artifactMetadata(
		stagingRoot, []string{clean}, &BuildStatus{}, "collecting",
	)
	if err != nil || len(records) != 1 || records[0].Digest != result.SHA256 ||
		records[0].SizeBytes != result.Size {
		return "", result, ArtifactBudget{},
			fmt.Errorf("streamed artifact %q differs from its gateway digest", rel)
	}
	next := append(append([]string(nil), collected...), clean)
	budget, err := m.setArtifactGeneration(jobID, "collected", stagingRoot, next)
	if err != nil {
		return "", result, ArtifactBudget{},
			fmt.Errorf("account streamed artifact %q: %w", rel, err)
	}
	return clean, result, budget, nil
}

// artifactMatchesPackage reports whether rel (e.g.
// "app-misc/jq-1.8.1-1.gpkg.tar") is the binpkg of the requested atom
// ("app-misc/jq") rather than a dependency that happened to be built.
func artifactMatchesPackage(rel, packageName string) bool {
	category, pn := "", packageName
	if i := strings.LastIndex(packageName, "/"); i >= 0 {
		category, pn = packageName[:i], packageName[i+1:]
	}
	base := rel
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if !strings.HasPrefix(base, pn+"-") || len(base) <= len(pn)+1 {
		return false
	}
	if c := base[len(pn)+1]; c < '0' || c > '9' {
		return false // "jq-extras-1.0" must not match "jq"
	}
	if category != "" && strings.Contains(rel, "/") && !strings.HasPrefix(rel, category+"/") {
		return false
	}
	return true
}

// artifactRelCPV extracts category/PF from Portage's category-preserving GPKG
// layout: category/package/PF-BUILD_ID.gpkg.tar. The exact CPV lets native
// verification remove only this VDB entry, never a similarly prefixed package.
func artifactRelCPV(rel string) string {
	parts := strings.Split(strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return ""
		}
	}
	base := parts[len(parts)-1]
	category := ""
	switch {
	case strings.HasSuffix(base, ".gpkg.tar"):
		if len(parts) < 3 {
			return ""
		}
		category = parts[len(parts)-3]
		base = strings.TrimSuffix(base, ".gpkg.tar")
		if cut := strings.LastIndex(base, "-"); cut > 0 {
			buildID := base[cut+1:]
			if buildID != "" && strings.IndexFunc(buildID, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
				base = base[:cut]
			}
		}
	case strings.HasSuffix(base, ".tbz2"):
		category = parts[len(parts)-2]
		base = strings.TrimSuffix(base, ".tbz2")
	default:
		return ""
	}
	cpv := category + "/" + base
	if !atomPattern.MatchString(cpv) {
		return ""
	}
	return cpv
}

func (m *Manager) downloadArtifactRelToRoot(
	root, baseURL, remoteJobID, rel string,
	mode os.FileMode,
	maxBytes int64,
) (string, string, error) {
	clean := path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if clean == "." || strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") {
		return "", "", fmt.Errorf("invalid artifact path %q", rel)
	}

	url := fmt.Sprintf("%s/api/v1/artifacts/download/%s?path=%s", baseURL, remoteJobID, neturl.QueryEscape(rel))
	httpReq, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	setBuilderAuth(httpReq, m.config.BuilderToken)

	resp, err := artifactHTTPClient.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("download artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("artifact download returned %d: %s", resp.StatusCode, string(body))
	}
	if err := validateArtifactContentLength(resp.ContentLength, maxBytes); err != nil {
		return "", "", err
	}

	dest := filepath.Join(root, filepath.FromSlash(clean))
	destDir := filepath.Dir(dest)
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return "", "", fmt.Errorf("create artifact directory: %w", err)
	}
	tmp, err := os.CreateTemp(destDir, ".artifact-*")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	if _, err := copyArtifactWithLimit(tmp, resp.Body, maxBytes); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("write artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", "", err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		_ = os.Remove(tmpName)
		return "", "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return "", "", err
	}
	return dest, clean, nil
}

func (m *Manager) fetchArtifactToBinhost(baseURL, remoteJobID, packageName, remoteArtifact string) (string, string, error) {
	if m.config.BinpkgPath == "" {
		return "", "", fmt.Errorf("BINPKG_PATH is not configured")
	}
	dest, rel, err := m.downloadArtifactToRoot(
		m.config.BinpkgPath, baseURL, remoteJobID, packageName,
		remoteArtifact, 0o644, maxPullArtifactBytes,
	)
	if err != nil {
		return "", "", err
	}
	if m.onArtifactStored != nil {
		m.onArtifactStored()
	}
	return dest, "/binpkgs/" + rel, nil
}

func (m *Manager) downloadArtifactToRoot(
	root, baseURL, remoteJobID, packageName, remoteArtifact string,
	mode os.FileMode,
	maxBytes int64,
) (string, string, error) {
	url := fmt.Sprintf("%s/api/v1/artifacts/download/%s", baseURL, remoteJobID)
	httpReq, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	setBuilderAuth(httpReq, m.config.BuilderToken)

	resp, err := artifactHTTPClient.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("download artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("artifact download returned %d: %s", resp.StatusCode, string(body))
	}
	if err := validateArtifactContentLength(resp.ContentLength, maxBytes); err != nil {
		return "", "", err
	}

	filename := artifactFilename(resp.Header.Get("Content-Disposition"), remoteArtifact)
	if filename == "" {
		return "", "", fmt.Errorf("cannot determine artifact filename")
	}

	// PKGDIR layout is <category>/<PF>.gpkg.tar; the category comes from the
	// validated package atom.
	category := ""
	if i := strings.IndexByte(packageName, '/'); i > 0 {
		category = packageName[:i]
	}
	destDir := filepath.Join(root, category)
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return "", "", fmt.Errorf("create artifact directory: %w", err)
	}

	// Download to a temp file and rename, so a concurrent index scan never
	// sees a half-written package.
	tmp, err := os.CreateTemp(destDir, ".artifact-*")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	if _, err := copyArtifactWithLimit(tmp, resp.Body, maxBytes); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("write artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", "", err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		_ = os.Remove(tmpName)
		return "", "", err
	}
	dest := filepath.Join(destDir, filename)
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return "", "", err
	}

	rel := filename
	if category != "" {
		rel = category + "/" + filename
	}
	return dest, filepath.ToSlash(rel), nil
}

func validateArtifactContentLength(contentLength, maxBytes int64) error {
	if maxBytes <= 0 {
		return fmt.Errorf("artifact byte limit must be positive")
	}
	if contentLength > maxBytes {
		return fmt.Errorf(
			"artifact content length %d exceeds remaining quarantine budget %d",
			contentLength, maxBytes,
		)
	}
	return nil
}

func copyArtifactWithLimit(destination io.Writer, source io.Reader, maxBytes int64) (int64, error) {
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return 0, fmt.Errorf("artifact byte limit is invalid")
	}
	written, err := io.Copy(destination, io.LimitReader(source, maxBytes+1))
	if err != nil {
		return written, err
	}
	if written > maxBytes {
		return written, fmt.Errorf("artifact exceeds remaining quarantine budget %d", maxBytes)
	}
	return written, nil
}

// artifactFilename extracts a safe filename from a Content-Disposition header,
// falling back to the basename of the builder-side artifact path.
func artifactFilename(disposition, remoteArtifact string) string {
	if disposition != "" {
		if _, params, err := mime.ParseMediaType(disposition); err == nil {
			if name := filepath.Base(params["filename"]); name != "." && name != string(filepath.Separator) && !strings.HasPrefix(name, ".") {
				return name
			}
		}
	}
	if name := filepath.Base(remoteArtifact); remoteArtifact != "" && name != "." && name != string(filepath.Separator) && !strings.HasPrefix(name, ".") {
		return name
	}
	return ""
}

// authHeader sets the shared builder token on an outbound request when configured.
func setBuilderAuth(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("X-API-Key", token)
	}
}

// getFromBuilder issues an authenticated GET to a builder endpoint.
func (m *Manager) getFromBuilder(url string) (*http.Response, error) {
	return m.builderGet(builderHTTPClient, url)
}

// builderGet issues an authenticated GET using the supplied client.
func (m *Manager) builderGet(client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setBuilderAuth(req, m.config.BuilderToken)
	return client.Do(req)
}
