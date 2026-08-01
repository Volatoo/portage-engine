package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/binpkg"
	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/iam"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestLoadBuildCatalog(t *testing.T) {
	t.Run("configured example", func(t *testing.T) {
		cfg := &config.ServerConfig{CatalogPath: filepath.Join("..", "..", "configs", "catalog.example.json")}
		s := New(cfg)
		defer s.builder.Shutdown()
		if err := s.loadBuildCatalog(); err != nil {
			t.Fatalf("loadBuildCatalog failed: %v", err)
		}
		buildCatalog := s.builder.BuildCatalog()
		if _, err := buildCatalog.ResolveAt(catalog.ResolveRequest{ProfileID: "pe/amd64/glibc/systemd/base-v1"}, buildCatalog.MirrorBundles[0].CreatedAt.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "not published") {
			t.Fatalf("candidate example catalog was accepted for a build: %v", err)
		}
	})

	t.Run("invalid configured catalog fails closed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "catalog.json")
		if err := os.WriteFile(path, []byte(`{"version":1,"unknown":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		s := New(&config.ServerConfig{CatalogPath: path})
		defer s.builder.Shutdown()
		if err := s.loadBuildCatalog(); err == nil {
			t.Fatal("invalid configured catalog was accepted")
		}
	})
}

func TestInitializeRejectsInvalidCatalogBeforePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(&config.ServerConfig{CatalogPath: path, DataDir: t.TempDir(), BinpkgPath: t.TempDir()})
	defer s.Shutdown()
	if err := s.Initialize(); err == nil {
		t.Fatal("invalid catalog was accepted")
	}
	if s.persister != nil || s.store != nil {
		t.Fatal("persistence started before catalog validation")
	}
}

// TestNew tests creating a new server.
func TestNew(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
	}

	server := New(cfg)
	if server == nil {
		t.Fatal("New returned nil")
	}

	if server.config != cfg {
		t.Error("Config not set correctly")
	}

	if server.binpkgStore == nil {
		t.Error("binpkgStore not initialized")
	}

	if server.builder == nil {
		t.Error("builder not initialized")
	}
}

func TestCatalogBinhostStoresAndInventory(t *testing.T) {
	root := t.TempDir()
	cfg := &config.ServerConfig{
		BinpkgPath:  root,
		CatalogPath: filepath.Join("..", "..", "configs", "catalog.example.json"),
	}
	server := New(cfg)
	defer server.builder.Shutdown()
	if err := server.loadBuildCatalog(); err != nil {
		t.Fatal(err)
	}
	if err := server.configureBinhostStores(); err != nil {
		t.Fatal(err)
	}
	server.refreshBinhostIndexes("test")

	for _, rel := range []string{
		"releases/amd64/binpackages/23.0/x86-64_pe-systemd-base-v1/Packages",
		"releases/amd64/binpackages/23.0/x86-64_pe-desktop-verifier-v1/Packages",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("missing profile Packages at %s: %v", rel, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/binhosts", nil)
	w := httptest.NewRecorder()
	server.handleBinhostInventory(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("inventory returned %d: %s", w.Code, w.Body.String())
	}
	var inventory struct {
		Binhosts []binhostProfile `json:"binhosts"`
	}
	if err := json.NewDecoder(w.Body).Decode(&inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Binhosts) != 2 || !inventory.Binhosts[0].Default {
		t.Fatalf("unexpected inventory: %#v", inventory.Binhosts)
	}
	if inventory.Binhosts[0].SyncPath != "/binpkgs/"+inventory.Binhosts[0].BinhostPath {
		t.Fatalf("sync path does not bind the profile namespace: %#v", inventory.Binhosts[0])
	}
}

// TestRouter tests the HTTP router setup.
func TestRouter(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
	}

	server := New(cfg)
	router := server.Router()

	if router == nil {
		t.Fatal("Router returned nil")
	}
}

func TestIAMRouteClassification(t *testing.T) {
	for _, path := range []string{
		"/readyz",
		"/livez",
		"/metrics/prometheus",
		"/api/v1/binhosts",
		"/api/v1/gpg/public-key",
		"/api/v1/public/packages",
		"/api/v1/public/status",
		"/api/v1/iam/exchange",
		"/api/v1/iam/device/authorization",
		"/api/v1/iam/device/token",
		"/api/v1/iam/providers/authentik/backchannel-logout",
		"/binpkgs/releases/amd64/binpackages/23.0/Packages",
	} {
		if !publicControlPlanePath(path) {
			t.Errorf("publicControlPlanePath(%q) = false", path)
		}
	}
	for _, path := range []string{
		"/api/v1/iam/exchange/extra",
		"/api/v1/iam/device/decision",
		"/api/v1/iam/device/token/extra",
		"/api/v1/iam/providers/authentik/backchannel-logout/extra",
		"/api/v1/iam/providers/authentik/other",
		"/api/v1/iam/providers//backchannel-logout",
	} {
		if publicControlPlanePath(path) {
			t.Errorf("near-match IAM lifecycle route %q was public", path)
		}
	}
	for _, path := range []string{
		"/api/v1/packages/request-build",
		"/api/v1/builds/list",
		"/api/v1/cluster/status",
		"/api/v1/events/jobs",
	} {
		if publicControlPlanePath(path) || systemAdminPath(path) {
			t.Errorf("project route %q was classified as public or system-admin", path)
		}
	}
	for _, path := range []string{
		"/api/v1/settings/cloud",
		"/api/v1/instances",
		"/api/v1/worker-gateway/status",
		"/health",
	} {
		if !systemAdminPath(path) {
			t.Errorf("systemAdminPath(%q) = false", path)
		}
	}
	for _, test := range []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodGet, "/api/v1/settings/cloud", false},
		{http.MethodPut, "/api/v1/settings/cloud", true},
		{http.MethodPut, "/api/v1/projects/policy", true},
		{http.MethodDelete, "/api/v1/projects/members", true},
		{http.MethodPost, "/api/v1/heartbeat", false},
		{http.MethodDelete, "/api/v1/iam/sessions", false},
		{http.MethodPost, "/api/v1/iam/sessions/revoke-all", true},
		{http.MethodPost, "/api/v1/worker-gateway/certificates/revoke", true},
		{http.MethodPost, "/api/v1/worker-gateway/issuers/revoke", true},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		if got := stepUpRequired(request); got != test.want {
			t.Errorf("stepUpRequired(%s %s)=%t, want %t",
				test.method, test.path, got, test.want)
		}
	}
}

func TestLegacyHighRiskWriteRequiresIndependentStepUp(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: t.TempDir(), MaxWorkers: 1,
		APIKey: "primary-key", StepUpAPIKey: "second-key",
	}
	router := New(cfg).Router()
	request := func(stepUp string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(
			http.MethodPut, "/api/v1/settings/cloud", strings.NewReader(`{}`),
		)
		req.Header.Set("X-API-Key", "primary-key")
		req.Header.Set("Content-Type", "application/json")
		if stepUp != "" {
			req.Header.Set("X-Step-Up-Key", stepUp)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		return recorder
	}
	if result := request(""); result.Code != http.StatusPreconditionRequired ||
		!strings.Contains(result.Body.String(), "step_up_required") {
		t.Fatalf("missing step-up response=%d %s", result.Code, result.Body.String())
	}
	if result := request("primary-key"); result.Code != http.StatusPreconditionRequired {
		t.Fatalf("reused primary credential status=%d", result.Code)
	}
	if result := request("second-key"); result.Code == http.StatusPreconditionRequired ||
		result.Code == http.StatusUnauthorized {
		t.Fatalf("independent step-up was not accepted: %d %s",
			result.Code, result.Body.String())
	}
}

func TestOIDCStepUpPolicyRequiresFreshAcceptedContext(t *testing.T) {
	now := time.Now()
	server := New(&config.ServerConfig{
		BinpkgPath:          t.TempDir(),
		OIDCStepUpMaxAgeMin: 10,
		OIDCStepUpAMRValues: []string{"otp", "hwk"},
		OIDCStepUpACRValues: []string{"urn:example:mfa"},
	})
	principal := iam.Principal{
		Authentication:  "oidc",
		AuthenticatedAt: now.Add(-time.Minute),
		AMR:             []string{"pwd", "otp"},
		ACR:             "urn:example:mfa",
	}
	if !server.oidcStepUpSatisfied(principal, now) {
		t.Fatal("fresh OIDC MFA context did not satisfy step-up policy")
	}

	principal.AuthenticatedAt = now.Add(-11 * time.Minute)
	if server.oidcStepUpSatisfied(principal, now) {
		t.Fatal("stale OIDC authentication satisfied step-up policy")
	}
	principal.AuthenticatedAt = now.Add(-time.Minute)
	principal.AMR = []string{"pwd"}
	if server.oidcStepUpSatisfied(principal, now) {
		t.Fatal("OIDC authentication without an accepted AMR satisfied step-up")
	}
	principal.AMR = []string{"otp"}
	principal.ACR = "urn:example:password"
	if server.oidcStepUpSatisfied(principal, now) {
		t.Fatal("OIDC authentication without an accepted ACR satisfied step-up")
	}
}

func TestProjectClusterStatusDoesNotCrossProjects(t *testing.T) {
	now := time.Now()
	status := projectClusterStatus([]*builder.BuildStatus{
		{ProjectID: "alpha", Status: "building", InstanceID: "vm-alpha", CreatedAt: now},
		{ProjectID: "alpha", Status: "completed", CreatedAt: now},
		{ProjectID: "alpha", Status: "failed", CreatedAt: now},
		{ProjectID: "beta", Status: "queued", InstanceID: "vm-beta", CreatedAt: now},
	}, "alpha")
	if status.TotalBuilds != 3 || status.ActiveBuilds != 1 ||
		status.ActiveInstances != 1 || status.QueuedBuilds != 0 ||
		status.CompletedBuilds != 1 || status.FailedBuilds != 1 ||
		status.SuccessRate != 50 {
		t.Fatalf("project-scoped cluster status = %+v", status)
	}
}

// TestHandleHealth tests the health check endpoint.
func TestHandleHealth(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
	}

	server := New(cfg)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}

func TestHandleHealthIgnoresStoppedOnDemandBuilderHistory(t *testing.T) {
	cfg := &config.ServerConfig{BinpkgPath: t.TempDir()}
	server := New(cfg)
	server.builderRegistry.Register(&builder.BuilderInfo{
		ID: "retired-ephemeral-vm", Endpoint: "http://10.0.0.9:9090", Status: "offline",
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	server.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("on-demand builder history degraded control-plane health: status %d body %s",
			w.Code, w.Body.String())
	}
}

// TestHandlePackageQuery tests the package query endpoint.
func TestHandlePackageQuery(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
	}

	server := New(cfg)

	queryReq := binpkg.QueryRequest{
		Name:    "dev-lang/python",
		Version: "3.11.0",
		Arch:    "amd64",
	}

	body, err := json.Marshal(queryReq)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/packages/query", bytes.NewReader(body))
	w := httptest.NewRecorder()

	server.handlePackageQuery(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var queryResp binpkg.QueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&queryResp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
}

// TestHandlePackageQueryMethodNotAllowed tests method validation.
func TestHandlePackageQueryMethodNotAllowed(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
	}

	server := New(cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/packages/query", nil)
	w := httptest.NewRecorder()

	server.handlePackageQuery(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", resp.StatusCode)
	}
}

// TestHandleBuildRequest tests the build request endpoint.
func TestHandleBuildRequest(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
	}

	server := New(cfg)

	buildReq := builder.BuildRequest{
		PackageName: "app-editors/vim",
		Version:     "9.0",
		Arch:        "amd64",
	}

	body, err := json.Marshal(buildReq)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/packages/request-build", bytes.NewReader(body))
	w := httptest.NewRecorder()

	server.handleBuildRequest(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("Expected status 202, got %d", resp.StatusCode)
	}

	var buildResp builder.BuildResponse
	if err := json.NewDecoder(resp.Body).Decode(&buildResp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if buildResp.JobID == "" {
		t.Error("Expected non-empty job ID")
	}
}

// TestHandleBuildStatus tests the build status endpoint.
func TestHandleBuildStatus(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
	}

	server := New(cfg)

	// Submit a build first
	buildReq := builder.BuildRequest{
		PackageName: "sys-apps/portage",
		Version:     "3.0.0",
		Arch:        "amd64",
	}

	body, _ := json.Marshal(buildReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/packages/request-build", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleBuildRequest(w, req)

	var buildResp builder.BuildResponse
	_ = json.NewDecoder(w.Result().Body).Decode(&buildResp)

	// Query the build status
	req = httptest.NewRequest(http.MethodGet, "/api/v1/packages/status?job_id="+buildResp.JobID, nil)
	w = httptest.NewRecorder()

	server.handleBuildStatus(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}

// TestHandleHeartbeat tests the heartbeat endpoint.
func TestHandleHeartbeat(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
		MaxWorkers: 2,
	}

	server := New(cfg)

	tests := []struct {
		name           string
		method         string
		body           interface{}
		expectedStatus int
	}{
		{
			name:   "valid heartbeat",
			method: http.MethodPost,
			body: builder.HeartbeatRequest{
				BuilderID:  "builder-1",
				Status:     "healthy",
				Endpoint:   "http://localhost:9090",
				Capacity:   4,
				ActiveJobs: 2,
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:           "method not allowed",
			method:         http.MethodGet,
			body:           nil,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:   "missing builder_id",
			method: http.MethodPost,
			body: builder.HeartbeatRequest{
				Status: "healthy",
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:   "missing status",
			method: http.MethodPost,
			body: builder.HeartbeatRequest{
				BuilderID: "builder-1",
			},
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body []byte
			if tt.body != nil {
				body, _ = json.Marshal(tt.body)
			}

			req := httptest.NewRequest(tt.method, "/api/v1/heartbeat", bytes.NewReader(body))
			w := httptest.NewRecorder()

			server.handleHeartbeat(w, req)

			resp := w.Result()
			if resp.StatusCode != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, resp.StatusCode)
			}

			if tt.expectedStatus == http.StatusOK {
				var heartbeatResp builder.HeartbeatResponse
				_ = json.NewDecoder(resp.Body).Decode(&heartbeatResp)

				if !heartbeatResp.Success {
					t.Error("Expected success=true")
				}
			}
		})
	}
}

// TestHandleHeartbeatInvalidJSON tests heartbeat with invalid JSON.
func TestHandleHeartbeatInvalidJSON(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
		MaxWorkers: 2,
	}

	server := New(cfg)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/heartbeat", bytes.NewReader([]byte("invalid json")))
	w := httptest.NewRecorder()

	server.handleHeartbeat(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", resp.StatusCode)
	}
}

// TestHandleBuilderRegister tests the builder registration endpoint.
func TestHandleBuilderRegister(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
	}

	server := New(cfg)

	tests := []struct {
		name           string
		method         string
		body           interface{}
		expectedStatus int
	}{
		{
			name:   "valid registration",
			method: http.MethodPost,
			body: builder.BuilderInfo{
				ID:           "builder-1",
				Endpoint:     "http://localhost:9090",
				Architecture: "amd64",
				Status:       "online",
				Capacity:     4,
				CurrentLoad:  0,
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:   "registration with metrics",
			method: http.MethodPost,
			body: builder.BuilderInfo{
				ID:           "builder-2",
				Endpoint:     "http://localhost:9091",
				Architecture: "arm64",
				Status:       "online",
				Capacity:     2,
				CPUUsage:     45.5,
				MemoryUsage:  60.2,
				DiskUsage:    55.0,
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:   "reject endpoint with credentials and path",
			method: http.MethodPost,
			body: builder.BuilderInfo{
				ID:       "builder-ssrf",
				Endpoint: "http://user:password@localhost:9090/metadata",
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "method not allowed",
			method:         http.MethodGet,
			body:           nil,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "invalid json",
			method:         http.MethodPost,
			body:           "invalid",
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body []byte
			if tt.body != nil {
				if str, ok := tt.body.(string); ok {
					body = []byte(str)
				} else {
					body, _ = json.Marshal(tt.body)
				}
			}

			req := httptest.NewRequest(tt.method, "/api/v1/builders/register", bytes.NewReader(body))
			w := httptest.NewRecorder()

			server.handleBuilderRegister(w, req)

			resp := w.Result()
			if resp.StatusCode != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, resp.StatusCode)
			}

			if tt.expectedStatus == http.StatusOK {
				var result map[string]interface{}
				_ = json.NewDecoder(resp.Body).Decode(&result)

				if success, ok := result["success"].(bool); !ok || !success {
					t.Error("Expected success=true")
				}
			}
		})
	}
}

// TestHandleBuildersList tests the builders list endpoint.
func TestHandleBuildersList(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
	}

	server := New(cfg)

	// Register some builders first
	builders := []builder.BuilderInfo{
		{
			ID:           "builder-1",
			Endpoint:     "http://localhost:9090",
			Architecture: "amd64",
			Status:       "online",
		},
		{
			ID:           "builder-2",
			Endpoint:     "http://localhost:9091",
			Architecture: "arm64",
			Status:       "online",
		},
	}

	for _, b := range builders {
		body, _ := json.Marshal(b)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/builders/register", bytes.NewReader(body))
		w := httptest.NewRecorder()
		server.handleBuilderRegister(w, req)
	}

	// Test list endpoint
	tests := []struct {
		name           string
		method         string
		expectedStatus int
		expectBuilders bool
	}{
		{
			name:           "get builders list",
			method:         http.MethodGet,
			expectedStatus: http.StatusOK,
			expectBuilders: true,
		},
		{
			name:           "method not allowed",
			method:         http.MethodPost,
			expectedStatus: http.StatusMethodNotAllowed,
			expectBuilders: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/v1/builders/list", nil)
			w := httptest.NewRecorder()

			server.handleBuildersList(w, req)

			resp := w.Result()
			if resp.StatusCode != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, resp.StatusCode)
			}

			if tt.expectBuilders {
				var result []*builder.BuilderInfo
				_ = json.NewDecoder(resp.Body).Decode(&result)

				if len(result) < 2 {
					t.Errorf("Expected at least 2 builders, got %d", len(result))
				}
			}
		})
	}
}

// TestHandleBuildersStatus tests the builders status endpoint.
func TestHandleBuildersStatus(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
		// No remote builders configured - should return empty list
	}

	server := New(cfg)

	// Test status endpoint
	tests := []struct {
		name           string
		method         string
		expectedStatus int
	}{
		{
			name:           "get builders status",
			method:         http.MethodGet,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "method not allowed",
			method:         http.MethodPost,
			expectedStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/v1/builders/status", nil)
			w := httptest.NewRecorder()

			server.handleBuildersStatus(w, req)

			resp := w.Result()
			if resp.StatusCode != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, resp.StatusCode)
			}

			if tt.expectedStatus == http.StatusOK {
				var result map[string]interface{}
				_ = json.NewDecoder(resp.Body).Decode(&result)

				// Check stats
				stats, ok := result["stats"].(map[string]interface{})
				if !ok {
					t.Fatal("Expected stats in response")
				}

				// With no remote builders configured, total should be 0
				if stats["total_builders"].(float64) != 0 {
					t.Errorf("Expected 0 total builders, got %v", stats["total_builders"])
				}
			}
		})
	}
}

func TestHandleBuildersStatusAuthenticatesToBuilder(t *testing.T) {
	const token = "builder-monitor-token"

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != token {
			http.Error(w, "missing builder token", http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]interface{}{
			"instance_id": "builder-secure-1",
			"status":      "online",
			"capacity":    2,
		})
	}))
	defer remote.Close()

	server := New(&config.ServerConfig{
		BinpkgPath:     t.TempDir(),
		MaxWorkers:     0,
		BuilderToken:   token,
		RemoteBuilders: []string{remote.URL},
	})
	defer server.builder.Shutdown()

	w := httptest.NewRecorder()
	server.handleBuildersStatus(w, httptest.NewRequest(http.MethodGet, "/api/v1/builders/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var response struct {
		Builders []BuilderStatusInfo `json:"builders"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Builders) != 1 {
		t.Fatalf("expected one builder, got %d", len(response.Builders))
	}
	if response.Builders[0].ID != "builder-secure-1" {
		t.Fatalf("unexpected builder ID %q", response.Builders[0].ID)
	}
	if response.Builders[0].Status != "online" {
		t.Fatalf("expected online builder, got %q", response.Builders[0].Status)
	}
}

// TestCalculateBuilderStats tests the builder stats calculation.
func TestCalculateBuilderStats(t *testing.T) {
	builders := []BuilderStatusInfo{
		{
			ID:            "builder-1",
			Status:        "online",
			Capacity:      4,
			CurrentLoad:   2,
			TotalBuilds:   100,
			SuccessBuilds: 95,
			FailedBuilds:  5,
		},
		{
			ID:            "builder-2",
			Status:        "busy",
			Capacity:      2,
			CurrentLoad:   2,
			TotalBuilds:   50,
			SuccessBuilds: 48,
			FailedBuilds:  2,
		},
		{
			ID:            "builder-3",
			Status:        "offline",
			Capacity:      4,
			CurrentLoad:   0,
			TotalBuilds:   30,
			SuccessBuilds: 25,
			FailedBuilds:  5,
		},
		{
			ID:              "builder-4",
			Status:          "draining",
			Capacity:        0,
			AcceptingBuilds: false,
			TotalBuilds:     1,
			SuccessBuilds:   1,
		},
	}

	stats := calculateBuilderStats(builders)

	if stats["total_builders"].(int) != 4 {
		t.Errorf("Expected 4 total builders, got %v", stats["total_builders"])
	}

	if stats["online_builders"].(int) != 2 {
		t.Errorf("Expected 2 online builders, got %v", stats["online_builders"])
	}

	if stats["offline_builders"].(int) != 1 {
		t.Errorf("Expected 1 offline builder, got %v", stats["offline_builders"])
	}
	if stats["draining_builders"].(int) != 1 {
		t.Errorf("Expected 1 draining builder, got %v", stats["draining_builders"])
	}

	if stats["total_capacity"].(int) != 10 {
		t.Errorf("Expected capacity 10, got %v", stats["total_capacity"])
	}

	if stats["total_load"].(int) != 4 {
		t.Errorf("Expected load 4, got %v", stats["total_load"])
	}

	if stats["total_builds"].(int) != 181 {
		t.Errorf("Expected 181 total builds, got %v", stats["total_builds"])
	}

	expectedSuccessRate := float64(169) / float64(181) * 100
	if stats["success_rate"].(float64) != expectedSuccessRate {
		t.Errorf("Expected success rate %f, got %v", expectedSuccessRate, stats["success_rate"])
	}
}

// TestHelperFunctions tests the type conversion helper functions.
func TestHelperFunctions(t *testing.T) {
	m := map[string]interface{}{
		"string_val":  "test",
		"int_val":     42,
		"float_val":   3.14,
		"bool_val":    true,
		"int64_val":   int64(100),
		"float64_val": float64(99.9),
	}

	// Test getStringValue
	if v := getStringValue(m, "string_val", "default"); v != "test" {
		t.Errorf("Expected 'test', got '%s'", v)
	}
	if v := getStringValue(m, "missing", "default"); v != "default" {
		t.Errorf("Expected 'default', got '%s'", v)
	}

	// Test getIntValue
	if v := getIntValue(m, "int_val", 0); v != 42 {
		t.Errorf("Expected 42, got %d", v)
	}
	if v := getIntValue(m, "float64_val", 0); v != 99 {
		t.Errorf("Expected 99, got %d", v)
	}
	if v := getIntValue(m, "int64_val", 0); v != 100 {
		t.Errorf("Expected 100, got %d", v)
	}
	if v := getIntValue(m, "missing", 10); v != 10 {
		t.Errorf("Expected 10, got %d", v)
	}

	// Test getFloatValue
	if v := getFloatValue(m, "float_val", 0); v != 3.14 {
		t.Errorf("Expected 3.14, got %f", v)
	}
	if v := getFloatValue(m, "int_val", 0); v != 42.0 {
		t.Errorf("Expected 42.0, got %f", v)
	}
	if v := getFloatValue(m, "missing", 1.5); v != 1.5 {
		t.Errorf("Expected 1.5, got %f", v)
	}

	// Test getBoolValue
	if v := getBoolValue(m, "bool_val", false); v != true {
		t.Errorf("Expected true, got %v", v)
	}
	if v := getBoolValue(m, "missing", true); v != true {
		t.Errorf("Expected true, got %v", v)
	}
}

// TestBuilderRegistryIntegration tests the integration between server and builder registry.
func TestBuilderRegistryIntegration(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
	}

	server := New(cfg)

	// Register a builder
	builderInfo := builder.BuilderInfo{
		ID:           "test-builder",
		Endpoint:     "http://localhost:9090",
		Architecture: "amd64",
		Status:       "online",
		Capacity:     4,
		CurrentLoad:  1,
	}

	body, _ := json.Marshal(builderInfo)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/builders/register", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleBuilderRegister(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Registration failed: %d", w.Code)
	}

	// Send heartbeat to update the builder
	heartbeat := builder.HeartbeatRequest{
		BuilderID:  "test-builder",
		Status:     "busy",
		Endpoint:   "http://localhost:9090",
		Capacity:   4,
		ActiveJobs: 3,
	}

	body, _ = json.Marshal(heartbeat)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/heartbeat", bytes.NewReader(body))
	w = httptest.NewRecorder()
	server.handleHeartbeat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Heartbeat failed: %d", w.Code)
	}

	// Verify the builder was updated in the registry
	info, exists := server.builderRegistry.Get("test-builder")
	if !exists {
		t.Error("Builder not found in registry")
	}
	if info.Status != "busy" {
		t.Errorf("Expected status 'busy', got %v", info.Status)
	}
	if info.CurrentLoad != 3 {
		t.Errorf("Expected current_load 3, got %v", info.CurrentLoad)
	}
}

