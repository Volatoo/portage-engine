// Package server implements the core Portage Engine server functionality.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/internal/binpkg"
	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/metrics"
	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/internal/runtimecache"
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
	config          *config.ServerConfig
	binpkgRoot      string
	binpkgStore     *binpkg.Store
	binpkgMu        sync.RWMutex
	binpkgStores    map[string]*binpkg.Store
	binhostProfiles map[string]binhostProfile
	defaultBinhost  string
	builder         *builder.Manager
	builderRegistry *builder.Registry
	metrics         *metrics.Metrics
	startTime       time.Time
	store           *ServerStore
	persister       *ServerPersister
	database        *persistence.Database
	jobLedger       *persistence.JobRepository
	databaseInitErr string
	cache           *runtimecache.Client
	cacheInitErr    string
	cacheStop       context.CancelFunc
	cacheWG         sync.WaitGroup
	ledgerStop      chan struct{}
	ledgerWG        sync.WaitGroup
	binhostStop     chan struct{}
	settingsMu      sync.Mutex // serializes settings updates + persistence
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
	}

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

// Initialize initializes the server, including GPG key setup and state persistence.
func (s *Server) Initialize() error {
	// Validate an explicitly configured control-plane catalog before GPG,
	// persistence, or any other side effect. Compatibility mode is loaded later
	// because it must reflect dashboard-persisted cloud settings.
	if s.config.CatalogPath != "" {
		if err := s.loadBuildCatalog(); err != nil {
			return err
		}
	}

	if err := s.initDatabase(); err != nil {
		return err
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
	if err := s.syncFactoryStatus(); err != nil {
		log.Printf("Warning: image-factory status was not persisted: %v", err)
	}

	// Only the isolated signer's public key is exposed to verification and
	// mirror publication. The control plane never loads a release private key.
	s.builder.SetGPGKeyProvider(s.gpgKeyMaterial)

	// Build the binhost Packages index from whatever is already on disk so
	// clients can immediately consume this server as a binhost, then keep it
	// fresh in the background as new packages appear in PKGDIR.
	s.refreshBinhostIndexes("startup")
	if err := s.reconcileArtifacts(); err != nil {
		log.Printf("Warning: artifact metadata reconciliation failed: %v", err)
	}
	s.startBinhostRefresher(5 * time.Minute)
	s.builder.StartInfrastructureCleanup()
	s.builder.StartWorkers()

	return nil
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
	for _, profile := range profiles {
		store := stores[profile.BinhostPath]
		n, err := store.RegenerateIndex(profile.Arch)
		if err != nil {
			log.Printf("Warning: binhost index refresh (%s) failed for profile %s: %v", reason, profile.ID, err)
			continue
		}
		log.Printf("Binhost index refreshed (%s): profile=%s packages=%d path=%s/Packages",
			reason, profile.ID, n, store.BasePath())
	}
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
				if err := s.reconcileArtifacts(); err != nil {
					log.Printf("Warning: artifact metadata reconciliation failed: %v", err)
				}
			}
		}
	}()
}

// initPersistence sets up the server store and loads any previously saved state.
func (s *Server) initPersistence() error {
	if s.jobLedger != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		durableJobs, loadErr := s.jobLedger.LoadVisible(ctx)
		cancel()
		s.jobLedger.RecordProjectionSync(loadErr)
		if loadErr != nil {
			return fmt.Errorf("load PostgreSQL job projection: %w", loadErr)
		}
		s.builder.SyncLedgerJobs(durableJobs)
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
		s.startLedgerReconciler(5 * time.Second)
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

func (s *Server) startLedgerReconciler(interval time.Duration) {
	if s.jobLedger == nil || s.ledgerStop != nil {
		return
	}
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
	mux.HandleFunc("/api/v1/ledger/status", s.handleLedgerStatus)
	mux.HandleFunc("/api/v1/image-factory/status", s.handleImageFactoryStatus)
	mux.HandleFunc("/api/v1/runtime-metadata/status", s.handleRuntimeMetadataStatus)
	mux.HandleFunc("/api/v1/cache/status", s.handleCacheStatus)
	mux.HandleFunc("/api/v1/events/jobs", s.handleJobEvents)

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
	binhostFS := http.FileServer(http.Dir(s.binpkgRoot))
	mux.Handle("/binpkgs/", http.StripPrefix("/binpkgs/", s.binhostReadOnly(binhostFS)))
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
	// requestID → enhancedLogging → CORS → maxBodySize → apiKey auth
	var handler http.Handler = mux
	handler = s.apiKeyAuthMiddleware(handler)
	handler = s.rateLimitMiddleware(handler)
	handler = s.maxBodySizeMiddleware(handler)
	handler = s.corsMiddleware(handler)
	handler = s.enhancedLoggingMiddleware(handler)
	handler = s.requestIDMiddleware(handler)

	return handler
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
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")

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

// apiKeyAuthMiddleware protects API endpoints with a shared API key.
// Public endpoints (/health, /readyz, /livez, /metrics) are excluded.
// If APIKey is empty in config, the middleware is a no-op (backward compatible).
func (s *Server) apiKeyAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth if no API key is configured
		if s.config.APIKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Public endpoints that never require auth. The binhost (/binpkgs/) is
		// public because emerge cannot present the API key; it is read-only.
		path := r.URL.Path
		if path == "/health" || path == "/readyz" || path == "/livez" || path == "/metrics" || path == "/metrics/prometheus" ||
			path == "/api/v1/binhosts" ||
			strings.HasPrefix(path, "/binpkgs/") || strings.HasPrefix(path, "/verify-binhost/") {
			next.ServeHTTP(w, r)
			return
		}

		// CORS preflight must pass through
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		// Check API key from X-API-Key header or Authorization: Bearer <key>
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			auth := r.Header.Get("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				apiKey = strings.TrimPrefix(auth, "Bearer ")
			}
		}

		// Constant-time comparison to avoid leaking the key via timing.
		if subtle.ConstantTimeCompare([]byte(apiKey), []byte(s.config.APIKey)) != 1 {
			s.metrics.IncHTTPRequestErrors()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "unauthorized: invalid or missing API key",
			})
			return
		}

		next.ServeHTTP(w, r)
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
	return ok, map[string]interface{}{
		"enabled":            true,
		"ok":                 ok,
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
	switch s.config.StorageType {
	case "local":
		dir := s.config.StorageLocalDir
		if dir == "" {
			dir = s.config.BinpkgPath
		}
		info, err := os.Stat(dir)
		return err == nil && info.IsDir()
	default:
		// For non-local storage, assume OK (actual check would require SDK calls)
		return true
	}
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
