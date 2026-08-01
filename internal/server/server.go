// Package server implements the core Portage Engine server functionality.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/internal/binpkg"
	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/iam"
	"github.com/slchris/portage-engine/internal/metrics"
	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/internal/runtimecache"
	artifactstorage "github.com/slchris/portage-engine/internal/storage"
	"github.com/slchris/portage-engine/internal/workergateway"
	"github.com/slchris/portage-engine/pkg/config"
)

// Version information (set via -ldflags at build time).
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// Server represents the Portage Engine server.
type Server struct {
	config               *config.ServerConfig
	binpkgRoot           string
	binpkgStore          *binpkg.Store
	binpkgMu             sync.RWMutex
	binpkgStores         map[string]*binpkg.Store
	binhostProfiles      map[string]binhostProfile
	defaultBinhost       string
	builder              *builder.Manager
	builderRegistry      *builder.Registry
	metrics              *metrics.Metrics
	startTime            time.Time
	store                *ServerStore
	persister            *ServerPersister
	database             *persistence.Database
	jobLedger            *persistence.JobRepository
	iamRepository        *persistence.IAMRepository
	deviceAuthorizations deviceAuthorizationStore
	oidcVerifier         iam.Verifier
	identityVerifiers    map[string]iam.IdentityVerifier
	oidcVerifiers        map[string]iam.Verifier
	providerIssuers      map[string]string
	providerConfigs      map[string]config.IdentityProviderConfig
	identityAdmins       map[string]struct{}
	databaseInitErr      string
	cache                *runtimecache.Client
	cacheInitErr         string
	cacheStop            context.CancelFunc
	cacheWG              sync.WaitGroup
	artifactStorage      artifactstorage.Storage
	artifactStorageMu    sync.RWMutex
	artifactStorageErr   string
	publicStatusMu       sync.Mutex
	publicStatusCache    publicServiceStatus
	publicStatusUntil    time.Time
	ledgerStop           chan struct{}
	ledgerWG             sync.WaitGroup
	ledgerPruneOnce      sync.Once
	ledgerStaleAfter     time.Duration
	binhostStop          chan struct{}
	trustedProxies       []netip.Prefix
	settingsMu           sync.Mutex // serializes settings updates + persistence
}

// trustedProxyPrefixes parses TRUSTED_PROXY_CIDRS once. A malformed entry is
// dropped rather than widened (Validate() warns about it), so a typo can never
// make the process believe a client-supplied X-Forwarded-For.
func trustedProxyPrefixes(cidrs []string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, raw := range cidrs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes
}

// New creates a new Server instance.
func New(cfg *config.ServerConfig) *Server {
	metricsCfg := &metrics.Config{
		Enabled:  cfg.MetricsEnabled,
		Port:     cfg.MetricsPort,
		Password: cfg.MetricsPassword,
	}

	s := &Server{
		config:          cfg,
		binpkgRoot:      cfg.BinpkgPath,
		binpkgStore:     binpkg.NewStore(cfg.BinpkgPath),
		binpkgStores:    make(map[string]*binpkg.Store),
		binhostProfiles: make(map[string]binhostProfile),
		builder:         builder.NewManager(cfg),
		builderRegistry: builder.NewRegistry(60*time.Second, 30*time.Second),
		metrics:         metrics.New(metricsCfg),
		startTime:       time.Now(),
		trustedProxies:  trustedProxyPrefixes(cfg.TrustedProxyCIDRs),
	}
	s.metrics.SetSchedulerProvider(s.schedulerMetricsSnapshot)

	// When a build's artifact lands in the binhost PKGDIR, refresh the
	// Packages index right away so clients see the new package without
	// waiting for the periodic refresher.
	s.builder.SetArtifactStoredHook(func() {
		s.refreshBinhostIndexes("artifact ingest")
	})
	s.builder.SetArtifactPromotionHook(func(stagingRoot string, rels []string, arch, binhostPath string) ([]string, error) {
		store, profile, ok := s.binhostStoreByPath(binhostPath)
		if !ok {
			return nil, fmt.Errorf("binhost path %q is not registered by the active catalog", binhostPath)
		}
		if profile.Arch != arch {
			return nil, fmt.Errorf("binhost path %q is registered for %s, not %s", binhostPath, profile.Arch, arch)
		}
		return store.PromoteStaged(stagingRoot, rels, arch)
	})

	return s
}

