package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/slchris/portage-engine/internal/builder"
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

func TestDeviceAuthorizationUsesExistingFederatedSession(t *testing.T) {
	const platformSession = "pe1_browser-session-fixture"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/iam/device/decision" ||
			r.Header.Get("Authorization") != "Bearer "+platformSession ||
			r.URL.RawQuery != "" {
			t.Errorf("backend request path=%q query=%q auth=%q",
				r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"))
			http.Error(w, "bad request", http.StatusUnauthorized)
			return
		}
		var decision map[string]string
		if err := json.NewDecoder(r.Body).Decode(&decision); err != nil ||
			decision["user_code"] != "ABCD-EFGH" ||
			decision["decision"] != "approve" {
			t.Errorf("decision=%v err=%v", decision, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"approved"}`))
	}))
	defer backend.Close()

	cfg := &config.DashboardConfig{
		ServerURL: backend.URL, AuthEnabled: true,
		JWTSecret: "test-secret-that-is-at-least-32-chars-long",
	}
	dashboard := New(cfg)
	sealed, err := dashboard.seal("oidc-session", oidcSession{
		AccessToken: platformSession, ProviderID: "fixture",
		Expires: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookie, Value: "federated." + sealed}

	pageRequest := httptest.NewRequest(http.MethodGet, "/device?user_code=ABCD-EFGH", nil)
	pageRequest.Header.Set("Accept", "text/html")
	pageRequest.AddCookie(cookie)
	pageResponse := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK ||
		!strings.Contains(pageResponse.Body.String(), "ABCD-EFGH") ||
		pageResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("device page status=%d body=%s", pageResponse.Code, pageResponse.Body.String())
	}

	decisionRequest := httptest.NewRequest(http.MethodPost, "/api/iam/device/decision",
		bytes.NewBufferString(`{"user_code":"ABCD-EFGH","decision":"approve"}`))
	decisionRequest.Header.Set("Content-Type", "application/json")
	decisionRequest.AddCookie(cookie)
	decisionResponse := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(decisionResponse, decisionRequest)
	if decisionResponse.Code != http.StatusOK ||
		!strings.Contains(decisionResponse.Body.String(), "approved") {
		t.Fatalf("decision status=%d body=%s",
			decisionResponse.Code, decisionResponse.Body.String())
	}
}

func TestDeviceAuthorizationLoginRedirectPreservesCode(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		AuthEnabled: true,
		JWTSecret:   "test-secret-that-is-at-least-32-chars-long",
	})
	request := httptest.NewRequest(http.MethodGet, "/device?user_code=ABCD-EFGH", nil)
	request.Header.Set("Accept", "text/html")
	response := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(response, request)
	if response.Code != http.StatusFound ||
		response.Header().Get("Location") !=
			"/login?return_to=%2Fdevice%3Fuser_code%3DABCD-EFGH" {
		t.Fatalf("redirect status=%d location=%q",
			response.Code, response.Header().Get("Location"))
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

func TestLocalLoginHonorsSecureCookieBehindProxy(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL:       "http://localhost:8080",
		AuthEnabled:     true,
		JWTSecret:       "test-secret-that-is-at-least-32-chars-long",
		AdminUser:       "testuser",
		AdminPassword:   "testpass",
		TokenTTLMinutes: 60,
		CookieSecure:    true,
	}
	body := bytes.NewBufferString(`{"username":"testuser","password":"testpass"}`)
	w := httptest.NewRecorder()
	New(cfg).handleLoginRoute(w, httptest.NewRequest(http.MethodPost, "/login", body))
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie ||
		!cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("secure proxy session cookie = %#v", cookies)
	}
}

func TestLocalStepUpIsBoundToDashboardSession(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL: "http://localhost:8080", AuthEnabled: true,
		JWTSecret: "test-secret-that-is-at-least-32-chars-long",
		AdminUser: "admin", AdminPassword: "password",
		ServerAPIKey: "primary-key", ServerStepUpAPIKey: "second-key",
	}
	dashboard := New(cfg)
	session, err := signToken(cfg.JWTSecret, "admin", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost, "/auth/step-up",
		bytes.NewBufferString(`{"username":"admin","password":"password"}`),
	)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	recorder := httptest.NewRecorder()
	dashboard.handleLocalStepUp(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("step-up status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var stepCookie *http.Cookie
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == stepUpCookie {
			stepCookie = cookie
		}
	}
	if stepCookie == nil || !stepCookie.HttpOnly ||
		stepCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("step-up cookie=%#v", stepCookie)
	}
	incoming := httptest.NewRequest(http.MethodPut, "/api/settings/cloud", nil)
	incoming.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	incoming.AddCookie(stepCookie)
	outgoing := httptest.NewRequest(http.MethodPut, "http://localhost:8080/api/v1/settings/cloud", nil)
	dashboard.applyBackendAuth(incoming, outgoing)
	if outgoing.Header.Get("X-API-Key") != "primary-key" ||
		outgoing.Header.Get("X-Step-Up-Key") != "second-key" {
		t.Fatalf("backend auth headers=%v", outgoing.Header)
	}
	otherSession, err := signToken(cfg.JWTSecret, "admin", time.Now().Add(time.Second), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	incoming = httptest.NewRequest(http.MethodPut, "/api/settings/cloud", nil)
	incoming.AddCookie(&http.Cookie{Name: sessionCookie, Value: otherSession})
	incoming.AddCookie(stepCookie)
	outgoing = httptest.NewRequest(http.MethodPut, "http://localhost:8080/api/v1/settings/cloud", nil)
	dashboard.applyBackendAuth(incoming, outgoing)
	if outgoing.Header.Get("X-Step-Up-Key") != "" {
		t.Fatal("step-up cookie was accepted with a different dashboard session")
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

func TestLoginPageRendersConfiguredIdentityMethods(t *testing.T) {
	base := &config.DashboardConfig{
		ServerURL:   "http://localhost:8080",
		AuthEnabled: true,
		JWTSecret:   "test-secret-that-is-at-least-32-chars-long",
	}

	oidcOnly := *base
	oidcOnly.OIDCEnabled = true
	dashboard := New(&oidcOnly)
	w := httptest.NewRecorder()
	dashboard.handleLoginRoute(w, httptest.NewRequest(http.MethodGet, "/login", nil))
	if body := w.Body.String(); !strings.Contains(body, "/auth/oidc/start") ||
		strings.Contains(body, `id="login-form"`) {
		t.Fatalf("OIDC-only login rendered the wrong methods: %s", body)
	}

	hybrid := *base
	hybrid.OIDCEnabled = true
	hybrid.AdminUser = "admin"
	hybrid.AdminPassword = "break-glass"
	dashboard = New(&hybrid)
	w = httptest.NewRecorder()
	dashboard.handleLoginRoute(w, httptest.NewRequest(http.MethodGet, "/login", nil))
	if body := w.Body.String(); !strings.Contains(body, "/auth/oidc/start") ||
		!strings.Contains(body, `id="login-form"`) {
		t.Fatalf("hybrid login did not render both methods: %s", body)
	}
}

func TestLoginPageRendersEachConfiguredProvider(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL:   "http://localhost:8080",
		AuthEnabled: true,
		JWTSecret:   "test-secret-that-is-at-least-32-chars-long",
	}
	cfg.IdentityProviders = []config.IdentityProviderConfig{
		{ID: "authentik", DisplayName: "Authentik"},
		{ID: "google", DisplayName: "Google"},
		{ID: "github", DisplayName: "GitHub"},
	}
	dashboard := New(cfg)
	dashboard.providers = map[string]*oidcRuntime{
		"authentik": {}, "google": {}, "github": {},
	}
	w := httptest.NewRecorder()
	dashboard.handleLoginRoute(w, httptest.NewRequest(http.MethodGet, "/login", nil))
	body := w.Body.String()
	for _, expected := range []string{
		"/auth/provider/authentik/start",
		"/auth/provider/google/start",
		"/auth/provider/github/start",
		"Sign in with Authentik", "Sign in with Google", "Sign in with GitHub",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("multi-provider login does not contain %q", expected)
		}
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

	// Public community pages and their narrow read APIs stay reachable without
	// a console session.
	for _, p := range []string{
		"/login", "/", "/packages", "/docs", "/status",
		"/api/public/binhosts", "/api/public/packages", "/api/public/status",
		"/binpkgs/releases/amd64/binpackages/23.0/target/Packages",
	} {
		req = httptest.NewRequest(http.MethodGet, p, nil)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if got := w.Result().StatusCode; got != http.StatusOK {
			t.Errorf("public path %s: expected 200, got %d", p, got)
		}
	}
}

func TestPublicCommunityPagesUseAnonymousShell(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		ServerURL: "http://127.0.0.1:1", AuthEnabled: true,
		JWTSecret: strings.Repeat("x", 32),
	})
	router := dashboard.Router()
	for _, target := range []string{"/packages", "/docs", "/status"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", target, response.Code, response.Body.String())
		}
		body := response.Body.String()
		if !strings.Contains(body, `class="public-main"`) {
			t.Errorf("%s did not render the public page shell", target)
		}
		if strings.Contains(body, "/api/iam/me") ||
			strings.Contains(body, "project-switcher") {
			t.Errorf("%s included authenticated console bootstrap", target)
		}
	}
}

func TestPublicProxyForwardsOnlySafeQueryAndNoCredentials(t *testing.T) {
	var captured *http.Request
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=30")
		_, _ = w.Write([]byte(`{"packages":[],"total":0,"limit":10,"offset":0}`))
	}))
	defer backend.Close()

	dashboard := New(&config.DashboardConfig{
		ServerURL: backend.URL, ServerAPIKey: "dashboard-server-secret",
		AuthEnabled: true, JWTSecret: strings.Repeat("x", 32),
	})
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/public/packages?q=app-misc%2Fjq&limit=10&ignored=secret",
		nil,
	)
	request.Header.Set("Authorization", "Bearer browser-secret")
	request.Header.Set("X-API-Key", "browser-api-key")
	request.Header.Set("X-Project-ID", "private-project")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "private-session"})
	response := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if captured == nil {
		t.Fatal("backend did not receive public request")
	}
	if captured.URL.Path != "/api/v1/public/packages" ||
		captured.URL.Query().Get("q") != "app-misc/jq" ||
		captured.URL.Query().Get("limit") != "10" ||
		captured.URL.Query().Has("ignored") {
		t.Fatalf("unexpected backend request: %s", captured.URL.String())
	}
	for _, name := range []string{
		"Authorization", "X-API-Key", "X-Project-ID", "Cookie",
	} {
		if value := captured.Header.Get(name); value != "" {
			t.Errorf("public proxy forwarded %s=%q", name, value)
		}
	}
	if response.Header().Get("Cache-Control") != "public, max-age=30" {
		t.Errorf("cache policy was not relayed: %q", response.Header().Get("Cache-Control"))
	}
}

func TestPublicBinpkgProxyAllowsAnonymousHEAD(t *testing.T) {
	var method string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.Header().Set("Content-Length", "123")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	dashboard := New(&config.DashboardConfig{
		ServerURL: backend.URL, ServerAPIKey: "must-not-be-forwarded",
		AuthEnabled: true, JWTSecret: strings.Repeat("x", 32),
	})
	response := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodHead,
			"/binpkgs/releases/amd64/binpackages/23.0/target/Packages",
			nil,
		),
	)
	if response.Code != http.StatusOK || method != http.MethodHead {
		t.Fatalf("status=%d backend method=%q", response.Code, method)
	}
	if response.Header().Get("Content-Length") != "123" {
		t.Fatalf("content length was not relayed: %q", response.Header().Get("Content-Length"))
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

func TestDashboardProxyForwardsProjectContext(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Project-ID") != "project-alpha" {
			http.Error(w, "missing project context", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	dashboard := New(&config.DashboardConfig{
		ServerURL:      backend.URL,
		ServerAPIKey:   "break-glass",
		AllowAnonymous: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/instances", nil)
	req.Header.Set("X-Project-ID", "project-alpha")
	w := httptest.NewRecorder()
	dashboard.handleInstances(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("project-aware proxy status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDashboardProjectPolicyProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/policy" ||
			r.Header.Get("X-Project-ID") != "project-alpha" {
			http.Error(w, "wrong project policy request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project_id":"project-alpha","max_active_jobs":2}`))
	}))
	defer backend.Close()

	dashboard := New(&config.DashboardConfig{
		ServerURL: backend.URL, ServerAPIKey: "break-glass", AllowAnonymous: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/projects/policy", nil)
	req.Header.Set("X-Project-ID", "project-alpha")
	w := httptest.NewRecorder()
	dashboard.handleProjectPolicyProxy(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"max_active_jobs":2`) {
		t.Fatalf("policy proxy status=%d body=%s", w.Code, w.Body.String())
	}

	wrongMethod := httptest.NewRecorder()
	dashboard.handleProjectPolicyProxy(
		wrongMethod, httptest.NewRequest(http.MethodDelete, "/api/projects/policy", nil),
	)
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("policy proxy accepted delete: %d", wrongMethod.Code)
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

func TestWorkloadIdentityInventoryProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/worker-gateway/identities" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuers":[],"certificates":[],"certificate_limit":100}`))
	}))
	defer backend.Close()

	dashboard := New(&config.DashboardConfig{
		ServerURL: backend.URL, AllowAnonymous: true,
	})
	w := httptest.NewRecorder()
	dashboard.handleWorkloadIdentityInventory(
		w, httptest.NewRequest(
			http.MethodGet, "/api/worker-gateway/identities", nil,
		),
	)
	if w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"certificate_limit":100`) {
		t.Fatalf("workload identity proxy status=%d body=%s", w.Code, w.Body.String())
	}
	wrongMethod := httptest.NewRecorder()
	dashboard.handleWorkloadIdentityInventory(
		wrongMethod, httptest.NewRequest(
			http.MethodPost, "/api/worker-gateway/identities", nil,
		),
	)
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("workload identity proxy accepted mutation: %d", wrongMethod.Code)
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
		!strings.Contains(body, `write_errors`) ||
		!strings.Contains(body, `lease_expiries`) ||
		!strings.Contains(body, `projection.lag_seconds`) {
		t.Fatalf("monitor missing ledger surface: status=%d", w.Code)
	}
}

// The scheduler card's degraded verdict rests on projection validity alone. The
// bound published beside the lag reading is the read-through cache TTL that
// produced it: the server reloads the moment a snapshot reaches that age, so
// every reading reachable by a browser is strictly below it and a
// lag-over-bound term can only ever be false. The Prometheus rule and the
// Grafana threshold that drew the same comparison are already gone; this copy
// fed a badge an operator reads as "the scheduler is degraded".
// Red by: putting a lag_seconds > bound term back into projectionBad.
func TestMonitorProjectionBadgeRestsOnValidityAlone(t *testing.T) {
	dashboard := New(&config.DashboardConfig{ServerURL: "http://localhost:8080", AllowAnonymous: true})
	w := httptest.NewRecorder()
	dashboard.handleBuildersMonitor(w, httptest.NewRequest(http.MethodGet, "/monitor", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("monitor status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `var projectionBad = projection.valid === false;`) {
		t.Error("the scheduler degraded verdict is no longer projection.valid alone")
	}
}

// monitorJS reads the scheduler status straight out of the API's JSON, so every
// projection.<field> it names has to be one the API publishes. A field the
// server has stopped emitting under that name reads as undefined in a browser
// and silently deletes the line it feeds — no console error, no blank value,
// just a row an operator stops being shown. Deriving the published set from the
// struct that serialises it, rather than restating the names here, is what makes
// a rename on the server side red on this side.
// Red by: renaming any json tag on builder.MonitorProjectionStatus without
// following it into monitorJS.
func TestMonitorPageReadsOnlyPublishedProjectionFields(t *testing.T) {
	published := map[string]bool{}
	status := reflect.TypeOf(builder.MonitorProjectionStatus{})
	for index := 0; index < status.NumField(); index++ {
		name, _, _ := strings.Cut(status.Field(index).Tag.Get("json"), ",")
		published[name] = true
	}
	if len(published) < status.NumField() {
		t.Fatalf("builder.MonitorProjectionStatus has %d fields but %d distinct "+
			"json names; the set below would be checked against a lie",
			status.NumField(), len(published))
	}
	// The leading guard keeps the mon.targets.projection.* message keys out:
	// they end in the same dotted shape as a property read, and counting them
	// as field reads would bury the real ones under seven permanent failures.
	reads := regexp.MustCompile(`(^|[^\w.])projection\.([a-z0-9_]+)`).
		FindAllStringSubmatch(monitorJS, -1)
	if len(reads) == 0 {
		t.Fatal("monitorJS reads no projection field at all; the pattern no longer matches the code")
	}
	for _, read := range reads {
		if !published[read[2]] {
			t.Errorf("monitorJS reads projection.%s, which "+
				"builder.MonitorProjectionStatus does not publish", read[2])
		}
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

func TestSessionInAuthorizationHeaderNeverBorrowsServerAPIKey(t *testing.T) {
	const platformSession = "pe1_low_privilege_user_session"
	var captured http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"subject":"viewer"}`))
	}))
	defer backend.Close()

	cfg := &config.DashboardConfig{
		ServerURL: backend.URL, AuthEnabled: true,
		JWTSecret:    "test-secret-that-is-at-least-32-chars-long",
		ServerAPIKey: "ADMIN-SERVER-KEY",
	}
	dashboard := New(cfg)
	sealed, err := dashboard.seal("oidc-session", oidcSession{
		AccessToken: platformSession, ProviderID: "fixture",
		Expires: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	session := "federated." + sealed

	// authMiddleware accepts the session from either place, so both must reach
	// the backend as the same principal.
	for _, presentation := range []struct {
		name   string
		attach func(*http.Request)
	}{
		{"cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		}},
		{"authorization header", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+session)
		}},
	} {
		captured = nil
		request := httptest.NewRequest(http.MethodGet, "/api/iam/me", nil)
		presentation.attach(request)
		response := httptest.NewRecorder()
		dashboard.Router().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s",
				presentation.name, response.Code, response.Body.String())
		}
		if got := captured.Get("Authorization"); got != "Bearer "+platformSession {
			t.Errorf("%s: upstream Authorization=%q, want the caller's own bearer",
				presentation.name, got)
		}
		if got := captured.Get("X-API-Key"); got != "" {
			t.Errorf("%s: upstream X-API-Key=%q promoted a signed-in viewer to system admin",
				presentation.name, got)
		}
	}

	// A federated session the dashboard cannot resolve is refused upstream, not
	// widened to the legacy administrator credential.
	stale, err := dashboard.seal("oidc-session", oidcSession{
		AccessToken: platformSession, ProviderID: "fixture",
		Expires: time.Now().Add(-time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	incoming := httptest.NewRequest(http.MethodGet, "/api/iam/me", nil)
	incoming.Header.Set("Authorization", "Bearer federated."+stale)
	outgoing := httptest.NewRequest(http.MethodGet, backend.URL+"/api/v1/iam/me", nil)
	dashboard.applyBackendAuth(incoming, outgoing)
	if outgoing.Header.Get("X-API-Key") != "" ||
		outgoing.Header.Get("Authorization") != "" {
		t.Fatalf("expired federated session headers=%v", outgoing.Header)
	}
}

func TestShellProxyForwardsCallerSessionNotServerAPIKey(t *testing.T) {
	const platformSession = "pe1_low_privilege_user_session"
	var captured http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		http.Error(w, "not a shell", http.StatusForbidden)
	}))
	defer backend.Close()

	cfg := &config.DashboardConfig{
		ServerURL: backend.URL, AuthEnabled: true,
		JWTSecret:    "test-secret-that-is-at-least-32-chars-long",
		ServerAPIKey: "ADMIN-SERVER-KEY",
	}
	dashboard := New(cfg)
	sealed, err := dashboard.seal("oidc-session", oidcSession{
		AccessToken: platformSession, ProviderID: "fixture",
		Expires: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/shell?id=builder-1", nil)
	request.Header.Set("Authorization", "Bearer federated."+sealed)
	dashboard.Router().ServeHTTP(httptest.NewRecorder(), request)

	if captured == nil {
		t.Fatal("shell proxy did not reach the backend")
	}
	if got := captured.Get("Authorization"); got != "Bearer "+platformSession {
		t.Errorf("shell handshake Authorization=%q, want the caller's own bearer", got)
	}
	if got := captured.Get("X-API-Key"); got != "" {
		t.Errorf("shell handshake X-API-Key=%q opened a root shell as system admin", got)
	}
}

// TestShellProxyCarriesStepUpToAnEnforcingBackend puts a backend behind the
// bridge that refuses the shell the way the control plane does — 428 unless the
// handshake carries the independent step-up credential. A stub that accepts any
// handshake cannot tell a bridge that forwards the caller's credentials from
// one that hand-assembles its own headers and leaves step-up out, which is the
// difference between a working web shell and a route no caller can satisfy.
func TestShellProxyCarriesStepUpToAnEnforcingBackend(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Step-Up-Key") != "second-key" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPreconditionRequired)
			_, _ = io.WriteString(w,
				`{"error":"fresh step-up authentication required","code":"step_up_required"}`)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer backend.Close()

	cfg := &config.DashboardConfig{
		ServerURL: backend.URL, AuthEnabled: true,
		JWTSecret: "test-secret-that-is-at-least-32-chars-long",
		AdminUser: "admin", AdminPassword: "password",
		ServerAPIKey: "primary-key", ServerStepUpAPIKey: "second-key",
	}
	dashboard := New(cfg)
	session, err := signToken(cfg.JWTSecret, "admin", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sessionValue := &http.Cookie{Name: sessionCookie, Value: session}

	front := httptest.NewServer(dashboard.Router())
	defer front.Close()

	// Returns the handshake status rather than the response: a rejected upgrade
	// carries a body that has to be drained here, and the status is the only
	// part of it the assertions below care about. 0 means no HTTP reply at all.
	dial := func(cookies ...*http.Cookie) (int, error) {
		jar := make([]string, 0, len(cookies))
		for _, cookie := range cookies {
			jar = append(jar, cookie.Name+"="+cookie.Value)
		}
		header := http.Header{}
		header.Set("Cookie", strings.Join(jar, "; "))
		header.Set("Origin", front.URL)
		conn, response, dialErr := websocket.DefaultDialer.Dial(
			"ws"+strings.TrimPrefix(front.URL, "http")+"/api/shell?id=builder-1",
			header,
		)
		if conn != nil {
			_ = conn.Close()
		}
		status := 0
		if response != nil {
			status = response.StatusCode
			_ = response.Body.Close()
		}
		return status, dialErr
	}
	preflight := func(cookies ...*http.Cookie) (int, map[string]any) {
		request := httptest.NewRequest(http.MethodGet, "/api/shell/preflight", nil)
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		recorder := httptest.NewRecorder()
		dashboard.Router().ServeHTTP(recorder, request)
		decoded := map[string]any{}
		_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
		return recorder.Code, decoded
	}

	// Signed in, but not elevated: the shell must be refused, and the page has
	// to be able to learn that over plain HTTP — a rejected WebSocket handshake
	// reaches page script with no status at all.
	if status, dialErr := dial(sessionValue); dialErr == nil ||
		status != http.StatusPreconditionRequired {
		t.Fatalf("unelevated shell handshake err=%v status=%d", dialErr, status)
	}
	if code, body := preflight(sessionValue); code != http.StatusPreconditionRequired ||
		body["method"] != "local" {
		t.Fatalf("unelevated preflight status=%d body=%v", code, body)
	}

	elevation := httptest.NewRequest(http.MethodPost, "/auth/step-up",
		bytes.NewBufferString(`{"username":"admin","password":"password"}`))
	elevation.AddCookie(sessionValue)
	recorder := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(recorder, elevation)
	if recorder.Code != http.StatusOK {
		t.Fatalf("step-up status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var elevated *http.Cookie
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == stepUpCookie {
			elevated = cookie
		}
	}
	if elevated == nil {
		t.Fatal("step-up issued no elevation cookie")
	}

	if code, body := preflight(sessionValue, elevated); code != http.StatusOK {
		t.Fatalf("elevated preflight status=%d body=%v", code, body)
	}
	if status, dialErr := dial(sessionValue, elevated); dialErr != nil {
		t.Fatalf("elevated shell handshake failed (status=%d): %v; "+
			"the bridge did not forward the step-up credential", status, dialErr)
	}
}

// TestShellPreflightReportsAnUnsatisfiableRoute: with no independent step-up
// key the dashboard can never elevate a local session, so the web shell cannot
// be opened at all. Saying so is the difference between a deployment note and a
// terminal that closes without ever printing a reason.
func TestShellPreflightReportsAnUnsatisfiableRoute(t *testing.T) {
	cfg := &config.DashboardConfig{
		ServerURL: "http://localhost:8080", AuthEnabled: true,
		JWTSecret: "test-secret-that-is-at-least-32-chars-long",
		AdminUser: "admin", AdminPassword: "password",
		ServerAPIKey: "primary-key",
	}
	dashboard := New(cfg)
	session, err := signToken(cfg.JWTSecret, "admin", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/shell/preflight", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	recorder := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(recorder, request)

	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if recorder.Code != http.StatusPreconditionRequired ||
		body["code"] != "step_up_unavailable" {
		t.Fatalf("preflight status=%d body=%v, want an explicit unavailable answer",
			recorder.Code, body)
	}
}

func TestBinpkgProxyStreamsPastTheControlPlaneDeadline(t *testing.T) {
	const payload = "GENTOO-BINARY-PACKAGE-BODY"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload[:4])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Stand in for the server hashing a multi-hundred-megabyte package
		// while the body is already open.
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, payload[4:])
	}))
	defer backend.Close()

	dashboard := New(&config.DashboardConfig{
		ServerURL: backend.URL, AuthEnabled: true,
		JWTSecret: strings.Repeat("x", 32),
	})
	// Shrink the control-plane deadline rather than waiting out the real 10s:
	// a package download must not be governed by it at all.
	dashboard.httpClient.Timeout = 50 * time.Millisecond

	response := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		"/binpkgs/releases/amd64/binpackages/23.0/target/app-misc-jq.gpkg.tar",
		nil,
	))
	if response.Code != http.StatusOK || response.Body.String() != payload {
		t.Fatalf("status=%d body=%q want %q",
			response.Code, response.Body.String(), payload)
	}
}