// TestHandleGPGPublicKey tests the GPG public key endpoint.
func TestHandleGPGPublicKey(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		gpgEnabled     bool
		expectedStatus int
	}{
		{
			name:           "method not allowed",
			method:         http.MethodPost,
			gpgEnabled:     true,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "gpg not enabled",
			method:         http.MethodGet,
			gpgEnabled:     false,
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "no key configured",
			method:         http.MethodGet,
			gpgEnabled:     true,
			expectedStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.ServerConfig{
				BinpkgPath: "/tmp/binpkgs",
				GPGEnabled: tt.gpgEnabled,
			}

			server := New(cfg)

			req := httptest.NewRequest(tt.method, "/api/v1/gpg/public-key", nil)
			w := httptest.NewRecorder()

			server.handleGPGPublicKey(w, req)

			resp := w.Result()
			if resp.StatusCode != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, resp.StatusCode)
			}
		})
	}
}

// TestHandleArtifactInfo tests the artifact info endpoint.
func TestHandleArtifactInfo(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath:     "/tmp/binpkgs",
		RemoteBuilders: []string{"http://localhost:9090"},
	}

	server := New(cfg)

	// Test method not allowed
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/info/test-job-id", nil)
	w := httptest.NewRecorder()

	server.handleArtifactInfo(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", resp.StatusCode)
	}

	// Test missing job ID
	req = httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/info/", nil)
	w = httptest.NewRecorder()

	server.handleArtifactInfo(w, req)

	resp = w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400 for missing job ID, got %d", resp.StatusCode)
	}
}

