package metrics

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPrometheusSchedulerSnapshot(t *testing.T) {
	m := New(&Config{Enabled: true})
	m.SetSchedulerProvider(func() SchedulerSnapshot {
		return SchedulerSnapshot{
			QueuedTasks: 7, StarvedProjects: 2,
			WorkerDecisions: 9, WorkerMultiCandidate: 4,
			TargetSamples30d: 12, TargetSuccesses30d: 10,
			TargetFailures30d: 2, TargetSLOBreaches30d: 1,
			TargetReservedCost30d: 9000, TargetChargedCost30d: 7500,
			AutoscaleActiveSlots: 4, AutoscaleDesiredSlots: 6,
			AutoscalePools: 3, AutoscaleBlockedPools: 1,
			CapacityOpenActions: 2, CapacityProvisioning: 1,
			CapacityActive: 4, CapacityDraining: 1, CapacityDeleting: 1,
		}
	})
	request := httptest.NewRequest(
		http.MethodGet, "/metrics/prometheus", nil,
	)
	response := httptest.NewRecorder()
	m.PrometheusHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("prometheus status=%d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		"portage_scheduler_queued_tasks 7",
		"portage_scheduler_fair_starved_projects 2",
		"portage_scheduler_worker_decisions_last_hour 9",
		"portage_scheduler_worker_multi_candidate_last_hour 4",
		"portage_monitor_target_samples_30d 12",
		"portage_monitor_target_successes_30d 10",
		"portage_monitor_target_failures_30d 2",
		"portage_monitor_target_slo_breaches_30d 1",
		"portage_monitor_target_reserved_cost_microunits_30d 9000",
		"portage_monitor_target_charged_cost_microunits_30d 7500",
		"portage_scheduler_autoscale_active_slots 4",
		"portage_scheduler_autoscale_desired_slots 6",
		"portage_scheduler_autoscale_pools 3",
		"portage_scheduler_autoscale_blocked_pools 1",
		"portage_capacity_actuator_open_actions 2",
		"portage_capacity_instances_provisioning 1",
		"portage_capacity_instances_active 4",
		"portage_capacity_instances_draining 1",
		"portage_capacity_instances_deleting 1",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("prometheus body missing %q", expected)
		}
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		enabled bool
	}{
		{
			name:    "enabled metrics",
			cfg:     &Config{Enabled: true, Port: "2112"},
			enabled: true,
		},
		{
			name:    "disabled metrics",
			cfg:     &Config{Enabled: false, Port: "2112"},
			enabled: false,
		},
		{
			name:    "nil config",
			cfg:     nil,
			enabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New(tt.cfg)
			if m == nil {
				t.Fatal("Expected non-nil Metrics")
			}
			if m.IsEnabled() != tt.enabled {
				t.Errorf("Expected enabled=%v, got %v", tt.enabled, m.IsEnabled())
			}
		})
	}
}

func TestBuildMetrics(t *testing.T) {
	m := New(&Config{Enabled: true})

	m.IncBuildsTotal()
	m.IncBuildsSucceeded()
	m.IncBuildsFailed()
	m.SetBuildsQueued(5)
	m.RecordBuildDuration("test-package", 100*time.Millisecond)

	snapshot := m.GetSnapshot()
	if snapshot["builds_total"].(int64) != 1 {
		t.Errorf("Expected builds_total=1, got %v", snapshot["builds_total"])
	}
	if snapshot["builds_succeeded"].(int64) != 1 {
		t.Errorf("Expected builds_succeeded=1, got %v", snapshot["builds_succeeded"])
	}
	if snapshot["builds_failed"].(int64) != 1 {
		t.Errorf("Expected builds_failed=1, got %v", snapshot["builds_failed"])
	}
	if snapshot["builds_queued"].(int64) != 5 {
		t.Errorf("Expected builds_queued=5, got %v", snapshot["builds_queued"])
	}
}

func TestBuilderMetrics(t *testing.T) {
	m := New(&Config{Enabled: true})

	m.SetBuildersActive(3)
	m.SetBuildersHealthy(2)
	m.SetBuilderCapacity(10)
	m.IncHeartbeatsTotal()
	m.IncHeartbeatsFailed()

	snapshot := m.GetSnapshot()
	if snapshot["builders_active"].(int64) != 3 {
		t.Errorf("Expected builders_active=3, got %v", snapshot["builders_active"])
	}
	if snapshot["builders_healthy"].(int64) != 2 {
		t.Errorf("Expected builders_healthy=2, got %v", snapshot["builders_healthy"])
	}
	if snapshot["builder_capacity"].(int64) != 10 {
		t.Errorf("Expected builder_capacity=10, got %v", snapshot["builder_capacity"])
	}
	if snapshot["heartbeats_total"].(int64) != 1 {
		t.Errorf("Expected heartbeats_total=1, got %v", snapshot["heartbeats_total"])
	}
	if snapshot["heartbeats_failed"].(int64) != 1 {
		t.Errorf("Expected heartbeats_failed=1, got %v", snapshot["heartbeats_failed"])
	}
}

