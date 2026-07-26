package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/pkg/config"
)

// TestNew tests creating a new dashboard.
func TestNew(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL:      "http://localhost:8080",
		AuthEnabled:    false,
		AllowAnonymous: true,
	}

	dashboard := New(cfg)
	if dashboard == nil {
		t.Fatal("New returned nil")
	}

	if dashboard.config != cfg {
		t.Error("Config not set correctly")
	}

	if dashboard.templates == nil {
		t.Error("Templates not initialized")
	}

	if dashboard.httpClient == nil {
		t.Error("HTTP client not initialized")
	}
}

// TestRouter tests the HTTP router setup.
func TestRouter(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL:      "http://localhost:8080",
		AuthEnabled:    false,
		AllowAnonymous: true,
	}

	dashboard := New(cfg)
	router := dashboard.Router()

	if router == nil {
		t.Fatal("Router returned nil")
	}
}

// TestHandleIndex tests the index page handler.
func TestHandleIndex(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL:      "http://localhost:8080",
		AuthEnabled:    false,
		AllowAnonymous: true,
	}

	dashboard := New(cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	dashboard.handleLanding(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}

// TestHandleIndexNotFound tests 404 handling.
func TestHandleIndexNotFound(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL:      "http://localhost:8080",
		AuthEnabled:    false,
		AllowAnonymous: true,
	}

	dashboard := New(cfg)

	req := httptest.NewRequest(http.MethodGet, "/invalid-path", nil)
	w := httptest.NewRecorder()

	dashboard.handleLanding(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}
}

// TestHandleLogin tests the login handler with valid credentials.
func TestHandleLogin(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL:       "http://localhost:8080",
		AuthEnabled:     true,
		AllowAnonymous:  false,
		JWTSecret:       "test-secret-that-is-at-least-32-chars-long",
		AdminUser:       "testuser",
		AdminPassword:   "testpass",
		TokenTTLMinutes: 60,
	}

	dashboard := New(cfg)

	body, err := json.Marshal(map[string]string{"username": "testuser", "password": "testpass"})
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body))
	w := httptest.NewRecorder()

	dashboard.handleLoginRoute(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	// The issued token must verify against the configured secret.
	if err := verifyToken(cfg.JWTSecret, out["token"], time.Now()); err != nil {
		t.Errorf("issued token does not verify: %v", err)
	}
}

// TestHandleLoginRejectsBadCredentials ensures wrong credentials are refused.
func TestHandleLoginRejectsBadCredentials(t *testing.T) {
	cfg := &config.DashboardConfig{
		AuthEnabled:   true,
		JWTSecret:     "test-secret-that-is-at-least-32-chars-long",
		AdminUser:     "testuser",
		AdminPassword: "testpass",
	}
	dashboard := New(cfg)

	body, _ := json.Marshal(map[string]string{"username": "testuser", "password": "wrong"})
	req := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body))
	w := httptest.NewRecorder()

	dashboard.handleLoginRoute(w, req)

	if got := w.Result().StatusCode; got != http.StatusUnauthorized {
		t.Errorf("Expected status 401 for bad credentials, got %d", got)
	}
}

// TestHandleLoginMethodNotAllowed tests method validation.
func TestHandleLoginMethodNotAllowed(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL:      "http://localhost:8080",
		AuthEnabled:    true,
		AllowAnonymous: false,
	}

	dashboard := New(cfg)

	req := httptest.NewRequest(http.MethodDelete, "/login", nil)
	w := httptest.NewRecorder()

	dashboard.handleLoginRoute(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", resp.StatusCode)
	}
}

// TestHandleStatus tests the status API endpoint.
func TestHandleStatus(t *testing.T) {
	// Backend up: real data is proxied through as 200.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"active_builds": 1, "total_builds": 3})
	}))
	defer backend.Close()

	d := New(&config.DashboardConfig{ServerURL: backend.URL, AllowAnonymous: true})
	w := httptest.NewRecorder()
	d.handleStatus(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("backend up: expected 200, got %d", w.Result().StatusCode)
	}

	// Backend down: honest 502, NOT fabricated 200 data.
	d2 := New(&config.DashboardConfig{ServerURL: "http://127.0.0.1:1", AllowAnonymous: true})
	w2 := httptest.NewRecorder()
	d2.handleStatus(w2, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if w2.Result().StatusCode != http.StatusBadGateway {
		t.Errorf("backend down: expected 502, got %d", w2.Result().StatusCode)
	}
}

