// Package metrics provides monitoring and metrics collection.
package metrics

import (
	"expvar"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

var (
	once     sync.Once
	registry *Metrics
	// published exposes the process registry to recorders that live outside
	// the server package. Unlike New it cannot create, enable, disable, or
	// re-register anything, so a library that only counts can never decide the
	// process's metrics configuration by being initialized first.
	published atomic.Pointer[Metrics]
	// silent answers recorders that run before New. Its enabled flag is false,
	// so every recorder returns before touching an unallocated expvar.
	silent = &Metrics{}
)

// Default returns the process-wide registry for packages that only record
// metrics. It never changes the registry's configuration.
func Default() *Metrics {
	if current := published.Load(); current != nil {
		return current
	}
	return silent
}

// Config holds metrics configuration.
type Config struct {
	Enabled  bool
	Port     string
	Password string
}

type SchedulerSnapshot struct {
	QueuedTasks             int64
	UnschedulableTasks      int64
	RunningTasks            int64
	EligibleProjects        int64
	StarvedProjects         int64
	MaxQueueWaitSeconds     int64
	WorkerDecisions         int64
	WorkerMultiCandidate    int64
	TargetSamples30d        int64
	TargetSuccesses30d      int64
	TargetFailures30d       int64
	TargetSLOBreaches30d    int64
	TargetReservedCost30d   int64
	TargetChargedCost30d    int64
	AutoscaleActiveSlots    int64
	AutoscaleDesiredSlots   int64
	AutoscaleBacklog        int64
	AutoscalePools          int64
	AutoscaleBlockedPools   int64
	CapacityOpenActions     int64
	CapacityProvisioning    int64
	CapacityActive          int64
	CapacityDraining        int64
	CapacityDeleting        int64
	LeaseAttemptRequeued    int64
	LeaseAttemptFailed      int64
	LeaseAttemptCanceled    int64
	LeaseAdmissionRequeued  int64
	LeaseAdmissionFailed    int64
	LeaseAdmissionCanceled  int64
	LeasePhaseReclaimed     int64
	ProjectionConfigured    bool
	ProjectionSnapshotValid bool
	ProjectionSourcePresent bool
	ProjectionLagSeconds    int64
	DistCCWorkersFresh      int64
	DistCCSlotsTotal        int64
	DistCCSlotsLeased       int64
	DistCCLocalCompiles     int64
	DistCCRemoteCompiles    int64
	DistCCHits              int64
	DistCCFallbacks         int64
	DistCCNetworkBytes      int64
	DistCCQueueMillis       int64
	DistCCFailures          map[string]int64

	// Lifetime job census read from the durable ledger. Terminal counts only
	// ever grow — a completed job never un-completes and retention hides rows
	// rather than deleting them — so exposing them as counters is sound, and it
	// is the only way the scraped role can publish a completion total at all:
	// the process that increments the in-memory success counter is the phase
	// executor, which no scrape job reaches.
	JobsSubmitted int64
	JobsSucceeded int64
	JobsFailed    int64

	// Worker Gateway executor census. The heartbeat Registry behind
	// portage_builders_* is only written by the legacy HTTP registration
	// routes, so under the mTLS gateway topology these are the counts that
	// describe the executors actually taking work.
	GatewayWorkersActive     int64
	GatewayWorkersCapability int64
	GatewayWorkersStale      int64
}

// Metrics collects various system metrics.
type Metrics struct {
	// enabled is accessed concurrently by metric recorders and Handler,
	// so it is stored as an atomic to keep reads/writes race-free.
	enabled           atomic.Bool
	password          string
	mu                sync.RWMutex
	schedulerProvider func() SchedulerSnapshot

	// Build metrics
	buildsTotal     *expvar.Int
	buildsSucceeded *expvar.Int
	buildsFailed    *expvar.Int
	buildsQueued    *expvar.Int
	buildDurations  *expvar.Map

	// Builder metrics
	buildersActive   *expvar.Int
	buildersHealthy  *expvar.Int
	builderCapacity  *expvar.Int
	heartbeatsTotal  *expvar.Int
	heartbeatsFailed *expvar.Int

	// Storage metrics
	packagesStored *expvar.Int
	storageReads   *expvar.Int
	storageWrites  *expvar.Int
	storageErrors  *expvar.Int

	// HTTP metrics
	httpRequests      *expvar.Int
	httpRequestErrors *expvar.Int
	httpLatencies     *expvar.Map

	// System metrics
	goroutines *expvar.Int
	startTime  time.Time
}

// SetSchedulerProvider installs a low-cardinality scrape-time snapshot. The
// callback must not mutate scheduler state or expose project/package labels.
func (m *Metrics) SetSchedulerProvider(provider func() SchedulerSnapshot) {
	m.mu.Lock()
	m.schedulerProvider = provider
	m.mu.Unlock()
}

// New creates a new Metrics instance.
//
// New returns a process-wide singleton: the first call decides whether the
// metrics are published to the global expvar registry (expvar.Publish panics
// on duplicate names, so registration can only happen once). Subsequent calls
// return the same instance and may only toggle the enabled flag and password;
// they never re-register expvar variables. All access to the enabled flag is
// race-free via an atomic, so recorders and the HTTP handler can be used
// concurrently with additional New calls.
func New(cfg *Config) *Metrics {
	if cfg == nil {
		cfg = &Config{Enabled: false}
	}

	once.Do(func() {
		registry = &Metrics{
			password:          cfg.Password,
			buildsTotal:       new(expvar.Int),
			buildsSucceeded:   new(expvar.Int),
			buildsFailed:      new(expvar.Int),
			buildsQueued:      new(expvar.Int),
			buildDurations:    new(expvar.Map),
			buildersActive:    new(expvar.Int),
			buildersHealthy:   new(expvar.Int),
			builderCapacity:   new(expvar.Int),
			heartbeatsTotal:   new(expvar.Int),
			heartbeatsFailed:  new(expvar.Int),
			packagesStored:    new(expvar.Int),
			storageReads:      new(expvar.Int),
			storageWrites:     new(expvar.Int),
			storageErrors:     new(expvar.Int),
			httpRequests:      new(expvar.Int),
			httpRequestErrors: new(expvar.Int),
			httpLatencies:     new(expvar.Map),
			goroutines:        new(expvar.Int),
			startTime:         time.Now(),
		}
		registry.enabled.Store(cfg.Enabled)

		if cfg.Enabled {
			// Only publish to global expvar registry if metrics are enabled
			expvar.Publish("builds_total", registry.buildsTotal)
			expvar.Publish("builds_succeeded", registry.buildsSucceeded)
			expvar.Publish("builds_failed", registry.buildsFailed)
			expvar.Publish("builds_queued", registry.buildsQueued)
			expvar.Publish("build_durations", registry.buildDurations)
			expvar.Publish("builders_active", registry.buildersActive)
			expvar.Publish("builders_healthy", registry.buildersHealthy)
			expvar.Publish("builder_capacity", registry.builderCapacity)
			expvar.Publish("heartbeats_total", registry.heartbeatsTotal)
			expvar.Publish("heartbeats_failed", registry.heartbeatsFailed)
			expvar.Publish("packages_stored", registry.packagesStored)
			expvar.Publish("storage_reads", registry.storageReads)
			expvar.Publish("storage_writes", registry.storageWrites)
			expvar.Publish("storage_errors", registry.storageErrors)
			expvar.Publish("http_requests_total", registry.httpRequests)
			expvar.Publish("http_request_errors", registry.httpRequestErrors)
			expvar.Publish("http_latencies", registry.httpLatencies)
			expvar.Publish("goroutines", registry.goroutines)

			// Publish uptime
			expvar.Publish("uptime_seconds", expvar.Func(func() interface{} {
				return time.Since(registry.startTime).Seconds()
			}))
		}
		published.Store(registry)
	})

	// Update enabled flag if different. Note: this only toggles the runtime
	// flag; expvar registration is decided once by the first New call.
	if registry.enabled.Load() != cfg.Enabled {
		registry.enabled.Store(cfg.Enabled)
	}

	// Update password if different
	registry.mu.Lock()
	if registry.password != cfg.Password {
		registry.password = cfg.Password
	}
	registry.mu.Unlock()

	return registry
}

// IsEnabled returns whether metrics are enabled.
func (m *Metrics) IsEnabled() bool {
	return m.enabled.Load()
}

// Build metrics

// IncBuildsTotal increments the total builds counter.
func (m *Metrics) IncBuildsTotal() {
	if !m.enabled.Load() {
		return
	}
	m.buildsTotal.Add(1)
}

// IncBuildsSucceeded increments the successful builds counter.
func (m *Metrics) IncBuildsSucceeded() {
	if !m.enabled.Load() {
		return
	}
	m.buildsSucceeded.Add(1)
}

// IncBuildsFailed increments the failed builds counter.
func (m *Metrics) IncBuildsFailed() {
	if !m.enabled.Load() {
		return
	}
	m.buildsFailed.Add(1)
}

// RaiseBuildsTotal, RaiseBuildsSucceeded and RaiseBuildsFailed lift the build
// counters to a durable-ledger census taken at scrape time. They raise instead
// of adding on purpose: the census already contains every build this process
// executed, so a process that runs both the admission role and the phase
// executor would otherwise count its own builds twice. Raising is also what
// keeps the exported series a counter when the two sources disagree — a replica
// reading a lagging projection publishes the value it already published rather
// than a decrease Prometheus would read as a process restart.
func (m *Metrics) RaiseBuildsTotal(count int64) {
	if !m.enabled.Load() {
		return
	}
	raiseCounter(m.buildsTotal, count)
}

// RaiseBuildsSucceeded lifts the successful-build counter to the ledger census.
func (m *Metrics) RaiseBuildsSucceeded(count int64) {
	if !m.enabled.Load() {
		return
	}
	raiseCounter(m.buildsSucceeded, count)
}

// RaiseBuildsFailed lifts the failed-build counter to the ledger census.
func (m *Metrics) RaiseBuildsFailed(count int64) {
	if !m.enabled.Load() {
		return
	}
	raiseCounter(m.buildsFailed, count)
}

// raiseCounter moves counter up to value, never down and never by summation.
// The read and the write are not one atomic step, but the two writers converge:
// an Add that lands in between is already reflected in the next census, so the
// following scrape restores it.
func raiseCounter(counter *expvar.Int, value int64) {
	if value > counter.Value() {
		counter.Set(value)
	}
}

// SetBuildsQueued sets the number of queued builds.
func (m *Metrics) SetBuildsQueued(count int64) {
	if !m.enabled.Load() {
		return
	}
	m.buildsQueued.Set(count)
}

// RecordBuildDuration records a build duration.
func (m *Metrics) RecordBuildDuration(packageName string, duration time.Duration) {
	if !m.enabled.Load() {
		return
	}
	m.buildDurations.Add(packageName, duration.Milliseconds())
}

// Builder metrics

// SetBuildersActive sets the number of active builders.
func (m *Metrics) SetBuildersActive(count int64) {
	if !m.enabled.Load() {
		return
	}
	m.buildersActive.Set(count)
}

// SetBuildersHealthy sets the number of healthy builders.
func (m *Metrics) SetBuildersHealthy(count int64) {
	if !m.enabled.Load() {
		return
	}
	m.buildersHealthy.Set(count)
}

// SetBuilderCapacity sets the total builder capacity.
func (m *Metrics) SetBuilderCapacity(count int64) {
	if !m.enabled.Load() {
		return
	}
	m.builderCapacity.Set(count)
}

// IncHeartbeatsTotal increments the total heartbeats counter.
func (m *Metrics) IncHeartbeatsTotal() {
	if !m.enabled.Load() {
		return
	}
	m.heartbeatsTotal.Add(1)
}

// IncHeartbeatsFailed increments the failed heartbeats counter.
func (m *Metrics) IncHeartbeatsFailed() {
	if !m.enabled.Load() {
		return
	}
	m.heartbeatsFailed.Add(1)
}

// Storage metrics

// IncPackagesStored increments the packages stored counter.
func (m *Metrics) IncPackagesStored() {
	if !m.enabled.Load() {
		return
	}
	m.packagesStored.Add(1)
}

// IncStorageReads increments the storage reads counter.
func (m *Metrics) IncStorageReads() {
	if !m.enabled.Load() {
		return
	}
	m.storageReads.Add(1)
}

// IncStorageWrites increments the storage writes counter.
func (m *Metrics) IncStorageWrites() {
	if !m.enabled.Load() {
		return
	}
	m.storageWrites.Add(1)
}

// IncStorageErrors increments the storage errors counter.
func (m *Metrics) IncStorageErrors() {
	if !m.enabled.Load() {
		return
	}
	m.storageErrors.Add(1)
}

// HTTP metrics

// IncHTTPRequests increments the HTTP requests counter.
func (m *Metrics) IncHTTPRequests() {
	if !m.enabled.Load() {
		return
	}
	m.httpRequests.Add(1)
}

// IncHTTPRequestErrors increments the HTTP request errors counter.
func (m *Metrics) IncHTTPRequestErrors() {
	if !m.enabled.Load() {
		return
	}
	m.httpRequestErrors.Add(1)
}

// RecordHTTPLatency records an HTTP request latency.
func (m *Metrics) RecordHTTPLatency(endpoint string, duration time.Duration) {
	if !m.enabled.Load() {
		return
	}
	m.httpLatencies.Add(endpoint, duration.Milliseconds())
}

// System metrics

// UpdateGoroutines updates the goroutines counter.
func (m *Metrics) UpdateGoroutines(count int64) {
	if !m.enabled.Load() {
		return
	}
	m.goroutines.Set(count)
}

// Handler returns an HTTP handler for the metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	if !m.enabled.Load() {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Metrics disabled", http.StatusNotFound)
		})
	}

	// Capture the password under the lock, since New may update it concurrently.
	m.mu.RLock()
	expectedPassword := m.password
	m.mu.RUnlock()

	// If password is not set, return expvar handler directly
	if expectedPassword == "" {
		return expvar.Handler()
	}

	// Wrap expvar handler with basic auth
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "metrics" || password != expectedPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="Metrics"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		expvar.Handler().ServeHTTP(w, r)
	})
}