func TestStorageMetrics(t *testing.T) {
	m := New(&Config{Enabled: true})

	m.IncPackagesStored()
	m.IncStorageReads()
	m.IncStorageWrites()
	m.IncStorageErrors()

	snapshot := m.GetSnapshot()
	if snapshot["packages_stored"].(int64) != 1 {
		t.Errorf("Expected packages_stored=1, got %v", snapshot["packages_stored"])
	}
	if snapshot["storage_reads"].(int64) != 1 {
		t.Errorf("Expected storage_reads=1, got %v", snapshot["storage_reads"])
	}
	if snapshot["storage_writes"].(int64) != 1 {
		t.Errorf("Expected storage_writes=1, got %v", snapshot["storage_writes"])
	}
	if snapshot["storage_errors"].(int64) != 1 {
		t.Errorf("Expected storage_errors=1, got %v", snapshot["storage_errors"])
	}
}

func TestHTTPMetrics(t *testing.T) {
	m := New(&Config{Enabled: true})

	m.IncHTTPRequests()
	m.IncHTTPRequestErrors()
	m.RecordHTTPLatency("/api/test", 50*time.Millisecond)

	snapshot := m.GetSnapshot()
	if snapshot["http_requests_total"].(int64) != 1 {
		t.Errorf("Expected http_requests_total=1, got %v", snapshot["http_requests_total"])
	}
	if snapshot["http_request_errors"].(int64) != 1 {
		t.Errorf("Expected http_request_errors=1, got %v", snapshot["http_request_errors"])
	}
}

func TestSystemMetrics(t *testing.T) {
	m := New(&Config{Enabled: true})

	goroutineCount := int64(runtime.NumGoroutine())
	m.UpdateGoroutines(goroutineCount)

	snapshot := m.GetSnapshot()
	if snapshot["goroutines"].(int64) != goroutineCount {
		t.Errorf("Expected goroutines=%v, got %v", goroutineCount, snapshot["goroutines"])
	}

	uptime := snapshot["uptime_seconds"].(float64)
	if uptime <= 0 {
		t.Errorf("Expected positive uptime, got %v", uptime)
	}
}

func TestMetricsDisabled(t *testing.T) {
	m := New(&Config{Enabled: false})

	m.IncBuildsTotal()
	m.IncBuildsSucceeded()
	m.SetBuildersActive(5)

	snapshot := m.GetSnapshot()
	if snapshot["enabled"].(bool) {
		t.Error("Expected metrics to be disabled")
	}

	if len(snapshot) != 1 {
		t.Errorf("Expected only 'enabled' field, got %v", snapshot)
	}
}

func TestHandler(t *testing.T) {
	tests := []struct {
		name           string
		enabled        bool
		password       string
		authHeader     string
		expectedStatus int
	}{
		{
			name:           "enabled handler no password",
			enabled:        true,
			password:       "",
			authHeader:     "",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "enabled handler with password valid auth",
			enabled:        true,
			password:       "secret123",
			authHeader:     "Basic " + base64.StdEncoding.EncodeToString([]byte("metrics:secret123")),
			expectedStatus: http.StatusOK,
		},
		{
			name:           "enabled handler with password no auth",
			enabled:        true,
			password:       "secret123",
			authHeader:     "",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "enabled handler with password wrong password",
			enabled:        true,
			password:       "secret123",
			authHeader:     "Basic " + base64.StdEncoding.EncodeToString([]byte("metrics:wrongpass")),
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "enabled handler with password wrong username",
			enabled:        true,
			password:       "secret123",
			authHeader:     "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret123")),
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "disabled handler",
			enabled:        false,
			password:       "",
			authHeader:     "",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New(&Config{Enabled: tt.enabled, Password: tt.password})
			handler := m.Handler()

			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %v, got %v", tt.expectedStatus, w.Code)
			}

			// Check WWW-Authenticate header for unauthorized responses
			if tt.expectedStatus == http.StatusUnauthorized {
				authHeader := w.Header().Get("WWW-Authenticate")
				if authHeader == "" {
					t.Error("Expected WWW-Authenticate header for unauthorized response")
				}
			}
		})
	}
}