func metricInt64(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

func metricMap(value any) map[string]any {
	if result, ok := value.(map[string]any); ok {
		return result
	}
	return nil
}

func metricBool(value any) bool {
	result, _ := value.(bool)
	return result
}

func capacityPoolMetricCounts(value any) (int64, int64) {
	pools, ok := value.([]any)
	if !ok {
		return 0, 0
	}
	var blocked int64
	for _, value := range pools {
		pool := metricMap(value)
		if metricInt64(pool["unschedulable_backlog"]) > 0 {
			blocked++
		}
	}
	return int64(len(pools)), blocked
}

func targetHistoryMetricCounts(value any) (
	samples, successes, failures, breaches, reserved, charged int64,
) {
	history := metricMap(value)
	targets, ok := history["targets"].([]any)
	if !ok {
		return
	}
	for _, targetValue := range targets {
		target := metricMap(targetValue)
		windows, windowsOK := target["windows"].([]any)
		if !windowsOK {
			continue
		}
		for _, windowValue := range windows {
			window := metricMap(windowValue)
			if name, _ := window["name"].(string); name != "30d" {
				continue
			}
			samples += metricInt64(window["samples"])
			successes += metricInt64(window["successes"])
			failures += metricInt64(window["failures"])
			reserved += metricInt64(window["reserved_cost_microunits"])
			charged += metricInt64(window["charged_cost_microunits"])
			insufficient, _ := window["insufficient_data"].(bool)
			sloMet, _ := window["slo_met"].(bool)
			if !insufficient && !sloMet {
				breaches++
			}
		}
	}
	return
}

func (s *Server) schedulerMetricsSnapshot() metrics.SchedulerSnapshot {
	snapshot := schedulerMetricsSnapshotFromStatus(s.builder.GetSchedulerStatus())
	if s.jobLedger == nil {
		return snapshot
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	freshness := time.Duration(s.config.DistCCWorkerFreshnessSeconds) * time.Second
	if freshness <= 0 {
		freshness = 45 * time.Second
	}
	compile, err := s.jobLedger.CompileMetrics(ctx, freshness, time.Hour)
	cancel()
	if err == nil {
		snapshot.DistCCWorkersFresh = compile.WorkersFresh
		snapshot.DistCCSlotsTotal = compile.SlotsTotal
		snapshot.DistCCSlotsLeased = compile.SlotsLeased
		snapshot.DistCCLocalCompiles = compile.LocalCompiles
		snapshot.DistCCRemoteCompiles = compile.RemoteCompiles
		snapshot.DistCCHits = compile.Hits
		snapshot.DistCCFallbacks = compile.Fallbacks
		snapshot.DistCCNetworkBytes = compile.NetworkBytes
		snapshot.DistCCQueueMillis = compile.QueueMillis
		snapshot.DistCCFailures = compile.FailuresByReason
	}
	censusCtx, cancelCensus := context.WithTimeout(
		context.Background(), 2*time.Second,
	)
	totals, err := s.jobLedger.BuildOutcomeTotals(censusCtx)
	cancelCensus()
	if err == nil {
		applyBuildOutcomeCensus(&snapshot, totals)
	}
	return snapshot
}

// applyBuildOutcomeCensus lifts the durable job census onto the scrape
// snapshot. It stays a separate step rather than an inline assignment so a
// failed read leaves all three at zero: the exposition only ever raises these
// counters, so zero republishes the previous reading, while a partially applied
// census would show more successes than submissions.
func applyBuildOutcomeCensus(
	snapshot *metrics.SchedulerSnapshot,
	totals persistence.BuildOutcomeTotals,
) {
	snapshot.JobsSubmitted = totals.Submitted
	snapshot.JobsSucceeded = totals.Succeeded
	snapshot.JobsFailed = totals.Failed
}

func schedulerMetricsSnapshotFromStatus(
	status map[string]any,
) metrics.SchedulerSnapshot {
	fairness := metricMap(status["fairness"])
	workerScoring := metricMap(status["worker_scoring"])
	leaseExpiries := metricMap(status["lease_expiries"])
	targetHistory := metricMap(status["target_history"])
	projection := metricMap(targetHistory["projection"])
	autoscaler := metricMap(status["autoscaler"])
	actuator := metricMap(autoscaler["actuator"])
	pools, blockedPools := capacityPoolMetricCounts(autoscaler["pools"])
	targetSamples, targetSuccesses, targetFailures, targetBreaches,
		targetReserved, targetCharged := targetHistoryMetricCounts(
		targetHistory,
	)
	snapshot := metrics.SchedulerSnapshot{
		QueuedTasks:             metricInt64(status["queued_tasks"]),
		UnschedulableTasks:      metricInt64(status["unschedulable_tasks"]),
		RunningTasks:            metricInt64(status["running_tasks"]),
		EligibleProjects:        metricInt64(fairness["eligible_projects"]),
		StarvedProjects:         metricInt64(fairness["starved_projects"]),
		MaxQueueWaitSeconds:     metricInt64(fairness["max_queue_wait_seconds"]),
		WorkerDecisions:         metricInt64(workerScoring["decisions_last_hour"]),
		WorkerMultiCandidate:    metricInt64(workerScoring["multi_candidate_last_hour"]),
		TargetSamples30d:        targetSamples,
		TargetSuccesses30d:      targetSuccesses,
		TargetFailures30d:       targetFailures,
		TargetSLOBreaches30d:    targetBreaches,
		TargetReservedCost30d:   targetReserved,
		TargetChargedCost30d:    targetCharged,
		AutoscaleActiveSlots:    metricInt64(autoscaler["active_slots"]),
		AutoscaleDesiredSlots:   metricInt64(autoscaler["desired_slots"]),
		AutoscaleBacklog:        metricInt64(autoscaler["backlog"]),
		AutoscalePools:          pools,
		AutoscaleBlockedPools:   blockedPools,
		CapacityOpenActions:     metricInt64(actuator["open_actions"]),
		CapacityProvisioning:    metricInt64(actuator["provisioning_instances"]),
		CapacityActive:          metricInt64(actuator["active_instances"]),
		CapacityDraining:        metricInt64(actuator["draining_instances"]),
		CapacityDeleting:        metricInt64(actuator["deleting_instances"]),
		LeaseAttemptRequeued:    metricInt64(leaseExpiries["attempt_requeued"]),
		LeaseAttemptFailed:      metricInt64(leaseExpiries["attempt_failed"]),
		LeaseAttemptCanceled:    metricInt64(leaseExpiries["attempt_canceled"]),
		LeaseAdmissionRequeued:  metricInt64(leaseExpiries["admission_requeued"]),
		LeaseAdmissionFailed:    metricInt64(leaseExpiries["admission_failed"]),
		LeaseAdmissionCanceled:  metricInt64(leaseExpiries["admission_canceled"]),
		LeasePhaseReclaimed:     metricInt64(leaseExpiries["phase_reclaimed"]),
		ProjectionConfigured:    status["authority"] == "postgresql",
		ProjectionSnapshotValid: metricBool(projection["valid"]),
		ProjectionSourcePresent: metricBool(projection["source_watermark_present"]),
		ProjectionLagSeconds:    metricInt64(projection["lag_seconds"]),
		// The mTLS gateway census. portage_builders_* is fed by the legacy
		// register/heartbeat routes, which nothing calls once executors arrive
		// over the gateway, so these are the only worker counts that move under
		// the deployed topology.
		GatewayWorkersActive:     metricInt64(status["active_workers"]),
		GatewayWorkersCapability: metricInt64(status["capability_workers"]),
		GatewayWorkersStale:      metricInt64(status["stale_workers"]),
	}
	return snapshot
}

// SetWorkerIssuer installs the listener-trust-bound issuer during startup.
func (s *Server) SetWorkerIssuer(issuer workergateway.Issuer) error {
	return s.builder.SetWorkerIssuer(issuer)
}

// Initialize initializes the server, including GPG key setup and state persistence.
func (s *Server) Initialize() error {
	executorOnly := s.config.RuntimeRole == "executor"
	apiOnly := s.config.RuntimeRole == "api"
	// Validate an explicitly configured control-plane catalog before GPG,
	// persistence, or any other side effect. Compatibility mode is loaded later
	// because it must reflect dashboard-persisted cloud settings.
	if s.config.CatalogPath != "" {
		if err := s.loadBuildCatalog(); err != nil {
			return err
		}
	}
	if err := s.initArtifactStorage(); err != nil {
		if s.config.DeploymentMode == config.DeploymentModePublic {
			return err
		}
		log.Printf("Warning: artifact storage initialization failed: %v", err)
	} else {
		s.builder.SetArtifactStorage(s.artifactStorage)
	}

	if err := s.initDatabase(); err != nil {
		return err
	}
	if !executorOnly {
		if err := s.initIAM(); err != nil {
			return err
		}
	}
	if err := s.initCache(); err != nil {
		return err
	}

	// Initialize persistence
	if err := s.initPersistence(); err != nil {
		// Persistence failure is non-fatal — log and continue
		log.Printf("Warning: failed to initialize persistence: %v (server will run without state persistence)", err)
	}

	// Apply dashboard-managed cloud settings saved from a previous run (these
	// override the static conf/env values).
	s.loadCloudSettingsOverride()

	// Compatibility mode is derived only after the dashboard cloud override.
	if s.config.CatalogPath == "" {
		if err := s.loadBuildCatalog(); err != nil {
			return err
		}
	}
	if err := s.configureBinhostStores(); err != nil {
		return err
	}
	if s.config.StorageType == "s3" && s.artifactStorage != nil {
		if err := s.validateObjectBinhostChannels(); err != nil {
			s.setArtifactStorageError(err)
			if s.config.DeploymentMode == config.DeploymentModePublic {
				return fmt.Errorf("validate object binhost channels: %w", err)
			}
			log.Printf("Warning: object binhost validation failed: %v", err)
		}
	}
	if !executorOnly {
		if err := s.syncFactoryStatus(); err != nil {
			log.Printf("Warning: image-factory status was not persisted: %v", err)
		}
	}

	// Only the isolated signer's public key is exposed to verification and
	// mirror publication. The control plane never loads a release private key.
	s.builder.SetGPGKeyProvider(s.gpgKeyMaterial)

	// Build the binhost Packages index from whatever is already on disk so
	// clients can immediately consume this server as a binhost, then keep it
	// fresh in the background as new packages appear in PKGDIR.
	if !executorOnly {
		s.refreshBinhostIndexes("startup")
		if s.config.StorageType != "s3" {
			if err := s.reconcileArtifacts(); err != nil {
				log.Printf("Warning: artifact metadata reconciliation failed: %v", err)
			}
		}
		s.startBinhostRefresher(5 * time.Minute)
	}
	// The public API role never receives provider/SSH credentials and therefore
	// cannot own Terraform cleanup. Combined trusted mode retains the legacy
	// behavior; separated executors own cleanup in the public topology.
	if !apiOnly {
		s.builder.StartInfrastructureCleanup()
	}
	if s.config.PhaseExecutorMode == "active" {
		if s.jobLedger == nil || !s.config.Database.Required ||
			!s.config.WorkerGatewayEnabled ||
			len(s.config.RemoteBuilders) > 0 {
			return fmt.Errorf(
				"active phase executor requires the PostgreSQL authority, required database mode, outbound worker gateway, and no legacy remote builders",
			)
		}
	}
	// A worker pool that refused to start leaves a replica that accepts builds
	// and never runs them; that is a startup failure, not a warning.
	if err := s.builder.StartWorkers(); err != nil {
		return fmt.Errorf("start build workers: %w", err)
	}
	if executorOnly {
		log.Printf(
			"Persistent executor role started without control-plane or Worker Gateway listeners",
		)
	} else if apiOnly {
		log.Printf(
			"API role started admission and Worker Gateway without provider phase execution",
		)
	}

	return nil
}

func (s *Server) initArtifactStorage() error {
	store, err := artifactstorage.NewStorage(&artifactstorage.Config{
		Type:            s.config.StorageType,
		LocalDir:        firstNonEmpty(s.config.StorageLocalDir, s.config.BinpkgPath),
		S3Bucket:        s.config.StorageS3Bucket,
		S3Region:        s.config.StorageS3Region,
		S3Prefix:        s.config.StorageS3Prefix,
		S3Endpoint:      s.config.StorageS3Endpoint,
		S3UsePathStyle:  s.config.StorageS3UsePathStyle,
		S3PublicBaseURL: s.config.StorageS3PublicBaseURL,
		S3AllowDelete:   s.config.StorageS3AllowDelete,
	})
	if err != nil {
		s.setArtifactStorageError(err)
		return fmt.Errorf("initialize artifact storage: %w", err)
	}
	s.artifactStorage = store
	s.setArtifactStorageError(nil)
	return nil
}

func (s *Server) setArtifactStorageError(err error) {
	s.artifactStorageMu.Lock()
	defer s.artifactStorageMu.Unlock()
	if err == nil {
		s.artifactStorageErr = ""
		return
	}
	s.artifactStorageErr = err.Error()
}

func (s *Server) artifactStorageError() string {
	s.artifactStorageMu.RLock()
	defer s.artifactStorageMu.RUnlock()
	return s.artifactStorageErr
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *Server) initCache() error {
	if !s.config.Cache.Enabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cache, err := runtimecache.Open(ctx, s.config.Cache)
	cancel()
	if err != nil {
		s.cacheInitErr = err.Error()
		if s.config.Cache.Required {
			return fmt.Errorf("required Redis unavailable: %w", err)
		}
		log.Printf("Warning: optional Redis unavailable; PostgreSQL polling remains active: %v", err)
		return nil
	}
	s.cache = cache
	s.cacheInitErr = ""
	s.builder.SetEphemeralHooks(
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = cache.PublishWake(ctx)
			cancel()
		},
		func(status builder.BuildStatus) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = cache.PublishJobEvent(ctx, runtimecache.JobEvent{
				JobID: status.JobID, Status: status.Status, UpdatedAt: status.UpdatedAt,
			})
			cancel()
		},
	)
	runCtx, stop := context.WithCancel(context.Background())
	s.cacheStop = stop
	presenceID, _ := os.Hostname()
	presenceID = presenceID + "/" + uuid.NewString()
	s.cacheWG.Add(2)
	go func() {
		defer s.cacheWG.Done()
		_ = cache.SubscribeWake(runCtx, s.builder.WakeWorkersLocal)
	}()
	go func() {
		defer s.cacheWG.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(runCtx, 2*time.Second)
			_ = cache.TouchPresence(ctx, presenceID, 45*time.Second)
			cancel()
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	log.Printf("Redis ephemeral acceleration enabled (presence, rate limit, wakeup, SSE)")
	return nil
}

// initDatabase establishes the DB-0 sidecar connection and validates the
// schema contract. The server never runs migrations; deployment must execute
// portage-migrate before starting a DATABASE_REQUIRED replica.
func (s *Server) initDatabase() error {
	if !s.config.Database.Enabled {
		return nil
	}
	db, err := persistence.Open(context.Background(), s.config.Database)
	if err != nil {
		s.databaseInitErr = err.Error()
		if s.config.Database.Required {
			return fmt.Errorf("required database unavailable: %w", err)
		}
		log.Printf("Warning: optional PostgreSQL unavailable: %v", err)
		return nil
	}
	s.database = db
	health := db.Check(context.Background())
	if !health.OK {
		s.databaseInitErr = health.Reason
		if s.config.Database.Required {
			db.Close()
			s.database = nil
			return fmt.Errorf("required database schema incompatible: %s", health.Reason)
		}
		log.Printf("Warning: optional PostgreSQL schema is not ready: %s", health.Reason)
		return nil
	}
	s.databaseInitErr = ""
	s.jobLedger = persistence.NewJobRepository(db)
	s.builder.SetJobLedger(s.jobLedger)
	log.Printf("PostgreSQL connected (schema version: %d; durable scheduler and runtime metadata enabled)", health.SchemaVersion)
	return nil
}

func (s *Server) loadBuildCatalog() error {
	if s.config.CatalogPath != "" {
		c, err := catalog.Load(s.config.CatalogPath)
		if err != nil {
			return fmt.Errorf("load build catalog: %w", err)
		}
		s.builder.SetBuildCatalog(c)
		log.Printf("Build catalog loaded from %s", s.config.CatalogPath)
		return nil
	}

	cs := s.builder.CloudSettings()
	c := catalog.NewCompatibility(catalog.CompatibilityOptions{
		Provider: cs.Provider, BuildMode: cs.BuildMode, Template: cs.PVETemplate,
		GentooSyncType: cs.PortageSyncMethod, GentooSyncURI: cs.PortageSyncURI,
	})
	s.builder.SetBuildCatalog(c)
	log.Printf("WARNING: CATALOG_PATH is empty; using single-template compatibility catalog")
	return nil
}

type binhostProfile struct {
	ID          string `json:"profile_id"`
	Arch        string `json:"arch"`
	ProfilePath string `json:"profile_path"`
	BinhostPath string `json:"binhost_path"`
	Channel     string `json:"channel,omitempty"`
	Default     bool   `json:"default"`
	SyncPath    string `json:"sync_path"`
}

// configureBinhostStores materializes one independent PKGDIR for each catalog
// profile. The catalog is the allowlist: promotion can never choose an
// arbitrary filesystem path supplied by a client or builder.
func (s *Server) configureBinhostStores() error {
	c := s.builder.BuildCatalog()
	if c == nil {
		return fmt.Errorf("configure binhosts: build catalog is unavailable")
	}
	stores := make(map[string]*binpkg.Store, len(c.Profiles))
	profiles := make(map[string]binhostProfile, len(c.Profiles))
	defaultPath := ""
	var defaultStore *binpkg.Store
	for _, profile := range c.Profiles {
		if err := catalog.ValidateBinhostPath(profile.BinhostPath, profile.Arch); err != nil {
			return fmt.Errorf("configure binhost for profile %q: %w", profile.ID, err)
		}
		store := binpkg.NewStore(filepath.Join(s.binpkgRoot, filepath.FromSlash(profile.BinhostPath)))
		stores[profile.BinhostPath] = store
		profiles[profile.ID] = binhostProfile{
			ID: profile.ID, Arch: profile.Arch, ProfilePath: profile.ProfilePath,
			BinhostPath: profile.BinhostPath, Channel: profile.Channel,
			Default: profile.Default, SyncPath: "/binpkgs/" + profile.BinhostPath,
		}
		if profile.Default {
			defaultPath = profile.BinhostPath
			defaultStore = store
		}
	}
	if defaultStore == nil {
		return fmt.Errorf("configure binhosts: catalog has no default profile")
	}
	s.binpkgMu.Lock()
	s.binpkgStores = stores
	s.binhostProfiles = profiles
	s.defaultBinhost = defaultPath
	// Retained as the default-profile store for internal compatibility.
	s.binpkgStore = defaultStore
	s.binpkgMu.Unlock()
	return nil
}

func (s *Server) binhostStoreByPath(binhostPath string) (*binpkg.Store, binhostProfile, bool) {
	s.binpkgMu.RLock()
	defer s.binpkgMu.RUnlock()
	store, ok := s.binpkgStores[binhostPath]
	if !ok {
		return nil, binhostProfile{}, false
	}
	for _, profile := range s.binhostProfiles {
		if profile.BinhostPath == binhostPath {
			return store, profile, true
		}
	}
	return nil, binhostProfile{}, false
}

func (s *Server) binhostStoreForProfile(profileID string) (*binpkg.Store, binhostProfile, bool) {
	s.binpkgMu.RLock()
	defer s.binpkgMu.RUnlock()
	if profileID == "" {
		if s.defaultBinhost == "" {
			// Unit tests that exercise New without Initialize retain an empty
			// compatibility store.
			return s.binpkgStore, binhostProfile{Arch: "amd64"}, s.binpkgStore != nil
		}
		for _, profile := range s.binhostProfiles {
			if profile.Default {
				return s.binpkgStores[profile.BinhostPath], profile, true
			}
		}
		return nil, binhostProfile{}, false
	}
	profile, ok := s.binhostProfiles[profileID]
	if !ok {
		return nil, binhostProfile{}, false
	}
	return s.binpkgStores[profile.BinhostPath], profile, true
}

func (s *Server) refreshBinhostIndexes(reason string) {
	s.binpkgMu.RLock()
	stores := make(map[string]*binpkg.Store, len(s.binpkgStores))
	profiles := make(map[string]binhostProfile, len(s.binhostProfiles))
	for path, store := range s.binpkgStores {
		stores[path] = store
	}
	for id, profile := range s.binhostProfiles {
		profiles[id] = profile
	}
	legacy := s.binpkgStore
	s.binpkgMu.RUnlock()
	if len(stores) == 0 {
		if legacy != nil {
			if _, err := legacy.RegenerateIndex("amd64"); err != nil {
				log.Printf("Warning: binhost index refresh (%s) failed: %v", reason, err)
			}
		}
		return
	}
	var objectRefreshErr error
	for _, profile := range profiles {
		store := stores[profile.BinhostPath]
		if s.config.StorageType == "s3" && s.artifactStorage != nil {
			n, err := s.refreshObjectBinhostIndex(store, profile)
			if err != nil {
				channelKey, keyErr := artifactstorage.StableChannelKey(
					profile.BinhostPath, profile.Arch,
				)
				if keyErr == nil {
					if exists, existsErr := s.artifactStorage.Exists(channelKey); existsErr == nil && !exists {
						continue
					}
				}
				objectRefreshErr = errors.Join(objectRefreshErr,
					fmt.Errorf("profile %s: %w", profile.ID, err))
				log.Printf("Warning: object binhost refresh (%s) failed for profile %s: %v",
					reason, profile.ID, err)
				continue
			}
			log.Printf("Object binhost refreshed (%s): profile=%s packages=%d",
				reason, profile.ID, n)
			continue
		}
		n, err := store.RegenerateIndex(profile.Arch)
		if err != nil {
			log.Printf("Warning: binhost index refresh (%s) failed for profile %s: %v", reason, profile.ID, err)
			continue
		}
		log.Printf("Binhost index refreshed (%s): profile=%s packages=%d path=%s/Packages",
			reason, profile.ID, n, store.BasePath())
	}
	if s.config.StorageType == "s3" {
		s.setArtifactStorageError(objectRefreshErr)
	}
}

func (s *Server) refreshObjectBinhostIndex(
	store *binpkg.Store,
	profile binhostProfile,
) (int, error) {
	pointer, _, err := s.loadObjectChannel(profile.BinhostPath, profile.Arch)
	if err != nil {
		return 0, err
	}
	packagesKey, err := artifactstorage.PublishedGenerationKey(
		profile.BinhostPath, profile.Arch, pointer.GenerationID, "Packages",
	)
	if err != nil {
		return 0, err
	}
	document, err := artifactstorage.DownloadBytes(
		s.artifactStorage, packagesKey, s.objectReadScratch(), 64<<20,
	)
	if err != nil {
		return 0, err
	}
	if digestDocument(document) != pointer.PackagesSHA256 {
		return 0, fmt.Errorf("published Packages digest mismatch")
	}
	return store.LoadPackagesIndex(document, profile.Arch)
}

func (s *Server) validateObjectBinhostChannels() error {
	s.binpkgMu.RLock()
	profiles := make([]binhostProfile, 0, len(s.binhostProfiles))
	for _, profile := range s.binhostProfiles {
		profiles = append(profiles, profile)
	}
	s.binpkgMu.RUnlock()
	for _, profile := range profiles {
		channelKey, err := artifactstorage.StableChannelKey(
			profile.BinhostPath, profile.Arch,
		)
		if err != nil {
			return err
		}
		exists, err := s.artifactStorage.Exists(channelKey)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		store, _, ok := s.binhostStoreByPath(profile.BinhostPath)
		if !ok {
			return fmt.Errorf("catalog binhost %q has no query store", profile.BinhostPath)
		}
		if _, err := s.refreshObjectBinhostIndex(store, profile); err != nil {
			return fmt.Errorf("profile %s: %w", profile.ID, err)
		}
	}
	s.setArtifactStorageError(nil)
	return nil
}

// startBinhostRefresher periodically regenerates the binhost index so packages
// added to PKGDIR out of band become visible to emerge without a restart.
func (s *Server) startBinhostRefresher(interval time.Duration) {
	s.binhostStop = make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.binhostStop:
				return
			case <-ticker.C:
				s.refreshBinhostIndexes("periodic")
				if s.config.StorageType != "s3" {
					if err := s.reconcileArtifacts(); err != nil {
						log.Printf("Warning: artifact metadata reconciliation failed: %v", err)
					}
				}
			}
		}
	}()
}