func TestJobEventStreamOutlivesTheControlPlaneDeadline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer backend.Close()

	dashboard := New(&config.DashboardConfig{
		ServerURL: backend.URL, AuthEnabled: false, AllowAnonymous: true,
		JWTSecret: strings.Repeat("x", 32),
	})
	dashboard.httpClient.Timeout = 50 * time.Millisecond

	response := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/api/events/jobs", nil))
	if !strings.Contains(response.Body.String(), "data: second") {
		t.Fatalf("event stream was cut short: %q", response.Body.String())
	}
}

func TestVersionedStylesheetIsCacheableAndRevalidates(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		ServerURL: "http://127.0.0.1:1", AuthEnabled: true,
		JWTSecret: strings.Repeat("x", 32),
	})
	router := dashboard.Router()

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(
		http.MethodGet, "/static/apple.css?v=2", nil))
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || etag == "" ||
		!strings.Contains(first.Header().Get("Cache-Control"), "max-age=31536000") {
		t.Fatalf("status=%d etag=%q cache-control=%q",
			first.Code, etag, first.Header().Get("Cache-Control"))
	}

	revalidation := httptest.NewRequest(http.MethodGet, "/static/apple.css?v=2", nil)
	revalidation.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	router.ServeHTTP(second, revalidation)
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 {
		t.Fatalf("revalidation status=%d bytes=%d", second.Code, second.Body.Len())
	}
}