// TestHandleArtifactDownload tests the artifact download endpoint.
func TestHandleArtifactDownload(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath:     "/tmp/binpkgs",
		RemoteBuilders: []string{"http://localhost:9090"},
	}

	server := New(cfg)

	// Test method not allowed
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/download/test-job-id", nil)
	w := httptest.NewRecorder()

	server.handleArtifactDownload(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", resp.StatusCode)
	}

	// Test missing job ID
	req = httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/download/", nil)
	w = httptest.NewRecorder()

	server.handleArtifactDownload(w, req)

	resp = w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400 for missing job ID, got %d", resp.StatusCode)
	}
}

// TestGetBuilderURLForJob tests the getBuilderURLForJob helper.
func TestGetBuilderURLForJob(t *testing.T) {
	// Test with no builders and no remote builders configured
	cfg := &config.ServerConfig{
		BinpkgPath: "/tmp/binpkgs",
	}

	server := New(cfg)

	_, err := server.getBuilderURLForJob("test-job")
	if err == nil {
		t.Error("Expected error when no builders configured")
	}

	// Test with remote builders configured
	cfg.RemoteBuilders = []string{"http://builder1:9090", "http://builder2:9090"}
	server = New(cfg)

	url, err := server.getBuilderURLForJob("test-job")
	if err != nil {
		t.Errorf("Expected no error with remote builders, got: %v", err)
	}
	if url != "http://builder1:9090" {
		t.Errorf("Expected first remote builder, got: %s", url)
	}
}