// initPersistence sets up the server store and loads any previously saved state.
func (s *Server) initPersistence() error {
	if s.jobLedger != nil {
		// The reconciler is installed before the first load because Initialize
		// downgrades our error to a warning and boots anyway: PostgreSQL is the
		// only job read authority here, so a replica that gave up after one
		// failed load would answer every lookup from an empty map until it was
		// restarted. Readiness stays red (see checkLedgerHealth) until one of
		// the 5s ticks actually lands a projection.
		s.startLedgerReconciler(5 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		durableJobs, loadErr := s.jobLedger.LoadVisible(ctx)
		cancel()
		s.jobLedger.RecordProjectionSync(loadErr)
		if loadErr != nil {
			return fmt.Errorf("load PostgreSQL job projection: %w", loadErr)
		}
		s.builder.SyncLedgerJobs(durableJobs)
		s.pruneLedgerOnce()
		log.Printf("PostgreSQL is the sole job read/write authority; legacy JSON job persistence is disabled")
		return nil
	}

	store, err := NewServerStore(s.config.DataDir)
	if err != nil {
		return fmt.Errorf("failed to create server store: %w", err)
	}
	s.store = store

	// Load previously persisted jobs
	jobs, err := store.LoadJobs()
	if err != nil {
		log.Printf("Warning: failed to load persisted jobs: %v", err)
	} else if len(jobs) > 0 {
		s.builder.LoadJobs(jobs)
		log.Printf("Loaded %d persisted jobs from disk", len(jobs))
	}
	// Start periodic persistence (save every 30s, clean jobs older than 7 days)
	s.persister = NewServerPersister(
		store,
		s.builder.GetJobsSnapshot,
		30*time.Second,
		7*24*time.Hour,
	)
	s.persister.Start()
	log.Printf("Server persistence started (data dir: %s)", s.config.DataDir)

	return nil
}

// pruneLedgerOnce performs the boot-time ledger janitorial work. It is bound to
// a sync.Once so the reconciler can still run it after a failed first load
// without repeating it on every tick.
func (s *Server) pruneLedgerOnce() {
	s.ledgerPruneOnce.Do(func() {
		if pruned, err := s.jobLedger.PruneStaleWorkers(context.Background(), time.Now().Add(-time.Hour)); err != nil {
			log.Printf("Warning: stale scheduler worker pruning failed: %v", err)
		} else if pruned > 0 {
			log.Printf("Pruned %d stale scheduler worker slot(s)", pruned)
		}
		if expired, err := s.jobLedger.ExpireTerminal(context.Background(), time.Now().Add(-7*24*time.Hour)); err != nil {
			log.Printf("Warning: PostgreSQL job retention failed: %v", err)
		} else if expired > 0 {
			log.Printf("PostgreSQL job retention hid %d terminal job(s)", expired)
		}
	})
}

func (s *Server) startLedgerReconciler(interval time.Duration) {
	if s.jobLedger == nil || s.ledgerStop != nil {
		return
	}
	// Readiness is judged against projection freshness rather than the ledger's
	// last write error: ClaimNext records a successful write on every empty
	// poll, which would wash a permanently stale projection green in about a
	// second. Six missed refreshes tolerate a slow query without hiding a
	// projection that has stopped advancing.
	s.ledgerStaleAfter = interval * 6
	s.ledgerStop = make(chan struct{})
	s.ledgerWG.Add(1)
	go func() {
		defer s.ledgerWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ledgerStop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				jobs, err := s.jobLedger.LoadVisible(ctx)
				cancel()
				s.jobLedger.RecordProjectionSync(err)
				if err != nil {
					log.Printf("Warning: PostgreSQL job projection refresh failed: %v", err)
					continue
				}
				s.builder.SyncLedgerJobs(jobs)
				s.pruneLedgerOnce()
			}
		}
	}()
}