func TestPageLanguageIsResolvedBeforeTheFirstByte(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		ServerURL: "http://127.0.0.1:1", AuthEnabled: true,
		JWTSecret: strings.Repeat("x", 32),
	})
	router := dashboard.Router()

	for _, testCase := range []struct {
		name   string
		attach func(*http.Request)
		want   string
	}{
		{"english by default", func(*http.Request) {}, "en"},
		{"browser preference", func(r *http.Request) {
			r.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		}, "zh"},
		{"cookie overrides the browser", func(r *http.Request) {
			r.Header.Set("Accept-Language", "zh-CN")
			r.AddCookie(&http.Cookie{Name: languageCookie, Value: "en"})
		}, "en"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/docs", nil)
		testCase.attach(request)
		if got := resolveLanguage(request); got != testCase.want {
			t.Errorf("%s: resolveLanguage=%q want %q", testCase.name, got, testCase.want)
		}
		data := dashboard.pageData(request, nil)
		if data["Lang"] != testCase.want {
			t.Errorf("%s: pageData Lang=%v want %q", testCase.name, data["Lang"], testCase.want)
		}
		wantTag := "en"
		if testCase.want == "zh" {
			wantTag = "zh-CN"
		}
		if data["HTMLLang"] != wantTag {
			t.Errorf("%s: pageData HTMLLang=%v want %q",
				testCase.name, data["HTMLLang"], wantTag)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if !strings.Contains(response.Header().Get("Vary"), "Cookie") {
			t.Errorf("%s: Vary=%q does not cover the language cookie",
				testCase.name, response.Header().Get("Vary"))
		}
	}
}

func TestLanguagePreferenceEndpointPersistsTheChoice(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		ServerURL: "http://127.0.0.1:1", AuthEnabled: true,
		JWTSecret: strings.Repeat("x", 32),
	})

	// Public: the landing, packages, docs and status pages carry the toggle and
	// are reachable without a session.
	accepted := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(accepted, httptest.NewRequest(
		http.MethodPost, "/api/preferences/language",
		bytes.NewBufferString(`{"lang":"zh"}`)))
	if accepted.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	var cookie *http.Cookie
	for _, candidate := range accepted.Result().Cookies() {
		if candidate.Name == languageCookie {
			cookie = candidate
		}
	}
	if cookie == nil || cookie.Value != "zh" || cookie.Path != "/" ||
		cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("language cookie=%#v", cookie)
	}

	rejected := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(rejected, httptest.NewRequest(
		http.MethodPost, "/api/preferences/language",
		bytes.NewBufferString(`{"lang":"klingon"}`)))
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("unsupported language status=%d", rejected.Code)
	}
}