// TestHandleBuilds tests the builds API endpoint.
func TestHandleBuilds(t *testing.T) {
	// Backend down must be an honest 502, not fabricated sample builds.
	d := New(&config.DashboardConfig{ServerURL: "http://127.0.0.1:1", AllowAnonymous: true})
	w := httptest.NewRecorder()
	d.handleBuilds(w, httptest.NewRequest(http.MethodGet, "/api/builds", nil))
	if w.Result().StatusCode != http.StatusBadGateway {
		t.Errorf("backend down: expected 502, got %d", w.Result().StatusCode)
	}
	if strings.Contains(w.Body.String(), "sample-job") {
		t.Error("response contains fabricated sample data")
	}
}

// TestHandleKeyEndpointsProxyRealKey verifies the key endpoints serve the
// server's real GPG key (not the old hardcoded fake key).
func TestHandleKeyEndpointsProxyRealKey(t *testing.T) {
	const realKey = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nREALKEYDATA\n-----END PGP PUBLIC KEY BLOCK-----\n"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/gpg/public-key" {
			_, _ = w.Write([]byte(realKey))
			return
		}
		http.NotFound(w, r)
	}))
	defer backend.Close()

	d := New(&config.DashboardConfig{ServerURL: backend.URL, AllowAnonymous: true})

	w := httptest.NewRecorder()
	d.handlePublicKeyAPI(w, httptest.NewRequest(http.MethodGet, "/api/keys/public", nil))
	body := w.Body.String()
	if !strings.Contains(body, "REALKEYDATA") {
		t.Errorf("public key endpoint did not proxy the real key: %s", body)
	}
	if strings.Contains(body, "portage-engine-2024") {
		t.Error("public key endpoint still returns the hardcoded fake key")
	}

	// Backend down → 502, not a fake key.
	d2 := New(&config.DashboardConfig{ServerURL: "http://127.0.0.1:1", AllowAnonymous: true})
	w2 := httptest.NewRecorder()
	d2.handleKeyInfoAPI(w2, httptest.NewRequest(http.MethodGet, "/api/keys/info", nil))
	if w2.Result().StatusCode != http.StatusBadGateway {
		t.Errorf("key info with backend down: expected 502, got %d", w2.Result().StatusCode)
	}
}

// TestHandleInstances verifies the instances endpoint proxies the server's
// real instance list (backend up → data; backend down → honest 502).
func TestHandleInstances(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instances" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"pve-123","provider":"pve","status":"running","ip_address":"10.0.0.9"}]`))
	}))
	defer backend.Close()

	d := New(&config.DashboardConfig{ServerURL: backend.URL, AllowAnonymous: true})
	w := httptest.NewRecorder()
	d.handleInstances(w, httptest.NewRequest(http.MethodGet, "/api/instances", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("backend up: expected 200, got %d", w.Result().StatusCode)
	}
	if !strings.Contains(w.Body.String(), "pve-123") {
		t.Errorf("expected proxied instance data, got: %s", w.Body.String())
	}

	d2 := New(&config.DashboardConfig{ServerURL: "http://127.0.0.1:1", AllowAnonymous: true})
	w2 := httptest.NewRecorder()
	d2.handleInstances(w2, httptest.NewRequest(http.MethodGet, "/api/instances", nil))
	if w2.Result().StatusCode != http.StatusBadGateway {
		t.Errorf("backend down: expected 502, got %d", w2.Result().StatusCode)
	}
}

// TestAuthMiddleware verifies the middleware rejects missing/invalid tokens on
// protected routes and accepts a validly signed token.
func TestAuthMiddleware(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL:      "http://localhost:8080",
		AuthEnabled:    true,
		AllowAnonymous: false,
		JWTSecret:      "test-secret-that-is-at-least-32-chars-long",
	}

	dashboard := New(cfg)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := dashboard.authMiddleware(ok)

	// A protected path (not /, /login, or /static/) with no token → 401.
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if got := w.Result().StatusCode; got != http.StatusUnauthorized {
		t.Errorf("no token: expected 401, got %d", got)
	}

	// An invalid token → 401.
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if got := w.Result().StatusCode; got != http.StatusUnauthorized {
		t.Errorf("bad token: expected 401, got %d", got)
	}

	// A validly signed token → 200.
	token, err := signToken(cfg.JWTSecret, "admin", time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if got := w.Result().StatusCode; got != http.StatusOK {
		t.Errorf("valid token: expected 200, got %d", got)
	}

	// The login and index paths must stay reachable without a token.
	for _, p := range []string{"/login", "/"} {
		req = httptest.NewRequest(http.MethodGet, p, nil)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if got := w.Result().StatusCode; got != http.StatusOK {
			t.Errorf("public path %s: expected 200, got %d", p, got)
		}
	}
}