// Shutdown gracefully shuts down the server components.
// It saves state, stops the builder, and closes the registry.
func (s *Server) Shutdown() {
	log.Println("Shutting down server components...")

	// Stop the binhost index refresher.
	if s.binhostStop != nil {
		close(s.binhostStop)
		s.binhostStop = nil
	}

	if s.ledgerStop != nil {
		close(s.ledgerStop)
		s.ledgerWG.Wait()
		s.ledgerStop = nil
	}
	// Stop persistence after the reconciler (performs final save).
	if s.persister != nil {
		log.Println("Saving server state to disk...")
		s.persister.Stop()
	}

	// Shutdown builder manager (closes work queue, stops IaC cleanup)
	if s.builder != nil {
		s.builder.Shutdown()
	}

	// Close builder registry
	if s.builderRegistry != nil {
		s.builderRegistry.Close()
	}

	if s.database != nil {
		s.database.Close()
		s.database = nil
	}
	if s.cacheStop != nil {
		s.cacheStop()
		s.cacheWG.Wait()
		s.cacheStop = nil
	}
	if s.cache != nil {
		_ = s.cache.Close()
		s.cache = nil
	}

	log.Println("Server components shut down")
}

// Router returns the HTTP router for the server.
func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()

	// Package query endpoints
	mux.HandleFunc("/api/v1/packages/query", s.handlePackageQuery)
	mux.HandleFunc("/api/v1/packages/request-build", s.handleBuildRequest)
	mux.HandleFunc("/api/v1/packages/status", s.handleBuildStatus)
	mux.HandleFunc("/api/v1/public/packages", s.handlePublicPackages)
	mux.HandleFunc("/api/v1/public/status", s.handlePublicStatus)
	mux.HandleFunc("/api/v1/binhosts", s.handleBinhostInventory)

	// Build management endpoints
	mux.HandleFunc("/api/v1/settings/cloud", s.handleCloudSettings)
	mux.HandleFunc("/api/v1/settings/cloud/test", s.handleCloudSettingsTest)
	mux.HandleFunc("/api/v1/instances", s.handleInstancesList)
	mux.HandleFunc("/api/v1/instances/shell", s.handleInstanceShell)
	mux.HandleFunc("/api/v1/builds/delete", s.handleBuildDelete)
	mux.HandleFunc("/api/v1/builds/cancel", s.handleBuildCancel)
	mux.HandleFunc("/api/v1/builds/retry", s.handleBuildRetry)
	mux.HandleFunc("/api/v1/builds/cleanup-failed", s.handleBuildsCleanupFailed)
	mux.HandleFunc("/api/v1/builds/list", s.handleBuildsList)
	mux.HandleFunc("/api/v1/builds/submit", s.handleSubmitBuildWithConfig)
	mux.HandleFunc("/api/v1/builds/status", s.handleBuildStatus)
	mux.HandleFunc("/api/v1/builds/logs", s.handleBuildLogs)
	mux.HandleFunc("/api/v1/cluster/status", s.handleClusterStatus)
	mux.HandleFunc("/api/v1/scheduler/status", s.handleSchedulerStatus)
	mux.HandleFunc("/api/v1/worker-gateway/status", s.handleWorkerGatewayStatus)
	mux.HandleFunc("/api/v1/worker-gateway/identities", s.handleWorkloadIdentityInventory)
	mux.HandleFunc("/api/v1/worker-gateway/certificates/revoke", s.handleWorkloadCertificateRevoke)
	mux.HandleFunc("/api/v1/worker-gateway/issuers/revoke", s.handleWorkloadIssuerRevoke)
	mux.HandleFunc("/api/v1/ledger/status", s.handleLedgerStatus)
	mux.HandleFunc("/api/v1/image-factory/status", s.handleImageFactoryStatus)
	mux.HandleFunc("/api/v1/runtime-metadata/status", s.handleRuntimeMetadataStatus)
	mux.HandleFunc("/api/v1/cache/status", s.handleCacheStatus)
	mux.HandleFunc("/api/v1/events/jobs", s.handleJobEvents)
	mux.HandleFunc("/api/v1/iam/exchange", s.handleIAMExchange)
	mux.HandleFunc("/api/v1/iam/device/authorization", s.handleIAMDeviceAuthorization)
	mux.HandleFunc("/api/v1/iam/device/token", s.handleIAMDeviceToken)
	mux.HandleFunc("/api/v1/iam/device/decision", s.handleIAMDeviceDecision)
	mux.HandleFunc("/api/v1/iam/providers/", s.handleIAMProviderLifecycle)
	mux.HandleFunc("/api/v1/iam/me", s.handleIAMMe)
	mux.HandleFunc("/api/v1/projects", s.handleProjects)
	mux.HandleFunc("/api/v1/projects/members", s.handleProjectMembers)
	mux.HandleFunc("/api/v1/projects/policy", s.handleProjectPolicy)
	mux.HandleFunc("/api/v1/iam/sessions", s.handleIAMSessions)
	mux.HandleFunc("/api/v1/iam/sessions/revoke-all", s.handleIAMRevokeAllSessions)

	// Builder endpoints
	mux.HandleFunc("/api/v1/builders/register", s.handleBuilderRegister)
	mux.HandleFunc("/api/v1/builders/list", s.handleBuildersList)
	mux.HandleFunc("/api/v1/builders/status", s.handleBuildersStatus)

	// Artifact download proxy endpoints
	mux.HandleFunc("/api/v1/artifacts/download/", s.handleArtifactDownload)
	mux.HandleFunc("/api/v1/artifacts/info/", s.handleArtifactInfo)

	// Binhost: serve the PKGDIR (including the Packages index) so a stock
	// `emerge --getbinpkg` can consume this server. This is intentionally public
	// (emerge cannot present the API key) and read-only.
	if s.config.StorageType == "s3" && s.artifactStorage != nil {
		mux.Handle("/binpkgs/", s.objectBinhostHandler())
	} else {
		binhostFS := http.FileServer(http.Dir(s.binpkgRoot))
		mux.Handle("/binpkgs/", http.StripPrefix("/binpkgs/", s.binhostReadOnly(binhostFS)))
	}
	mux.HandleFunc("/verify-binhost/", s.builder.ServeVerificationBinhost)

	// GPG endpoint
	mux.HandleFunc("/api/v1/gpg/public-key", s.handleGPGPublicKey)
	mux.HandleFunc("/api/v1/gpg/status", s.handleGPGStatus)
	mux.HandleFunc("/api/v1/gpg/generate", s.handleGPGGenerate)
	mux.HandleFunc("/api/v1/gpg/pubkey", s.handleGPGPubkey)

	// Heartbeat endpoint
	mux.HandleFunc("/api/v1/heartbeat", s.handleHeartbeat)

	// Metrics endpoints
	if s.metrics.IsEnabled() {
		mux.Handle("/metrics", s.metrics.Handler())                      // Legacy expvar JSON
		mux.Handle("/metrics/prometheus", s.metrics.PrometheusHandler()) // Prometheus text format
	}

	// Health / readiness / liveness probes (always public)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/livez", s.handleLivez)

	// Stack middleware (outermost first):
	// requestID → enhancedLogging → CORS → maxBodySize → authentication/RBAC
	var handler http.Handler = mux
	handler = s.rateLimitMiddleware(handler)
	handler = s.apiKeyAuthMiddleware(handler)
	handler = s.authenticationRateLimitMiddleware(handler)
	handler = s.maxBodySizeMiddleware(handler)
	handler = s.corsMiddleware(handler)
	handler = s.enhancedLoggingMiddleware(handler)
	handler = s.requestIDMiddleware(handler)

	return handler
}