func TestConcurrentMetrics(t *testing.T) {
	m := New(&Config{Enabled: true})

	// Get initial values
	initialSnapshot := m.GetSnapshot()
	initialBuildsTotal := initialSnapshot["builds_total"].(int64)
	initialBuildsSucceeded := initialSnapshot["builds_succeeded"].(int64)
	initialHTTPRequests := initialSnapshot["http_requests_total"].(int64)

	var wg sync.WaitGroup
	iterations := 100

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				m.IncBuildsTotal()
				m.IncBuildsSucceeded()
				m.IncBuildsFailed()
				m.IncHeartbeatsTotal()
				m.IncPackagesStored()
				m.IncStorageReads()
				m.IncHTTPRequests()
			}
		}()
	}

	wg.Wait()

	snapshot := m.GetSnapshot()
	expected := int64(10 * iterations)

	if snapshot["builds_total"].(int64) != initialBuildsTotal+expected {
		t.Errorf("Expected builds_total=%v, got %v", initialBuildsTotal+expected, snapshot["builds_total"])
	}
	if snapshot["builds_succeeded"].(int64) != initialBuildsSucceeded+expected {
		t.Errorf("Expected builds_succeeded=%v, got %v", initialBuildsSucceeded+expected, snapshot["builds_succeeded"])
	}
	if snapshot["http_requests_total"].(int64) != initialHTTPRequests+expected {
		t.Errorf("Expected http_requests_total=%v, got %v", initialHTTPRequests+expected, snapshot["http_requests_total"])
	}
}

func TestGetSnapshotStructure(t *testing.T) {
	m := New(&Config{Enabled: true})

	snapshot := m.GetSnapshot()

	requiredFields := []string{
		"enabled",
		"builds_total",
		"builds_succeeded",
		"builds_failed",
		"builds_queued",
		"builders_active",
		"builders_healthy",
		"builder_capacity",
		"heartbeats_total",
		"heartbeats_failed",
		"packages_stored",
		"storage_reads",
		"storage_writes",
		"storage_errors",
		"http_requests_total",
		"http_request_errors",
		"goroutines",
		"uptime_seconds",
	}

	for _, field := range requiredFields {
		if _, ok := snapshot[field]; !ok {
			t.Errorf("Missing required field: %s", field)
		}
	}
}

func TestPasswordUpdate(t *testing.T) {
	// Create metrics with no password
	m := New(&Config{Enabled: true, Password: ""})

	// Test without password
	handler := m.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %v without password, got %v", http.StatusOK, w.Code)
	}

	// Update metrics with password
	m = New(&Config{Enabled: true, Password: "newpass"})

	// Test with wrong password
	handler = m.Handler()
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status %v without auth, got %v", http.StatusUnauthorized, w.Code)
	}

	// Test with correct password
	handler = m.Handler()
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("metrics:newpass")))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %v with correct password, got %v", http.StatusOK, w.Code)
	}
}

func TestPasswordEdgeCases(t *testing.T) {
	tests := []struct {
		name           string
		password       string
		auth           string
		expectedStatus int
	}{
		{
			name:           "empty password empty auth",
			password:       "",
			auth:           "",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "password with spaces",
			password:       "pass word",
			auth:           "Basic " + base64.StdEncoding.EncodeToString([]byte("metrics:pass word")),
			expectedStatus: http.StatusOK,
		},
		{
			name:           "password with special chars",
			password:       "p@ss!w0rd#123",
			auth:           "Basic " + base64.StdEncoding.EncodeToString([]byte("metrics:p@ss!w0rd#123")),
			expectedStatus: http.StatusOK,
		},
		{
			name:           "malformed auth header",
			password:       "secret",
			auth:           "Bearer token123",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "invalid base64 auth",
			password:       "secret",
			auth:           "Basic invalid!!!",
			expectedStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New(&Config{Enabled: true, Password: tt.password})
			handler := m.Handler()

			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %v, got %v", tt.expectedStatus, w.Code)
			}
		})
	}
}

// TestConcurrentNewAndEnabledAccess exercises concurrent New calls (which toggle
// the enabled flag) alongside recorders, IsEnabled and Handler reads to catch
// data races on the enabled flag (run with -race).
func TestConcurrentNewAndEnabledAccess(_ *testing.T) {
	var wg sync.WaitGroup

	// Goroutines that repeatedly reconfigure via New, toggling enabled and
	// password.
	for i := 0; i < 4; i++ {
		enabled := i%2 == 0
		pw := ""
		if i%2 == 1 {
			pw = "secret"
		}
		wg.Add(1)
		go func(en bool, password string) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m := New(&Config{Enabled: en, Password: password})
				_ = m.IsEnabled()
			}
		}(enabled, pw)
	}

	// Goroutines that record metrics and build handlers concurrently.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m := New(&Config{Enabled: true})
				m.IncBuildsTotal()
				m.IncHTTPRequests()
				_ = m.Handler()
				_ = m.GetSnapshot()
			}
		}()
	}

	wg.Wait()
}