// TestHandleArtifactInfo tests the artifact info endpoint.
func TestHandleArtifactInfo(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL:      "http://localhost:8080",
		AuthEnabled:    false,
		AllowAnonymous: true,
	}

	dashboard := New(cfg)

	// Test method not allowed
	req := httptest.NewRequest(http.MethodPost, "/api/artifacts/info/test-job-id", nil)
	w := httptest.NewRecorder()

	dashboard.handleArtifactInfo(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", resp.StatusCode)
	}

	// Test missing job ID
	req = httptest.NewRequest(http.MethodGet, "/api/artifacts/info/", nil)
	w = httptest.NewRecorder()

	dashboard.handleArtifactInfo(w, req)

	resp = w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400 for missing job ID, got %d", resp.StatusCode)
	}
}

// TestHandleArtifactDownload tests the artifact download endpoint.
func TestHandleArtifactDownload(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL:      "http://localhost:8080",
		AuthEnabled:    false,
		AllowAnonymous: true,
	}

	dashboard := New(cfg)

	// Test method not allowed
	req := httptest.NewRequest(http.MethodPost, "/api/artifacts/download/test-job-id", nil)
	w := httptest.NewRecorder()

	dashboard.handleArtifactDownload(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", resp.StatusCode)
	}

	// Test missing job ID
	req = httptest.NewRequest(http.MethodGet, "/api/artifacts/download/", nil)
	w = httptest.NewRecorder()

	dashboard.handleArtifactDownload(w, req)

	resp = w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400 for missing job ID, got %d", resp.StatusCode)
	}
}

// TestHandleBuildsPage verifies the /builds page renders (was a 500 due to a
// missing "builds" template).
func TestHandleBuildsPage(t *testing.T) {
	cfg := &config.DashboardConfig{ServerURL: "http://localhost:8080", AuthEnabled: false, AllowAnonymous: true}
	dashboard := New(cfg)

	req := httptest.NewRequest(http.MethodGet, "/builds", nil)
	w := httptest.NewRecorder()
	dashboard.handleBuildsPage(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Result().StatusCode)
	}
	if !strings.Contains(w.Body.String(), "Builds") {
		t.Errorf("expected builds page content, got: %s", w.Body.String()[:min(200, w.Body.Len())])
	}
}