// WorkerGatewayHandler exposes only the attempt-bound worker protocol. Callers
// must mount it on the dedicated mutual-TLS listener, never the ordinary API
// listener.
func (s *Server) WorkerGatewayHandler() http.Handler {
	return s.builder.WorkerGatewayHandler()
}

func (s *Server) handleWorkerGatewayStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status := s.builder.WorkerGatewayStatus()
	issuerRuntime := s.builder.WorkerIssuerStatus()
	issuerObserved := status.ActiveIssuers + status.DrainingIssuers +
		status.RevokedIssuers
	issuerLifecycleHealthy := issuerObserved == 0 || status.ActiveIssuers > 0
	issuerHealthy := issuerLifecycleHealthy && issuerRuntime.Healthy
	response := map[string]any{
		"enabled":               s.config.WorkerGatewayEnabled,
		"authority":             status.Authority,
		"listener_port":         s.config.WorkerGatewayPort,
		"transport":             "mTLS",
		"registered_sessions":   status.RegisteredSessions,
		"connected_sessions":    status.ConnectedSessions,
		"pending_tasks":         status.PendingTasks,
		"pending_uploads":       status.PendingUploads,
		"active_issuers":        status.ActiveIssuers,
		"draining_issuers":      status.DrainingIssuers,
		"revoked_issuers":       status.RevokedIssuers,
		"active_certificates":   status.ActiveCertificates,
		"revoked_certificates":  status.RevokedCertificates,
		"expiring_certificates": status.ExpiringCertificates,
		"issuer_id":             s.config.WorkerGatewayIssuerID,
		"issuer_provider":       s.config.WorkerGatewayIssuerProvider,
		"issuer_healthy":        issuerHealthy,
		"issuer_runtime":        issuerRuntime,
		"inbound_builder_api":   !s.config.WorkerGatewayEnabled,
		"executor_protocol":     builder.ExecutorProtocolVersion,
		"certificate_ttl_min":   s.config.WorkerCertificateTTLMin,
		"phase_executor_mode":   s.config.PhaseExecutorMode,
	}
	if s.jobLedger != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		phaseWork, err := s.jobLedger.GlobalPhaseWorkStatus(ctx)
		cancel()
		if err != nil {
			response["phase_work_error"] = err.Error()
		} else {
			response["phase_work"] = phaseWork
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// corsMiddleware adds CORS headers using the configured allowed origins.
// If no origins are configured, it falls back to "*" for backward compatibility.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := s.isOriginAllowed(origin)

		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		} else if len(s.config.CORSAllowedOrigins) == 0 {
			// No whitelist configured: allow all for backward compatibility
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		// If origin is not allowed and a whitelist IS configured, do not set
		// the Allow-Origin header at all — the browser will block the request.

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set(
			"Access-Control-Allow-Headers",
			"Content-Type, Authorization, X-API-Key, X-Step-Up-Key, "+
				"X-Step-Up-Authorization, X-Project-ID, Idempotency-Key",
		)

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isOriginAllowed checks whether the given origin is in the whitelist.
func (s *Server) isOriginAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, o := range s.config.CORSAllowedOrigins {
		if o == origin || o == "*" {
			return true
		}
	}
	return false
}