// TestHandleSubmitBuildWithConfig verifies the config-bundle submit endpoint
// (used by cmd/client) accepts a bundle and returns the actual early job state
// rather than the former 501 Not Implemented response.
func TestHandleSubmitBuildWithConfig(t *testing.T) {
	cfg := &config.ServerConfig{BinpkgPath: "/tmp/binpkgs", MaxWorkers: 1}
	server := New(cfg)
	defer server.builder.Shutdown()

	req := builder.LocalBuildRequest{
		PackageName: "dev-lang/python",
		Version:     "3.11.0",
		ConfigBundle: &builder.ConfigBundle{
			Config: &builder.PortageConfig{},
			Packages: &builder.BuildPackageSpec{
				Packages: []builder.PackageSpec{{Atom: "dev-lang/python", Version: "3.11.0"}},
			},
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/builds/submit", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleSubmitBuildWithConfig(w, httpReq)

	resp := w.Result()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
	var out struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.JobID == "" {
		t.Error("expected a job_id in the response")
	}
	validStatus := map[string]bool{
		"queued": true, "claimed": true, "provisioning": true,
		"deploying": true, "forwarding": true, "building": true,
		"collecting": true, "verifying": true, "signing": true, "publishing": true,
		"completed": true, "success": true, "failed": true,
	}
	if !validStatus[out.Status] {
		t.Errorf("unexpected early job status %q", out.Status)
	}
}

// TestHandleSubmitBuildWithConfig_RejectsEmptyBundle verifies validation.
func TestHandleSubmitBuildWithConfig_RejectsEmptyBundle(t *testing.T) {
	server := New(&config.ServerConfig{BinpkgPath: "/tmp/binpkgs", MaxWorkers: 1})

	body, _ := json.Marshal(builder.LocalBuildRequest{PackageName: "x/y"})
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/builds/submit", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleSubmitBuildWithConfig(w, httpReq)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing bundle, got %d", w.Result().StatusCode)
	}
}

