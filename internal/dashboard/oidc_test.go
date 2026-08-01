package dashboard

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/slchris/portage-engine/pkg/config"
)

func TestProviderCredentialExchangeReturnsOnlyPlatformSession(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/iam/exchange" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var request map[string]string
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request["provider_id"] != "github" ||
			request["credential"] != "upstream-secret" {
			http.Error(w, "bad exchange", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "pe1_platform-session",
			"token_type":   "Bearer", "expires_in": 600,
		})
	}))
	defer backend.Close()
	dashboard := New(&config.DashboardConfig{
		ServerURL: backend.URL, JWTSecret: "test-secret-that-is-at-least-32-chars",
	})
	token, expires, err := dashboard.exchangeProviderCredential(
		t.Context(), "github", "upstream-secret",
	)
	if err != nil || token != "pe1_platform-session" ||
		time.Until(expires) < 9*time.Minute {
		t.Fatalf("provider exchange token=%q expires=%s err=%v", token, expires, err)
	}
}

func TestOIDCCookieSealRoundTripAndTamperDetection(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		ServerURL: "http://localhost:8080",
		JWTSecret: "test-secret-that-is-at-least-32-chars-long",
	})
	input := oidcFlow{
		State: "state", Verifier: "verifier", Nonce: "nonce",
		Expires: time.Now().Add(time.Minute).Unix(),
	}
	sealed, err := dashboard.seal("oidc-flow", input)
	if err != nil {
		t.Fatalf("seal OIDC flow: %v", err)
	}
	var output oidcFlow
	if err := dashboard.unseal("oidc-flow", sealed, &output); err != nil {
		t.Fatalf("unseal OIDC flow: %v", err)
	}
	if output != input {
		t.Fatalf("OIDC flow round trip = %#v, want %#v", output, input)
	}

	tamperedBytes, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatal(err)
	}
	tamperedBytes[0] ^= 0x80
	tampered := base64.RawURLEncoding.EncodeToString(tamperedBytes)
	if err := dashboard.unseal("oidc-flow", tampered, &output); err == nil {
		t.Fatal("tampered OIDC flow cookie was accepted")
	}
	if err := dashboard.unseal("oidc-session", sealed, &output); err == nil {
		t.Fatal("OIDC flow cookie was accepted for the session purpose")
	}
}

func TestOIDCStartUnavailableBeforeProviderInitialization(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		ServerURL:   "http://localhost:8080",
		OIDCEnabled: true,
		JWTSecret:   strings.Repeat("s", 32),
	})
	if dashboard.oidc != nil {
		t.Fatal("OIDC runtime unexpectedly initialized")
	}
	w := httptest.NewRecorder()
	dashboard.handleOIDCStart(w, httptest.NewRequest(http.MethodGet, "/auth/oidc/start", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("OIDC start before discovery status=%d, want 404", w.Code)
	}
}

func TestOIDCStepUpStartForcesProviderReauthentication(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		ServerURL: "http://localhost:8080",
		JWTSecret: strings.Repeat("s", 32),
	})
	dashboard.oidc = &oidcRuntime{oauth2: oauth2.Config{
		ClientID:    "portage-engine",
		RedirectURL: "https://dashboard.example.test/auth/oidc/callback",
		Endpoint: oauth2.Endpoint{
			AuthURL: "https://identity.example.test/authorize",
		},
	}}
	w := httptest.NewRecorder()
	dashboard.handleOIDCStart(
		w,
		httptest.NewRequest(http.MethodGet, "/auth/oidc/start?step_up=1", nil),
	)
	if w.Code != http.StatusFound {
		t.Fatalf("OIDC step-up start status=%d, want 302", w.Code)
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse OIDC redirect: %v", err)
	}
	query := location.Query()
	if query.Get("prompt") != "login" || query.Get("max_age") != "0" ||
		query.Get("code_challenge_method") != "S256" {
		t.Fatalf("OIDC step-up redirect query = %v", query)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != oidcFlowCookie ||
		cookies[0].Path != oidcFlowCookiePath || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("OIDC flow cookie = %#v", cookies)
	}
	var flow oidcFlow
	if err := dashboard.unseal("oidc-flow", cookies[0].Value, &flow); err != nil {
		t.Fatalf("unseal OIDC step-up flow: %v", err)
	}
	if !flow.StepUp {
		t.Fatal("OIDC flow did not retain the step-up intent")
	}
}

func TestProviderFlowCookieCoversMultiProviderCallback(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		ServerURL: "http://localhost:8080",
		JWTSecret: strings.Repeat("s", 32),
	})
	dashboard.providers = map[string]*oidcRuntime{
		"google": {
			id: "google", providerType: "oidc",
			oauth2: oauth2.Config{
				ClientID:    "google-client",
				RedirectURL: "https://dashboard.example.test/auth/provider/google/callback",
				Endpoint: oauth2.Endpoint{
					AuthURL: "https://accounts.google.com/o/oauth2/v2/auth",
				},
			},
		},
	}

	w := httptest.NewRecorder()
	dashboard.handleIdentityProviderRoute(
		w,
		httptest.NewRequest(
			http.MethodGet, "/auth/provider/google/start", nil,
		),
	)
	if w.Code != http.StatusFound {
		t.Fatalf("provider start status=%d, want 302", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Path != "/auth/" {
		t.Fatalf("provider flow cookie does not cover callback: %#v", cookies)
	}
	callback, err := url.Parse(
		"https://dashboard.example.test/auth/provider/google/callback",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(callback.Path, cookies[0].Path) {
		t.Fatalf("cookie path %q does not cover callback %q", cookies[0].Path, callback.Path)
	}
}