// apiKeyAuthMiddleware is retained as the stack entry-point name for
// compatibility, but now establishes a typed principal from either the legacy
// administrator key or a verified Portage Engine federated session.
func (s *Server) apiKeyAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicControlPlanePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		principal, err := s.authenticateRequest(r)
		if err != nil {
			s.metrics.IncHTTPRequestErrors()
			s.auditRequest(r, iam.Principal{Authentication: "unknown"},
				"request.authenticate", "http_request", r.URL.Path, "", "denied",
				map[string]any{"method": r.Method})
			writeIAMError(w, http.StatusUnauthorized, "unauthorized: invalid or missing identity")
			return
		}
		if systemAdminPath(r.URL.Path) && !principal.SystemAdmin {
			s.metrics.IncHTTPRequestErrors()
			s.auditRequest(r, principal, "request.authorize", "http_request",
				r.URL.Path, "", "denied", map[string]any{"required": "system-admin"})
			writeIAMError(w, http.StatusForbidden, "system administrator role required")
			return
		}
		if stepUpRequired(r) {
			principal, err = s.authorizeStepUp(r, principal)
			if err != nil {
				s.metrics.IncHTTPRequestErrors()
				s.auditRequest(r, principal, "request.step_up", "http_request",
					r.URL.Path, "", "denied", map[string]any{
						"method": r.Method, "reason": err.Error(),
					})
				w.Header().Set(
					"WWW-Authenticate",
					`Bearer error="insufficient_user_authentication"`,
				)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusPreconditionRequired)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "fresh step-up authentication required",
					"code":  "step_up_required",
				})
				return
			}
			s.auditRequest(r, principal, "request.step_up", "http_request",
				r.URL.Path, "", "success", map[string]any{
					"method": r.Method, "authentication": principal.Authentication,
					"session_id": principal.SessionID,
				})
		}
		next.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), principal)))
	})
}