func TestImageFactoryPageAndStatusProxy(t *testing.T) {
	const snapshot = `{"configured":true,"catalog":{"version":2,"profiles":[],"images":[],"mirror_bundles":[]},"status":{"schema_version":1,"overall_state":"in_progress","milestones":[],"blockers":[],"desktop_e2e":{"state":"planned"}}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/image-factory/status" || r.Header.Get("X-API-Key") != "dashboard-backend-key" {
			http.Error(w, "unexpected image factory request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(snapshot))
	}))
	defer backend.Close()

	dashboard := New(&config.DashboardConfig{ServerURL: backend.URL, ServerAPIKey: "dashboard-backend-key", AllowAnonymous: true})
	page := httptest.NewRecorder()
	dashboard.handleImageFactoryPage(page, httptest.NewRequest(http.MethodGet, "/image-factory", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "factory-milestones") {
		t.Fatalf("image factory page was not rendered: status=%d body=%s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "factory-step-details") {
		t.Fatal("image factory page does not render structured stage-log details")
	}

	proxied := httptest.NewRecorder()
	dashboard.handleImageFactoryStatusProxy(proxied, httptest.NewRequest(http.MethodGet, "/api/image-factory/status", nil))
	if proxied.Code != http.StatusOK || proxied.Body.String() != snapshot {
		t.Fatalf("image factory status was not proxied exactly: status=%d body=%s", proxied.Code, proxied.Body.String())
	}

	wrongMethod := httptest.NewRecorder()
	dashboard.handleImageFactoryStatusProxy(wrongMethod, httptest.NewRequest(http.MethodPost, "/api/image-factory/status", nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("image factory status accepted mutation method: %d", wrongMethod.Code)
	}
}

func TestBuildPagesRenderStructuredLogDetails(t *testing.T) {
	dashboard := New(&config.DashboardConfig{ServerURL: "http://localhost:8080", AllowAnonymous: true})

	detail := httptest.NewRecorder()
	dashboard.handleBuildDetail(detail, httptest.NewRequest(http.MethodGet, "/build/job-1", nil))
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `id="stage-log-summary"`) ||
		!strings.Contains(detail.Body.String(), `id="live-log-meta"`) ||
		!strings.Contains(detail.Body.String(), `{ key: 'publish'`) {
		t.Fatalf("build detail does not include structured log detail surfaces: status=%d", detail.Code)
	}

	logs := httptest.NewRecorder()
	dashboard.handleBuildLogs(logs, httptest.NewRequest(http.MethodGet, "/logs/job-1", nil))
	if logs.Code != http.StatusOK || !strings.Contains(logs.Body.String(), `id="log-filters"`) ||
		!strings.Contains(logs.Body.String(), `id="log-meta"`) {
		t.Fatalf("build logs page does not include filters and metadata: status=%d", logs.Code)
	}
}

func TestSchedulerStatusDoesNotFabricateFallbackData(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serverURL := backend.URL
	backend.Close()

	dashboard := New(&config.DashboardConfig{
		ServerURL:      serverURL,
		AllowAnonymous: true,
	})
	w := httptest.NewRecorder()
	dashboard.handleSchedulerStatus(w, httptest.NewRequest(http.MethodGet, "/api/scheduler/status", nil))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "builder-1") || strings.Contains(body, `"queued_tasks":5`) {
		t.Fatalf("response contains fabricated scheduler data: %s", body)
	}
	if !strings.Contains(body, "backend server unreachable") {
		t.Fatalf("expected backend error details, got: %s", body)
	}
}

func TestLedgerStatusProxyPreservesDegradedState(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ledger/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"enabled":true,"ok":false,"write_errors":2,"last_reconcile":{"consistent":false}}`))
	}))
	defer backend.Close()

	dashboard := New(&config.DashboardConfig{ServerURL: backend.URL, AllowAnonymous: true})
	w := httptest.NewRecorder()
	dashboard.handleLedgerStatusAPI(w, httptest.NewRequest(http.MethodGet, "/api/ledger/status", nil))

	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"write_errors":2`) {
		t.Fatalf("ledger proxy status=%d body=%s", w.Code, w.Body.String())
	}

	wrongMethod := httptest.NewRecorder()
	dashboard.handleLedgerStatusAPI(wrongMethod, httptest.NewRequest(http.MethodPost, "/api/ledger/status", nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("ledger proxy accepted mutation: %d", wrongMethod.Code)
	}
}

func TestMonitorRendersJobLedgerSurface(t *testing.T) {
	dashboard := New(&config.DashboardConfig{ServerURL: "http://localhost:8080", AllowAnonymous: true})
	w := httptest.NewRecorder()
	dashboard.handleBuildersMonitor(w, httptest.NewRequest(http.MethodGet, "/monitor", nil))

	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `id="ledger"`) ||
		!strings.Contains(body, `/api/ledger/status`) ||
		!strings.Contains(body, `id="scheduler"`) ||
		!strings.Contains(body, `/api/scheduler/status`) ||
		!strings.Contains(body, `id="runtime-metadata"`) ||
		!strings.Contains(body, `/api/runtime-metadata/status`) ||
		!strings.Contains(body, `id="cache-status"`) ||
		!strings.Contains(body, `/api/cache/status`) ||
		!strings.Contains(body, `write_errors`) {
		t.Fatalf("monitor missing ledger surface: status=%d", w.Code)
	}
}

func TestBuildCancelAndRetryProxies(t *testing.T) {
	var requests []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	dashboard := New(&config.DashboardConfig{ServerURL: backend.URL, AllowAnonymous: true})

	cancel := httptest.NewRecorder()
	dashboard.handleBuildCancelProxy(cancel, httptest.NewRequest(
		http.MethodPost, "/api/builds/cancel?job_id=job%2F1&reason=operator+request", nil,
	))
	retry := httptest.NewRecorder()
	dashboard.handleBuildRetryProxy(retry, httptest.NewRequest(
		http.MethodPost, "/api/builds/retry?job_id=job%2F1", nil,
	))
	if cancel.Code != http.StatusOK || retry.Code != http.StatusOK {
		t.Fatalf("cancel=%d retry=%d", cancel.Code, retry.Code)
	}
	if len(requests) != 2 ||
		requests[0] != "POST /api/v1/builds/cancel?job_id=job%2F1&reason=operator+request" ||
		requests[1] != "POST /api/v1/builds/retry?job_id=job%2F1" {
		t.Fatalf("proxied requests=%v", requests)
	}
}