func TestHandleSubmitBuildWithConfig_RejectsUnknownCatalogProfile(t *testing.T) {
	server := New(&config.ServerConfig{BinpkgPath: t.TempDir(), MaxWorkers: 0})
	req := builder.LocalBuildRequest{
		PackageName: "dev-lang/python",
		Version:     "3.11.0",
		ProfileID:   "unknown/profile",
		ConfigBundle: &builder.ConfigBundle{
			Config: &builder.PortageConfig{},
			Packages: &builder.BuildPackageSpec{Packages: []builder.PackageSpec{{
				Atom: "dev-lang/python", Version: "3.11.0",
			}}},
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/builds/submit", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleSubmitBuildWithConfig(w, httpReq)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown catalog profile should return 400, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
}

func TestHandleSubmitBuildWithConfig_RejectsUnknownFields(t *testing.T) {
	server := New(&config.ServerConfig{BinpkgPath: t.TempDir(), MaxWorkers: 1})
	body := `{
		"package_name":"dev-lang/python",
		"config_bundle":{
			"config":{"unexpected_policy_override":"allow"},
			"packages":{"packages":[{"atom":"dev-lang/python"}]},
			"metadata":{}
		}
	}`
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/builds/submit", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.handleSubmitBuildWithConfig(w, httpReq)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown nested field, got %d", w.Result().StatusCode)
	}
}