// maxBodySizeMiddleware limits the size of incoming request bodies to prevent
// abuse. POST/PUT/PATCH methods are limited; GET/DELETE/OPTIONS pass through.
func (s *Server) maxBodySizeMiddleware(next http.Handler) http.Handler {
	maxBytes := s.config.MaxRequestBodyBytes
	if maxBytes <= 0 {
		maxBytes = 10 * 1024 * 1024 // Default 10MB
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// --- Health, Readiness, and Liveness Probes ---

// handleHealth handles health check requests.
// Returns overall system health including version and component readiness.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// Check component health
	storageOK := s.checkStorageHealth()
	buildersOnline, buildersTotal := s.checkBuildersHealth()
	databaseHealth := s.checkDatabaseHealth()
	ledgerOK, ledgerStatus := s.checkLedgerHealth()
	cacheHealth := runtimecache.Health{Enabled: s.config.Cache.Enabled}
	workerGateway := s.builder.WorkerGatewayStatus()
	issuerRuntime := s.builder.WorkerIssuerStatus()
	issuerObserved := workerGateway.ActiveIssuers +
		workerGateway.DrainingIssuers + workerGateway.RevokedIssuers
	issuerLifecycleHealthy := issuerObserved == 0 || workerGateway.ActiveIssuers > 0
	issuerHealthy := issuerLifecycleHealthy && issuerRuntime.Healthy
	metadataHealth := map[string]any{"enabled": false, "ok": true}
	metadataOK := true
	if s.jobLedger != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		status, err := s.jobLedger.RuntimeMetadataStatus(ctx)
		cancel()
		metadataOK = err == nil && status.MissingArtifacts == 0 && status.CorruptArtifacts == 0
		metadataHealth = map[string]any{"enabled": true, "ok": metadataOK, "status": status}
		if err != nil {
			metadataHealth["error"] = err.Error()
		}
	}
	if s.cache != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cacheHealth = s.cache.Health(ctx)
		cancel()
	} else if s.config.Cache.Enabled {
		cacheHealth.LastError = s.cacheInitErr
	}

	overallStatus := "healthy"
	if !storageOK {
		overallStatus = "degraded"
	}
	if s.config.Database.Enabled && !databaseHealth.OK {
		overallStatus = "degraded"
	}
	if s.jobLedger != nil && !ledgerOK {
		overallStatus = "degraded"
	}
	if s.config.Cache.Required && !cacheHealth.OK {
		overallStatus = "degraded"
	}
	if !metadataOK {
		overallStatus = "degraded"
	}
	if s.config.WorkerGatewayEnabled && !issuerHealthy {
		overallStatus = "degraded"
	}
	// On-demand VM builders are expected to disappear after every build. Their
	// stale heartbeat history is useful observability data but is not a control
	// plane dependency and must not make the load-balancer health endpoint 503.
	// Only explicitly configured long-lived remote builders are required.
	if len(s.builder.CloudSettings().RemoteBuilders) > 0 && buildersTotal > 0 && buildersOnline == 0 {
		overallStatus = "degraded"
	}

	response := map[string]interface{}{
		"status":  overallStatus,
		"version": Version,
		"commit":  Commit,
		"build":   BuildTime,
		"checks": map[string]interface{}{
			"storage": map[string]interface{}{
				"ok":   storageOK,
				"type": s.config.StorageType,
			},
			"builders": map[string]interface{}{
				"online": buildersOnline,
				"total":  buildersTotal,
			},
			"worker_gateway": map[string]interface{}{
				"enabled":          s.config.WorkerGatewayEnabled,
				"registered":       workerGateway.RegisteredSessions,
				"connected":        workerGateway.ConnectedSessions,
				"issuer_ok":        issuerHealthy,
				"issuer_runtime":   issuerRuntime,
				"active_issuers":   workerGateway.ActiveIssuers,
				"draining_issuers": workerGateway.DrainingIssuers,
				"revoked_issuers":  workerGateway.RevokedIssuers,
			},
			"database": map[string]interface{}{
				"enabled":        s.config.Database.Enabled,
				"required":       s.config.Database.Required,
				"ok":             databaseHealth.OK,
				"schema_version": databaseHealth.SchemaVersion,
				"reason":         databaseHealth.Reason,
			},
			"job_ledger":       ledgerStatus,
			"redis_cache":      cacheHealth,
			"runtime_metadata": metadataHealth,
		},
		"uptime": time.Since(s.startTime).String(),
	}

	w.Header().Set("Content-Type", "application/json")
	if overallStatus != "healthy" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) checkLedgerHealth() (bool, map[string]interface{}) {
	if s.jobLedger == nil {
		return true, map[string]interface{}{"enabled": false, "ok": true}
	}
	status := s.jobLedger.Status()
	reconcile := status.LastReconcile
	ok := status.LastError == "" && (reconcile.CheckedAt.IsZero() || reconcile.Consistent)
	// A projection that never loaded, or stopped advancing, is not observable
	// through LastError: every successful write clears it. The bound is only
	// installed once the reconciler owns the refresh.
	stale := s.ledgerStaleAfter > 0 &&
		(status.LastProjectionAt.IsZero() ||
			time.Since(status.LastProjectionAt) > s.ledgerStaleAfter)
	ok = ok && !stale
	return ok, map[string]interface{}{
		"enabled":            true,
		"ok":                 ok,
		"projection_stale":   stale,
		"authority":          status.Authority,
		"writes":             status.Writes,
		"write_errors":       status.WriteErrors,
		"projection_errors":  status.ProjectionErrors,
		"last_write_at":      status.LastWriteAt,
		"last_projection_at": status.LastProjectionAt,
		"last_error":         status.LastError,
		"last_reconcile":     reconcile,
	}
}