// PrometheusHandler returns an HTTP handler that outputs metrics in Prometheus
// text exposition format. This allows Prometheus to scrape metrics without
// requiring the prometheus/client_golang dependency.
func (m *Metrics) PrometheusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.enabled.Load() {
			http.Error(w, "Metrics disabled", http.StatusNotFound)
			return
		}

		// Capture the password under the lock, since New may update it
		// concurrently (same fix as Handler()).
		m.mu.RLock()
		expectedPassword := m.password
		schedulerProvider := m.schedulerProvider
		m.mu.RUnlock()

		// Check basic auth if password is set
		if expectedPassword != "" {
			username, password, ok := r.BasicAuth()
			if !ok || username != "metrics" || password != expectedPassword {
				w.Header().Set("WWW-Authenticate", `Basic realm="Metrics"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}

		// The durable scheduler is the only authority on how deep the queue
		// actually is, and it is read here anyway. Refresh the queue gauge from
		// it before the exposition writes the gauge, or the queue panel and its
		// alert stay pinned at zero behind any backlog.
		//
		// The completion counters need the same treatment for a different
		// reason: they are incremented in the phase executor, and the only
		// process a scrape job reaches is the one serving the API. Deriving
		// them from the ledger census puts the success rate's numerator and
		// denominator in the same scraped process instead of leaving the panel
		// dividing this replica's submissions by another process's successes.
		var scheduler SchedulerSnapshot
		if schedulerProvider != nil {
			scheduler = schedulerProvider()
			m.SetBuildsQueued(scheduler.QueuedTasks)
			m.RaiseBuildsTotal(scheduler.JobsSubmitted)
			m.RaiseBuildsSucceeded(scheduler.JobsSucceeded)
			m.RaiseBuildsFailed(scheduler.JobsFailed)
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		// Build metrics
		_, _ = fmt.Fprintf(w, "# HELP portage_builds_total Total number of builds submitted.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_builds_total counter\n")
		_, _ = fmt.Fprintf(w, "portage_builds_total %d\n", m.buildsTotal.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_builds_succeeded_total Total number of successful builds.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_builds_succeeded_total counter\n")
		_, _ = fmt.Fprintf(w, "portage_builds_succeeded_total %d\n", m.buildsSucceeded.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_builds_failed_total Total number of failed builds.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_builds_failed_total counter\n")
		_, _ = fmt.Fprintf(w, "portage_builds_failed_total %d\n", m.buildsFailed.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_builds_queued Current number of queued builds.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_builds_queued gauge\n")
		_, _ = fmt.Fprintf(w, "portage_builds_queued %d\n", m.buildsQueued.Value())

		// Builder metrics. These three describe the legacy HTTP heartbeat
		// registry only — the one fed by /api/v1/builders/register and
		// /heartbeat. A deployment running the mTLS Worker Gateway registers no
		// builder there and must read portage_gateway_workers_* instead, so the
		// topology is named in every HELP string rather than left to whoever
		// wires the panel.
		_, _ = fmt.Fprintf(w, "# HELP portage_builders_active Enabled, non-offline builders in the legacy HTTP heartbeat registry; zero under the mTLS Worker Gateway topology, which reports portage_gateway_workers_active.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_builders_active gauge\n")
		_, _ = fmt.Fprintf(w, "portage_builders_active %d\n", m.buildersActive.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_builders_healthy Legacy HTTP heartbeat registry builders last reporting online or busy.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_builders_healthy gauge\n")
		_, _ = fmt.Fprintf(w, "portage_builders_healthy %d\n", m.buildersHealthy.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_builder_capacity Concurrent builds advertised by enabled, non-offline builders in the legacy HTTP heartbeat registry.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_builder_capacity gauge\n")
		_, _ = fmt.Fprintf(w, "portage_builder_capacity %d\n", m.builderCapacity.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_heartbeats_total Total builder heartbeats received.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_heartbeats_total counter\n")
		_, _ = fmt.Fprintf(w, "portage_heartbeats_total %d\n", m.heartbeatsTotal.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_heartbeats_failed_total Total failed heartbeats.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_heartbeats_failed_total counter\n")
		_, _ = fmt.Fprintf(w, "portage_heartbeats_failed_total %d\n", m.heartbeatsFailed.Value())

		// Storage metrics
		_, _ = fmt.Fprintf(w, "# HELP portage_packages_stored Total packages in storage.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_packages_stored gauge\n")
		_, _ = fmt.Fprintf(w, "portage_packages_stored %d\n", m.packagesStored.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_storage_reads_total Total storage read operations.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_storage_reads_total counter\n")
		_, _ = fmt.Fprintf(w, "portage_storage_reads_total %d\n", m.storageReads.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_storage_writes_total Total storage write operations.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_storage_writes_total counter\n")
		_, _ = fmt.Fprintf(w, "portage_storage_writes_total %d\n", m.storageWrites.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_storage_errors_total Total storage errors.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_storage_errors_total counter\n")
		_, _ = fmt.Fprintf(w, "portage_storage_errors_total %d\n", m.storageErrors.Value())

		// HTTP metrics
		_, _ = fmt.Fprintf(w, "# HELP portage_http_requests_total Total HTTP requests.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_http_requests_total counter\n")
		_, _ = fmt.Fprintf(w, "portage_http_requests_total %d\n", m.httpRequests.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_http_request_errors_total Total HTTP request errors.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_http_request_errors_total counter\n")
		_, _ = fmt.Fprintf(w, "portage_http_request_errors_total %d\n", m.httpRequestErrors.Value())

		// System metrics
		_, _ = fmt.Fprintf(w, "# HELP portage_goroutines Current number of goroutines.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_goroutines gauge\n")
		_, _ = fmt.Fprintf(w, "portage_goroutines %d\n", m.goroutines.Value())

		_, _ = fmt.Fprintf(w, "# HELP portage_uptime_seconds Server uptime in seconds.\n")
		_, _ = fmt.Fprintf(w, "# TYPE portage_uptime_seconds gauge\n")
		_, _ = fmt.Fprintf(w, "portage_uptime_seconds %.2f\n", time.Since(m.startTime).Seconds())

		if schedulerProvider != nil {
			writeSchedulerPrometheus(w, scheduler)
		}
	})
}

func writeSchedulerPrometheus(
	w http.ResponseWriter,
	snapshot SchedulerSnapshot,
) {
	gauges := []struct {
		name, help string
		value      int64
	}{
		{"portage_scheduler_queued_tasks", "Queued durable scheduler tasks.", snapshot.QueuedTasks},
		{"portage_scheduler_unschedulable_tasks", "Queued tasks without a matching active executor.", snapshot.UnschedulableTasks},
		{"portage_scheduler_running_tasks", "Running durable scheduler tasks.", snapshot.RunningTasks},
		// The executor inventory of the mTLS Worker Gateway topology, taken
		// from the same durable rows the scheduler dispatches against. Named
		// apart from portage_builders_* because the two count different
		// populations and only one of them is non-zero in a given deployment.
		{"portage_gateway_workers_active", "Worker Gateway executors whose durable heartbeat is inside the freshness window; the mTLS gateway counterpart of portage_builders_active.", snapshot.GatewayWorkersActive},
		{"portage_gateway_workers_capability_labeled", "Fresh Worker Gateway executors advertising at least one capability label, so capability-constrained jobs are dispatchable.", snapshot.GatewayWorkersCapability},
		{"portage_gateway_workers_stale", "Registered Worker Gateway executors with no heartbeat inside the freshness window.", snapshot.GatewayWorkersStale},
		{"portage_scheduler_fair_eligible_projects", "Projects currently eligible for fair scheduling.", snapshot.EligibleProjects},
		{"portage_scheduler_fair_starved_projects", "Projects promoted by the anti-starvation threshold.", snapshot.StarvedProjects},
		{"portage_scheduler_fair_max_wait_seconds", "Largest queue wait observed at a fair dispatch.", snapshot.MaxQueueWaitSeconds},
		{"portage_scheduler_worker_decisions_last_hour", "Worker soft-scoring decisions recorded during the last hour.", snapshot.WorkerDecisions},
		{"portage_scheduler_worker_multi_candidate_last_hour", "Worker decisions with more than one eligible candidate during the last hour.", snapshot.WorkerMultiCandidate},
		{"portage_monitor_target_samples_30d", "Terminal target samples retained during the last 30 days.", snapshot.TargetSamples30d},
		{"portage_monitor_target_successes_30d", "Successful target outcomes during the last 30 days.", snapshot.TargetSuccesses30d},
		{"portage_monitor_target_failures_30d", "Failed target outcomes during the last 30 days.", snapshot.TargetFailures30d},
		{"portage_monitor_target_slo_breaches_30d", "Targets with enough samples that are below the operator SLO during the last 30 days.", snapshot.TargetSLOBreaches30d},
		{"portage_monitor_target_reserved_cost_microunits_30d", "Reserved target cloud cost during the last 30 days.", snapshot.TargetReservedCost30d},
		{"portage_monitor_target_charged_cost_microunits_30d", "Settled target cloud cost during the last 30 days.", snapshot.TargetChargedCost30d},
		{"portage_scheduler_autoscale_active_slots", "Active phase-executor slots observed by autoscaling.", snapshot.AutoscaleActiveSlots},
		{"portage_scheduler_autoscale_desired_slots", "Observe-only desired phase-executor slots.", snapshot.AutoscaleDesiredSlots},
		{"portage_scheduler_autoscale_backlog", "Backlog observed by autoscaling.", snapshot.AutoscaleBacklog},
		{"portage_scheduler_autoscale_pools", "Catalog capacity pools observed by autoscaling.", snapshot.AutoscalePools},
		{"portage_scheduler_autoscale_blocked_pools", "Capacity pools with unschedulable backlog.", snapshot.AutoscaleBlockedPools},
		{"portage_capacity_actuator_open_actions", "Open fenced capacity actions.", snapshot.CapacityOpenActions},
		{"portage_capacity_instances_provisioning", "Actuator-owned instances awaiting executor heartbeat.", snapshot.CapacityProvisioning},
		{"portage_capacity_instances_active", "Actuator-owned active persistent executors.", snapshot.CapacityActive},
		{"portage_capacity_instances_draining", "Actuator-owned persistent executors draining live work.", snapshot.CapacityDraining},
		{"portage_capacity_instances_deleting", "Actuator-owned persistent executors undergoing exact provider deletion.", snapshot.CapacityDeleting},
		{"portage_distcc_workers_fresh", "Fresh credential-minimal compile workers.", snapshot.DistCCWorkersFresh},
		{"portage_distcc_slots_total", "Fresh exact-pool compile slots.", snapshot.DistCCSlotsTotal},
		{"portage_distcc_slots_leased", "Atomically leased compile slots.", snapshot.DistCCSlotsLeased},
	}
	for _, gauge := range gauges {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n", gauge.name, gauge.help)
		_, _ = fmt.Fprintf(w, "# TYPE %s gauge\n", gauge.name)
		_, _ = fmt.Fprintf(w, "%s %d\n", gauge.name, gauge.value)
	}
	writeLeaseExpiryPrometheus(w, snapshot)
	if snapshot.ProjectionConfigured {
		writeMonitorProjectionPrometheus(w, snapshot)
	}
	writeDistCCPrometheus(w, snapshot)
}

func writeLeaseExpiryPrometheus(
	w http.ResponseWriter,
	snapshot SchedulerSnapshot,
) {
	_, _ = fmt.Fprintln(w, "# HELP portage_scheduler_lease_expiries_total Durable scheduler lease expiries by fixed lease kind and recovery result.")
	_, _ = fmt.Fprintln(w, "# TYPE portage_scheduler_lease_expiries_total counter")
	series := []struct {
		lease, result string
		value         int64
	}{
		{"attempt", "requeued", snapshot.LeaseAttemptRequeued},
		{"attempt", "failed", snapshot.LeaseAttemptFailed},
		{"attempt", "canceled", snapshot.LeaseAttemptCanceled},
		{"admission", "requeued", snapshot.LeaseAdmissionRequeued},
		{"admission", "failed", snapshot.LeaseAdmissionFailed},
		{"admission", "canceled", snapshot.LeaseAdmissionCanceled},
		{"phase", "reclaimed", snapshot.LeasePhaseReclaimed},
	}
	for _, item := range series {
		_, _ = fmt.Fprintf(
			w,
			"portage_scheduler_lease_expiries_total{lease=%q,result=%q} %d\n",
			item.lease, item.result, item.value,
		)
	}
}

func boolMetric(value bool) int {
	if value {
		return 1
	}
	return 0
}

func writeMonitorProjectionPrometheus(
	w http.ResponseWriter,
	snapshot SchedulerSnapshot,
) {
	_, _ = fmt.Fprintln(w, "# HELP portage_monitor_projection_snapshot_valid Whether the PostgreSQL-backed Monitor projection snapshot is valid.")
	_, _ = fmt.Fprintln(w, "# TYPE portage_monitor_projection_snapshot_valid gauge")
	_, _ = fmt.Fprintf(
		w, "portage_monitor_projection_snapshot_valid %d\n",
		boolMetric(snapshot.ProjectionSnapshotValid),
	)
	_, _ = fmt.Fprintln(w, "# HELP portage_monitor_projection_source_watermark_present Whether at least one durable terminal source event exists.")
	_, _ = fmt.Fprintln(w, "# TYPE portage_monitor_projection_source_watermark_present gauge")
	_, _ = fmt.Fprintf(
		w, "portage_monitor_projection_source_watermark_present %d\n",
		boolMetric(snapshot.ProjectionSourcePresent),
	)
	_, _ = fmt.Fprintln(w, "# HELP portage_monitor_projection_lag_seconds How long the cached Monitor snapshot has been serving terminal events it does not contain; zero while current or empty, bounded above by the read-through cache TTL.")
	_, _ = fmt.Fprintln(w, "# TYPE portage_monitor_projection_lag_seconds gauge")
	_, _ = fmt.Fprintf(
		w, "portage_monitor_projection_lag_seconds %d\n",
		snapshot.ProjectionLagSeconds,
	)
}

func writeDistCCPrometheus(
	w http.ResponseWriter,
	snapshot SchedulerSnapshot,
) {
	counters := []struct {
		name, help string
		value      int64
	}{
		{"portage_distcc_compile_local_total", "Observed local compiler invocations.", snapshot.DistCCLocalCompiles},
		{"portage_distcc_compile_remote_total", "Observed remote compiler invocations.", snapshot.DistCCRemoteCompiles},
		{"portage_distcc_hits_total", "Observed exact-pool compile-slot reservation hits.", snapshot.DistCCHits},
		{"portage_distcc_fallback_total", "Observed controlled local fallbacks.", snapshot.DistCCFallbacks},
		{"portage_distcc_network_bytes_total", "Observed distcc network bytes.", snapshot.DistCCNetworkBytes},
		{"portage_distcc_queue_milliseconds_total", "Observed compile-slot queue milliseconds.", snapshot.DistCCQueueMillis},
	}
	for _, counter := range counters {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n", counter.name, counter.help)
		_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", counter.name)
		_, _ = fmt.Fprintf(w, "%s %d\n", counter.name, counter.value)
	}
	_, _ = fmt.Fprintln(w, "# HELP portage_distcc_failures_total Observed distcc failures by bounded reason.")
	_, _ = fmt.Fprintln(w, "# TYPE portage_distcc_failures_total counter")
	for _, reason := range []string{
		"capacity", "connect", "lease-expired", "lease-fenced",
		"pool-mismatch", "remote-compile", "worker-stale", "policy",
		"unknown",
	} {
		_, _ = fmt.Fprintf(w,
			"portage_distcc_failures_total{reason=%q} %d\n",
			reason, snapshot.DistCCFailures[reason],
		)
	}
}

// GetSnapshot returns a snapshot of current metrics.
func (m *Metrics) GetSnapshot() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.enabled.Load() {
		return map[string]interface{}{
			"enabled": false,
		}
	}

	return map[string]interface{}{
		"enabled":             true,
		"builds_total":        m.buildsTotal.Value(),
		"builds_succeeded":    m.buildsSucceeded.Value(),
		"builds_failed":       m.buildsFailed.Value(),
		"builds_queued":       m.buildsQueued.Value(),
		"builders_active":     m.buildersActive.Value(),
		"builders_healthy":    m.buildersHealthy.Value(),
		"builder_capacity":    m.builderCapacity.Value(),
		"heartbeats_total":    m.heartbeatsTotal.Value(),
		"heartbeats_failed":   m.heartbeatsFailed.Value(),
		"packages_stored":     m.packagesStored.Value(),
		"storage_reads":       m.storageReads.Value(),
		"storage_writes":      m.storageWrites.Value(),
		"storage_errors":      m.storageErrors.Value(),
		"http_requests_total": m.httpRequests.Value(),
		"http_request_errors": m.httpRequestErrors.Value(),
		"goroutines":          m.goroutines.Value(),
		"uptime_seconds":      time.Since(m.startTime).Seconds(),
	}
}
