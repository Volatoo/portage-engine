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
	"mime"
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

	"github.com/slchris/portage-engine/internal/binpkg"
	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/iac"
	"github.com/slchris/portage-engine/internal/signing"
	"github.com/slchris/portage-engine/pkg/config"
)

// BuildRequest represents a package build request.
type BuildRequest struct {
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
	Authority          string     `json:"authority"`
	QueuedTasks        int        `json:"queued_tasks"`
	RunningTasks       int        `json:"running_tasks"`
	ActiveLeases       int        `json:"active_leases"`
	ExpiredLeases      int        `json:"expired_leases"`
	RegisteredWorkers  int        `json:"registered_workers"`
	ActiveWorkers      int        `json:"active_workers"`
	StaleWorkers       int        `json:"stale_workers"`
	AttemptsLastHour   int        `json:"attempts_last_hour"`
	OldestQueuedAt     *time.Time `json:"oldest_queued_at,omitempty"`
	OldestLeaseExpires *time.Time `json:"oldest_lease_expires_at,omitempty"`
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
	ClaimNext(context.Context, string, time.Duration) (*SchedulerClaim, error)
	RenewClaim(context.Context, *BuildStatus, time.Duration) error
	CheckClaim(context.Context, *BuildStatus) error
	LoadVisible(context.Context) (map[string]*BuildStatus, error)
	CancelJob(context.Context, string, string) (*BuildStatus, error)
	RetryJob(context.Context, string) (*BuildStatus, error)
	RuntimeStatus(context.Context) (SchedulerRuntimeStatus, error)
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
	metadata     RuntimeMetadataLedger
	infraCleanup InfraCleanupLedger
	promotionDB  ArtifactPromotionLedger
	signing      signing.Coordinator
	logLedger    DurableLogLedger
	stopCh       chan struct{}
	shutdownOnce sync.Once
	workersOnce  sync.Once
	cleanupOnce  sync.Once
	stopped      bool // guarded by submitMu
	schedulerID  string
	wakeHook     func()
	eventHook    func(BuildStatus)

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

// SetGPGKeyProvider registers the isolated signer's public identity for
// verification and mirror publication. It is never used during deployment.
func (m *Manager) SetGPGKeyProvider(f func() (string, []byte, []byte)) {
	m.gpgKeyProvider = f
}

// NewManager creates a new build manager.
func NewManager(cfg *config.ServerConfig) *Manager {
	var iacOpts []iac.ManagerOption
	if cfg.CloudInstanceTTL > 0 {
		iacOpts = append(iacOpts, iac.WithDefaultTTL(time.Duration(cfg.CloudInstanceTTL)*time.Minute))
	}
	if cfg.DataDir != "" {
		iacOpts = append(iacOpts, iac.WithWorkspaceDir(filepath.Join(cfg.DataDir, "terraform-workspaces")))
		if !cfg.Database.Enabled {
			// Standalone compatibility mode has no PostgreSQL cleanup queue.
			// Persist the local instance map for restart recovery.
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
		schedulerID:  "control-plane/" + controlPlaneID,
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

// SetJobLedger installs the PostgreSQL job authority before durable jobs are
// projected into the manager. A nil ledger preserves standalone memory+JSON
// compatibility.
func (m *Manager) SetJobLedger(ledger JobLedger) {
	m.jobLedger = ledger
	if scheduler, ok := ledger.(DurableScheduler); ok {
		m.scheduler = scheduler
	}
	if metadata, ok := ledger.(RuntimeMetadataLedger); ok {
		m.metadata = metadata
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
		marker, markerErr := os.ReadFile(filepath.Join(root, verificationCapabilityFile))
		if markerErr == nil {
			expiry, parseErr := strconv.ParseInt(strings.TrimSpace(string(marker)), 10, 64)
			if parseErr != nil || now.Unix() >= expiry {
				_ = os.RemoveAll(root)
			}
			continue
		}
		info, statErr := entry.Info()
		if statErr == nil && now.Sub(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(root)
		}
	}
}

// SetEphemeralHooks connects optional Redis acceleration. These callbacks must
// never be used to decide build correctness; PostgreSQL polling remains active.
func (m *Manager) SetEphemeralHooks(wake func(), event func(BuildStatus)) {
	m.wakeHook = wake
	m.eventHook = event
}

// StartWorkers starts the configured executor pool after the server has loaded
// its catalog, signing state, and persisted projection.
func (m *Manager) StartWorkers() {
	m.workersOnce.Do(func() {
		for i := 0; i < m.config.MaxWorkers; i++ {
			go m.worker(i)
		}
	})
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
		}
		next[id] = &copyStatus
	}
	m.jobs = next
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
	m.StartWorkers()
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
		claim, err := m.scheduler.ClaimNext(ctx, workerName, 30*time.Second)
		cancel()
		if err == nil && claim != nil {
			m.jobsMu.Lock()
			m.jobs[claim.Status.JobID] = claim.Status
			m.jobsMu.Unlock()

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

	// Stream provisioning/deployment progress into the job's live log so the
	// dashboard's logs page can be used to follow and debug the whole flow.
	// The first "[deploy]" line flips the job status to "deploying" so the
	// status shown in the UI tracks the pipeline stage.
	var deployingOnce sync.Once
	provReq.LogSink = func(line string) {
		m.appendJobLog(jobID, line)
		if strings.HasPrefix(line, "[deploy]") {
			deployingOnce.Do(func() { m.updateStatus(jobID, "deploying", "", "") })
		}
	}
	var infraFence atomic.Pointer[BuildStatus]
	provReq.Lifecycle = func(instance *iac.Instance, state, failure string, deletedAt *time.Time) error {
		if captured := infraFence.Load(); captured != nil {
			return m.recordInfraWithStatus(captured, instance, state, failure, deletedAt)
		}
		return m.recordInfra(jobID, instance, state, failure, deletedAt)
	}

	// Every build receives a fresh native Gentoo instance/root. There is no
	// warm-pool path: emerge mutates VDB, installed files and arbitrary ebuild
	// post-install state.
	m.updateStatus(jobID, "provisioning", "", "")
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
		var termErr error
		if !pipelineClean {
			m.appendJobLog(jobID, fmt.Sprintf("[cleanup] destroying tainted instance %s after pipeline failure", instance.ID))
			if termErr = m.iacMgr.Terminate(instance.ID); termErr != nil {
				m.appendJobLog(jobID, fmt.Sprintf("[cleanup] destroy failed; cleanup routine will retry: %v", termErr))
			}
		} else {
			m.appendJobLog(jobID, fmt.Sprintf("[cleanup] destroying single-use native instance %s after successful publication", instance.ID))
			if termErr = m.iacMgr.Terminate(instance.ID); termErr != nil {
				m.appendJobLog(jobID, fmt.Sprintf("[cleanup] destroy failed; cleanup routine will retry: %v", termErr))
			}
		}
		state := "destroyed"
		var deletedAt *time.Time
		if termErr != nil {
			state = "destroy_failed"
		} else {
			now := time.Now().UTC()
			deletedAt = &now
		}
		if err := m.recordInfraWithStatus(cleanupFence, instance, state, errorString(termErr), deletedAt); err != nil {
			m.appendJobLog(jobID, "[cleanup] warning: durable infra result was not recorded: "+err.Error())
		}
	}()

	if instance.BuilderEndpoint == "" {
		pipelineClean = false
		m.updateStatus(jobID, "failed", instance.ID, "provisioned instance has no builder endpoint")
		return
	}

	// The builder service is (re)started at the very end of the deploy script;
	// on a fast deploy (native Gentoo) it may not have bound its port yet when
	// deploy returns. Wait for /health before submitting, so the first build
	// doesn't race the builder startup with a connection-refused.
	if !m.waitForBuilderReady(jobID, instance) {
		pipelineClean = false
		m.setFailedStage(jobID, "deploy")
		m.updateStatus(jobID, "failed", instance.ID, "builder did not become ready after deployment")
		return
	}

	m.updateStatus(jobID, "building", instance.ID, "")
	m.appendJobLog(jobID, "[build] submitting build to the instance builder…")

	// Submit and wait for the build on the builder, then pull the resulting
	// artifact back to the server's binpkg dir.
	if err := m.runBuildOnInstance(jobID, instance, req); err != nil {
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
		m.revokeArtifactCapability(jobID)
		m.updateStatus(jobID, "signing", instanceID, "")
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

	m.updateStatus(jobID, "publishing", instanceID, "")
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
	m.updateStatus(jobID, "completed", instanceID, "")
	return "", nil
}

func (m *Manager) revokeArtifactCapability(jobID string) {
	m.jobsMu.RLock()
	job := m.jobs[jobID]
	root := ""
	if job != nil {
		root = job.StagingRoot
	}
	m.jobsMu.RUnlock()
	if root != "" {
		_ = os.Remove(filepath.Join(root, verificationCapabilityFile))
	}
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
	if sourceRoot == "" || !capabilityTokenRegex.MatchString(sourceToken) || len(rels) == 0 {
		return fmt.Errorf("job quarantine is incomplete")
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
		SourceToken: sourceToken, Architecture: status.Arch, Artifacts: inputs,
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
	deadline := time.Now().Add(waitTimeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("signing task %s did not finish within %s", task.ID, waitTimeout)
		}
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		current, getErr := m.signing.GetSigningTask(ctx, task.ID)
		cancel()
		if getErr != nil {
			return getErr
		}
		switch current.State {
		case "completed":
			return m.adoptSignedArtifacts(jobID, sourceRoot, current)
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

func (m *Manager) adoptSignedArtifacts(jobID, sourceRoot string, task *signing.Task) error {
	outputToken, err := signing.OutputToken(task.ID)
	if err != nil {
		return err
	}
	outputRoot, err := signing.TokenRoot(m.config.BinpkgPath, outputToken)
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
		records, err := artifactMetadata(outputRoot, []string{artifact.RelativePath}, &BuildStatus{}, "signed")
		if err != nil {
			return err
		}
		if records[0].Digest != artifact.OutputDigest || records[0].SizeBytes != artifact.OutputSize {
			return fmt.Errorf("signed artifact %q does not match signer output digest", artifact.RelativePath)
		}
	}
	m.jobsMu.Lock()
	job, ok := m.jobs[jobID]
	if ok {
		job.StagingRoot = outputRoot
		job.VerificationToken = outputToken
		job.StagedArtifacts = rels
		job.Signed = true
	}
	m.jobsMu.Unlock()
	if !ok {
		return fmt.Errorf("job %s disappeared while adopting signer output", jobID)
	}
	if sourceRoot != outputRoot {
		_ = os.RemoveAll(sourceRoot)
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
	if err := m.scheduler.RenewClaim(ctx, status, 30*time.Second); err != nil {
		return err
	}
	return m.scheduler.CheckClaim(ctx, status)
}

// verifyOnBuilder asks a builder to install the just-built package from the
// job-private verification binhost in a throwaway native Portage root.
func (m *Manager) verifyOnBuilder(jobID, builderEndpoint, instanceID string, req *BuildRequest, binhostURL string) error {
	m.updateStatus(jobID, "verifying", instanceID, "")

	signed := false
	stagingRoot := ""
	var rels, builtPackages []string
	m.jobsMu.RLock()
	if job, ok := m.jobs[jobID]; ok {
		signed = job.Signed
		stagingRoot = job.StagingRoot
		rels = append(rels, job.StagedArtifacts...)
		for _, rel := range rels {
			if cpv := artifactRelCPV(rel); cpv != "" && !slices.Contains(builtPackages, cpv) {
				builtPackages = append(builtPackages, cpv)
			}
		}
	}
	m.jobsMu.RUnlock()
	if stagingRoot == "" || len(rels) == 0 {
		return fmt.Errorf("verification generation is missing staged artifacts")
	}

	generation := "unsigned"
	if signed {
		generation = "signed"
	}
	records, err := artifactMetadata(stagingRoot, rels, &BuildStatus{}, "verifying_"+generation)
	if err != nil {
		return fmt.Errorf("bind %s verification generation: %w", generation, err)
	}
	artifacts := make([]VerificationArtifact, len(records))
	for index := range records {
		artifacts[index] = VerificationArtifact{
			RelativePath: filepath.ToSlash(rels[index]),
			SHA256:       records[index].Digest,
			Size:         records[index].SizeBytes,
		}
	}

	keyID, pubkey := "", ""
	if signed {
		if m.gpgKeyProvider == nil {
			return fmt.Errorf("signed verification requires an isolated-signer public-key provider; refusing unsigned downgrade")
		}
		var public []byte
		keyID, public, _ = m.gpgKeyProvider()
		if keyID == "" || len(public) == 0 {
			return fmt.Errorf("signed verification requires a ready signer public key; refusing unsigned downgrade")
		}
		pubkey = string(public)
	}
	m.appendJobLog(jobID, fmt.Sprintf(
		"[verify] generation=%s signature_required=%t key_id=%s artifacts=%d digests=%s",
		generation, signed, keyID, len(artifacts), verificationDigestSummary(artifacts)))
	m.appendJobLog(jobID, fmt.Sprintf(
		"[verify] installing %s from the digest-bound %s job-private generation in a fresh PKGDIR and throwaway native root…",
		req.PackageName, generation))

	baseURL := normalizeBuilderURL(builderEndpoint)
	body, _ := json.Marshal(VerifyInstallRequest{
		PackageName:      req.PackageName,
		BinhostURL:       binhostURL,
		Generation:       generation,
		GPGPubkey:        pubkey,
		ExpectedKeyID:    keyID,
		BuiltPackages:    builtPackages,
		Artifacts:        artifacts,
		RequireSignature: signed,
	})
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

	var result struct {
		OK    bool   `json:"ok"`
		Log   string `json:"log"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("verification response invalid (status %d): %w", resp.StatusCode, err)
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

// verifyUnsignedRejectedOnBuilder is a mandatory negative control for signed
// pipelines. It submits the exact unsigned generation under the signed policy
// and requires the builder's independent GPKG verifier to reject it before the
// isolated signer is allowed to run.
func (m *Manager) verifyUnsignedRejectedOnBuilder(jobID, builderEndpoint string, req *BuildRequest, binhostURL string) error {
	m.jobsMu.RLock()
	job, ok := m.jobs[jobID]
	if !ok {
		m.jobsMu.RUnlock()
		return fmt.Errorf("job %s not found", jobID)
	}
	if job.Signed {
		m.jobsMu.RUnlock()
		return fmt.Errorf("negative control requires an unsigned generation")
	}
	root := job.StagingRoot
	rels := append([]string(nil), job.StagedArtifacts...)
	var builtPackages []string
	for _, rel := range rels {
		if cpv := artifactRelCPV(rel); cpv != "" && !slices.Contains(builtPackages, cpv) {
			builtPackages = append(builtPackages, cpv)
		}
	}
	m.jobsMu.RUnlock()
	if root == "" || len(rels) == 0 {
		return fmt.Errorf("unsigned generation is missing staged artifacts")
	}
	if m.gpgKeyProvider == nil {
		return fmt.Errorf("negative control requires an isolated-signer public-key provider")
	}
	keyID, public, _ := m.gpgKeyProvider()
	if keyID == "" || len(public) == 0 {
		return fmt.Errorf("negative control requires a ready signer public key")
	}
	records, err := artifactMetadata(root, rels, &BuildStatus{}, "negative_control")
	if err != nil {
		return err
	}
	artifacts := make([]VerificationArtifact, len(records))
	for index := range records {
		artifacts[index] = VerificationArtifact{
			RelativePath: filepath.ToSlash(rels[index]),
			SHA256:       records[index].Digest,
			Size:         records[index].SizeBytes,
		}
	}
	body, _ := json.Marshal(VerifyInstallRequest{
		PackageName: req.PackageName, BinhostURL: binhostURL,
		Generation: "signed", GPGPubkey: string(public), ExpectedKeyID: keyID,
		BuiltPackages: builtPackages, Artifacts: artifacts, RequireSignature: true,
	})
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
		sub := ""
		if j := strings.LastIndex(rels[i], "/"); j >= 0 {
			sub = rels[i][:j]
		}
		url, err := up.uploadLocalFile(local, sub)
		if err != nil {
			return fmt.Errorf("upload %s: %w", rels[i], err)
		}
		m.appendJobLog(jobID, "[collect] uploaded to mirror: "+url)
	}
	if m.config.BinpkgPath != "" {
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
	if provider == "" {
		return nil, fmt.Errorf("no remote builders configured and no cloud provider set (set REMOTE_BUILDERS or CLOUD_DEFAULT_PROVIDER)")
	}
	if cs.SSHKeyPath == "" {
		return nil, fmt.Errorf("cloud build requires CLOUD_SSH_KEY_PATH so the builder can be deployed to the instance")
	}
	if cs.ServerCallbackURL == "" {
		return nil, fmt.Errorf("cloud build requires SERVER_CALLBACK_URL so the deployed builder can reach this server")
	}
	if cs.BuilderBinaryPath == "" && cs.BuilderBinaryURL == "" {
		// Not fatal: the image/template may ship the builder preinstalled. But
		// if it does not, the instance will never start a builder service.
		fmt.Println("Warning: neither CLOUD_BUILDER_BINARY_PATH nor CLOUD_BUILDER_BINARY_URL is set; " +
			"the instance can only build if its image ships /opt/portage-builder/portage-builder")
	}
	if cs.BuilderBinaryPath == "" && cs.BuilderBinaryURL != "" {
		builderURL, err := neturl.Parse(cs.BuilderBinaryURL)
		if err != nil || (builderURL.Scheme != "http" && builderURL.Scheme != "https") || builderURL.Host == "" {
			return nil, fmt.Errorf("CLOUD_BUILDER_BINARY_URL must be an absolute http or https URL")
		}
		if builderURL.User != nil || builderURL.RawQuery != "" || builderURL.Fragment != "" {
			return nil, fmt.Errorf("CLOUD_BUILDER_BINARY_URL must not contain credentials, query parameters, or a fragment")
		}
		if !sha256DigestRegex.MatchString(cs.BuilderBinarySHA256) {
			return nil, fmt.Errorf("CLOUD_BUILDER_BINARY_SHA256 must be exactly 64 lowercase hexadecimal characters when downloading the builder")
		}
	}

	ttl := time.Duration(cs.InstanceTTLMinutes) * time.Minute

	// Point the deployed builder's Portage at a binhost for dependency reuse.
	// Prefer the internal mirror when uploads are configured: build nodes are
	// on the mirror's LAN (the central server may be on a different subnet the
	// nodes cannot route to), and the mirror already serves the signed
	// binpkgs the server publishes there.
	binhost := ""
	binhostPath, err := buildBinhostPath(req)
	if err != nil {
		return nil, err
	}
	if cs.ServerCallbackURL != "" {
		binhost = strings.TrimRight(cs.ServerCallbackURL, "/") + "/binpkgs/" + binhostPath
	}
	if cs.UploadURL != "" {
		dir := strings.Trim(cs.UploadDir, "/")
		if dir == "" {
			dir = "portage-engine"
		}
		binhost = strings.TrimRight(cs.UploadURL, "/") + "/local/" + dir + "/" + binhostPath
	}

	spec := req.MachineSpec
	buildMode := "native-gentoo"
	if req.ResolvedContext != nil && req.ResolvedContext.BuildMode != "" {
		buildMode = req.ResolvedContext.BuildMode
	}
	if buildMode != "native-gentoo" {
		return nil, fmt.Errorf("build mode %q is no longer supported; only native-gentoo is available", buildMode)
	}
	switch provider {
	case "pve":
		spec = pveSpecWithDefaults(cs, req.MachineSpec, buildMode)
		if req.ResolvedContext != nil && req.ResolvedContext.Template != "" {
			// The catalog, never machine_spec, owns template selection.
			spec["template"] = req.ResolvedContext.Template
		}
	case "gcp":
		spec = gcpSpecWithDefaults(cs, req.MachineSpec)
	case "aws":
		spec = awsSpecWithDefaults(cs, req.MachineSpec)
	}
	if req.ResolvedContext != nil {
		// Persist the catalog identity with the IaC instance so provisioning,
		// cleanup and audit evidence remain bound to the exact image/profile
		// generation used by this single-use VM.
		spec["pe_catalog_version"] = strconv.Itoa(req.ResolvedContext.CatalogVersion)
		spec["pe_profile_id"] = req.ResolvedContext.ProfileID
		spec["pe_image_id"] = req.ResolvedContext.ImageID
		spec["pe_image_generation"] = req.ResolvedContext.ImageGeneration
		spec["pe_image_digest"] = req.ResolvedContext.ImageDigest
		spec["pe_mirror_bundle_id"] = req.ResolvedContext.MirrorBundleID
		spec["pe_mirror_bundle_digest"] = req.ResolvedContext.MirrorBundleDigest
	}

	preq := &iac.ProvisionRequest{
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
	}
	return preq, nil
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

	remoteJobID, err := m.postBuildToBuilder(baseURL, req)
	if err != nil {
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
		m.updateStatus(jobID, localStatus, instance.ID, snap.Error)

		if snap.Terminal {
			if snap.Status == "failed" {
				return fmt.Errorf("remote build failed: %s", snap.Error)
			}
			// Success: pull every produced package off the instance into the
			// central binhost BEFORE the VM can go away. The requested
			// package's own file becomes the job's primary artifact;
			// dependencies land alongside it (category preserved).
			if err := m.collectInstanceArtifacts(jobID, baseURL, remoteJobID, req.PackageName, snap); err != nil {
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
		UseFlags:      make(map[string]string),
		Environment:   make(map[string]string),
		ConfigBundle:  req.ConfigBundle,
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

	m.updateStatus(jobID, "forwarding", "", "")

	// Convert BuildRequest to LocalBuildRequest format
	localReq := LocalBuildRequest{
		PackageName:   req.PackageName, // Already in category/package format from server
		Version:       req.Version,
		Arch:          req.Arch,
		ProfileID:     req.ProfileID,
		RepositoryIDs: append([]string(nil), req.RepositoryIDs...),
		ResourceClass: req.ResourceClass,
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
		m.updateStatus(localJobID, localStatus, "", remoteJob.Error)

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
					if err := m.collectInstanceArtifacts(localJobID, baseURL, remoteJobID, req.PackageName, snap); err != nil {
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
func (m *Manager) updateStatus(jobID, status, instanceID, errorMsg string) {
	m.jobsMu.Lock()
	job, exists := m.jobs[jobID]
	if !exists {
		m.jobsMu.Unlock()
		return
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := m.scheduler.RecordTransition(ctx, &previous, &current)
		cancel()
		if err != nil {
			return
		}
		m.jobsMu.Lock()
		if live, ok := m.jobs[jobID]; ok &&
			live.AttemptID == previous.AttemptID &&
			live.FenceToken == previous.FenceToken &&
			live.Status == previous.Status {
			*live = current
		}
		m.jobsMu.Unlock()
		m.emitStatus(&current)
		return
	}
	*job = current
	m.jobsMu.Unlock()

	if m.jobLedger != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = m.jobLedger.RecordTransition(ctx, &previous, &current)
		cancel()
	}
	m.emitStatus(&current)
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
	if m.config.BinpkgPath == "" {
		return "", fmt.Errorf("BINPKG_PATH is not configured")
	}
	token := strings.ReplaceAll(uuid.NewString(), "-", "")
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
	return filepath.Join(filepath.Dir(filepath.Clean(m.config.BinpkgPath)), ".portage-engine-quarantine")
}

func (m *Manager) prepareVerificationBinhost(jobID, arch string) (string, error) {
	m.jobsMu.RLock()
	job, ok := m.jobs[jobID]
	if !ok {
		m.jobsMu.RUnlock()
		return "", fmt.Errorf("job %s not found", jobID)
	}
	root, token := job.StagingRoot, job.VerificationToken
	rels := append([]string(nil), job.StagedArtifacts...)
	m.jobsMu.RUnlock()
	if root == "" || token == "" || len(rels) == 0 {
		return "", fmt.Errorf("job %s has no staged artifacts", jobID)
	}
	if arch == "" {
		return "", fmt.Errorf("job %s has no architecture for its verification index", jobID)
	}
	if _, err := binpkg.NewStore(root).RegenerateIndex(arch); err != nil {
		return "", fmt.Errorf("generate quarantine Packages index: %w", err)
	}
	// The marker is the shared, revocable capability state. Any control-plane
	// replica mounting the same BINPKG_PATH can serve it; deleting the
	// quarantine directory revokes it everywhere.
	expiresAt := time.Now().UTC().Add(30 * time.Minute)
	if err := os.WriteFile(filepath.Join(root, verificationCapabilityFile),
		[]byte(strconv.FormatInt(expiresAt.Unix(), 10)), 0o600); err != nil {
		return "", fmt.Errorf("activate quarantine capability: %w", err)
	}

	callback := strings.TrimRight(m.CloudSettings().ServerCallbackURL, "/")
	parsed, err := neturl.Parse(callback)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("SERVER_CALLBACK_URL must be an absolute http or https URL for quarantined verification")
	}
	return callback + "/verify-binhost/" + token, nil
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
	root := filepath.Join(m.artifactQuarantineBase(), token)
	marker, err := os.ReadFile(filepath.Join(root, verificationCapabilityFile)) // #nosec G703 -- token is regex-validated and confined to the quarantine base.
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
	file := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(file) // #nosec G703 -- token and cleaned relative path are confined to the quarantine root above.
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, file) // #nosec G703 -- regular-file and quarantine-root confinement checks passed above.
}

func (m *Manager) cleanupArtifactQuarantine(jobID string) {
	m.jobsMu.Lock()
	job, ok := m.jobs[jobID]
	if !ok {
		m.jobsMu.Unlock()
		return
	}
	root := job.StagingRoot
	job.StagingRoot = ""
	job.VerificationToken = ""
	job.StagedArtifacts = nil
	job.StagedPrimary = ""
	m.jobsMu.Unlock()

	if root != "" {
		_ = os.RemoveAll(root)
	}
}

func (m *Manager) promoteJobArtifacts(jobID string) error {
	if m.promoteArtifacts == nil {
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
	arch := job.Arch
	status := *job
	m.jobsMu.RUnlock()
	binhostPath, err := buildStatusBinhostPath(&status)
	if err != nil {
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
	paths, err := m.promoteArtifacts(root, rels, arch, binhostPath)
	if err != nil {
		return err
	}
	if len(paths) != len(rels) {
		return fmt.Errorf("promotion returned %d paths for %d artifacts", len(paths), len(rels))
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
			return fmt.Errorf("persist published artifact metadata: %w", err)
		}
	}
	m.cleanupArtifactQuarantine(jobID)
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
		handle, err := os.Open(file)
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
func (m *Manager) collectInstanceArtifacts(jobID, baseURL, remoteJobID, packageName string, snap *remoteJobSnapshot) error {
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
		m.appendJobLog(jobID, "[collect] fetching artifact into job quarantine...")
		_, rel, err := m.downloadArtifactToRoot(stagingRoot, baseURL, remoteJobID, packageName, snap.ArtifactURL, 0o600)
		if err != nil {
			return err
		}
		m.appendJobLog(jobID, "[collect] quarantined artifact: "+rel)
		m.jobsMu.Lock()
		if job, ok := m.jobs[jobID]; ok {
			job.StagedPrimary = rel
			job.StagedArtifacts = []string{rel}
			job.Signed = snap.Signed
		}
		m.jobsMu.Unlock()
		success = true
		return nil
	}

	m.appendJobLog(jobID, fmt.Sprintf("[collect] fetching %d artifact(s) into job quarantine...", len(snap.Artifacts)))
	rels := make([]string, 0, len(snap.Artifacts))
	primary := ""
	for _, rel := range snap.Artifacts {
		_, clean, err := m.downloadArtifactRelToRoot(stagingRoot, baseURL, remoteJobID, rel, 0o600)
		if err != nil {
			return err
		}
		m.appendJobLog(jobID, "[collect] quarantined artifact: "+clean)
		rels = append(rels, clean)
		if artifactMatchesPackage(rel, packageName) {
			primary = clean
		}
	}
	if primary == "" && len(rels) > 0 {
		primary = rels[0]
	}
	m.jobsMu.Lock()
	if job, ok := m.jobs[jobID]; ok {
		job.StagedPrimary = primary
		job.StagedArtifacts = rels
		job.Signed = snap.Signed
	}
	m.jobsMu.Unlock()
	success = true
	return nil
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

func (m *Manager) downloadArtifactRelToRoot(root, baseURL, remoteJobID, rel string, mode os.FileMode) (string, string, error) {
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
	if _, err := io.Copy(tmp, resp.Body); err != nil {
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
	dest, rel, err := m.downloadArtifactToRoot(m.config.BinpkgPath, baseURL, remoteJobID, packageName, remoteArtifact, 0o644)
	if err != nil {
		return "", "", err
	}
	if m.onArtifactStored != nil {
		m.onArtifactStored()
	}
	return dest, "/binpkgs/" + rel, nil
}

func (m *Manager) downloadArtifactToRoot(root, baseURL, remoteJobID, packageName, remoteArtifact string, mode os.FileMode) (string, string, error) {
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
	if _, err := io.Copy(tmp, resp.Body); err != nil {
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