// handleReadyz checks if the server is ready to accept traffic.
// Returns 200 if ready, 503 if not.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	storageOK := s.checkStorageHealth()
	if !storageOK {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "not ready", "reason": "storage unavailable"})
		return
	}
	if s.config.Database.Required || s.jobLedger != nil {
		databaseHealth := s.checkDatabaseHealth()
		if !databaseHealth.OK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "not ready", "reason": databaseHealth.Reason})
			return
		}
	}
	if s.jobLedger != nil {
		if ledgerOK, ledgerStatus := s.checkLedgerHealth(); !ledgerOK {
			reason, _ := ledgerStatus["last_error"].(string)
			if stale, _ := ledgerStatus["projection_stale"].(bool); stale && reason == "" {
				reason = "PostgreSQL job projection is stale"
			}
			if reason == "" {
				reason = "job ledger reconciliation is inconsistent"
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "not ready", "reason": reason})
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		metadata, err := s.jobLedger.RuntimeMetadataStatus(ctx)
		cancel()
		if err != nil || metadata.MissingArtifacts > 0 || metadata.CorruptArtifacts > 0 {
			reason := "artifact metadata integrity check failed"
			if err != nil {
				reason = err.Error()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "not ready", "reason": reason})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

func (s *Server) checkDatabaseHealth() persistence.Health {
	if !s.config.Database.Enabled {
		return persistence.Health{OK: true, Reason: "disabled"}
	}
	if s.database == nil {
		reason := s.databaseInitErr
		if reason == "" {
			reason = "database is not initialized"
		}
		return persistence.Health{Reason: reason}
	}
	return s.database.Check(context.Background())
}

// handleLivez checks if the server process is alive.
// Always returns 200 as long as the process is running.
func (s *Server) handleLivez(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "alive"})
}

// checkStorageHealth verifies the storage backend is accessible.
func (s *Server) checkStorageHealth() bool {
	if s.artifactStorage == nil {
		// Handler-focused tests and embedded trusted callers may construct a
		// Server without Initialize. Preserve the historical local check while
		// requiring initialized clients for every remote authority.
		if s.config.StorageType == "" || s.config.StorageType == "local" {
			dir := firstNonEmpty(s.config.StorageLocalDir, s.config.BinpkgPath)
			if dir == "" {
				return true
			}
			info, err := os.Stat(dir)
			return err == nil && info.IsDir()
		}
		return false
	}
	if s.artifactStorageError() != "" {
		return false
	}
	// A missing sentinel is healthy: Exists still authenticates, resolves the
	// bucket, and performs a real backend request. Permission, endpoint, or
	// bucket failures are returned as errors.
	_, err := s.artifactStorage.Exists(".portage-engine-health")
	return err == nil
}

// checkBuildersHealth returns (online, total) counts of configured remote builders.
func (s *Server) checkBuildersHealth() (online, total int) {
	builders := s.builderRegistry.List()
	total = len(builders)
	for _, b := range builders {
		if b.Status == "online" || b.Status == "busy" {
			online++
		}
	}
	// Also count configured but unregistered remote builders
	if total == 0 {
		total = len(s.builder.CloudSettings().RemoteBuilders)
	}
	return online, total
}