func TestHandleSubmitBuildWithConfig_RejectsOversizedBody(t *testing.T) {
	server := New(&config.ServerConfig{BinpkgPath: t.TempDir(), MaxWorkers: 1})
	body := `{"package_name":"dev-lang/python","padding":"` +
		strings.Repeat("x", int(builder.MaxBuildRequestBodyBytes)) + `"}`
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/builds/submit", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.handleSubmitBuildWithConfig(w, httpReq)

	if w.Result().StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d", w.Result().StatusCode)
	}
}

// TestAPIKeyAuthMiddleware verifies the API-key auth layer via the real Router:
// missing/wrong keys are rejected (constant-time compare), the correct key is
// accepted, and minimal probes/binhost endpoints bypass auth. The detailed
// /health inventory is system-admin-only.
func TestAPIKeyAuthMiddleware(t *testing.T) {
	cfg := &config.ServerConfig{
		BinpkgPath: t.TempDir(),
		MaxWorkers: 1,
		APIKey:     "s3cr3t-key",
	}
	router := New(cfg).Router()

	do := func(method, path, key string) int {
		req := httptest.NewRequest(method, path, nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Result().StatusCode
	}

	// Protected endpoint: no key -> 401.
	if got := do(http.MethodGet, "/api/v1/builds/list", ""); got != http.StatusUnauthorized {
		t.Errorf("no key: expected 401, got %d", got)
	}
	// Wrong key -> 401.
	if got := do(http.MethodGet, "/api/v1/builds/list", "wrong"); got != http.StatusUnauthorized {
		t.Errorf("wrong key: expected 401, got %d", got)
	}
	// Correct key -> not 401 (200-class or handler-specific, but authorized).
	if got := do(http.MethodGet, "/api/v1/builds/list", "s3cr3t-key"); got == http.StatusUnauthorized {
		t.Errorf("correct key: unexpectedly 401")
	}
	// Detailed health is operational inventory and requires system-admin auth.
	if got := do(http.MethodGet, "/health", ""); got != http.StatusUnauthorized {
		t.Errorf("/health without credentials: expected 401, got %d", got)
	}
	if got := do(http.MethodGet, "/health", "s3cr3t-key"); got == http.StatusUnauthorized {
		t.Errorf("/health with administrator key unexpectedly returned 401")
	}
	// Load-balancer probes remain intentionally public.
	if got := do(http.MethodGet, "/livez", ""); got != http.StatusOK {
		t.Errorf("/livez should bypass auth, got %d", got)
	}
	// Binhost bypasses auth (emerge can't present a key).
	if got := do(http.MethodGet, "/binpkgs/Packages", ""); got == http.StatusUnauthorized {
		t.Errorf("/binpkgs/ should bypass auth, got 401")
	}
	if got := do(http.MethodGet, "/api/v1/binhosts", ""); got == http.StatusUnauthorized {
		t.Errorf("/api/v1/binhosts should bypass auth, got 401")
	}
	if got := do(http.MethodGet, "/verify-binhost/unknown/Packages", ""); got != http.StatusNotFound {
		t.Errorf("unknown verification capability should bypass API auth and fail as 404, got %d", got)
	}
}

func TestShellOriginPolicy(t *testing.T) {
	s := New(&config.ServerConfig{
		BinpkgPath: t.TempDir(),
		MaxWorkers: 1,
		CORSAllowedOrigins: []string{
			"https://dashboard.example.test",
		},
	})
	for _, test := range []struct {
		name   string
		host   string
		origin string
		want   bool
	}{
		{name: "server-side proxy", host: "api.internal", want: true},
		{name: "same origin", host: "api.example.test", origin: "https://api.example.test", want: true},
		{name: "configured dashboard", host: "api.internal", origin: "https://dashboard.example.test", want: true},
		{name: "foreign browser", host: "api.internal", origin: "https://evil.example.test", want: false},
		{name: "malformed", host: "api.internal", origin: "not a URL", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://"+test.host+"/api/v1/instances/shell", nil)
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if got := s.shellOriginAllowed(request); got != test.want {
				t.Fatalf("shell origin allowed=%t, want %t", got, test.want)
			}
		})
	}
}

// TestHandleBuildRequestRejectsEmptyPackage verifies empty package requests are
// rejected with 400 rather than creating an empty queued job.
func TestHandleBuildRequestRejectsEmptyPackage(t *testing.T) {
	server := New(&config.ServerConfig{BinpkgPath: t.TempDir(), MaxWorkers: 1})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/packages/request-build", bytes.NewReader([]byte(`{"version":"1.0"}`)))
	w := httptest.NewRecorder()
	server.handleBuildRequest(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("empty package: expected 400, got %d", w.Result().StatusCode)
	}
}
