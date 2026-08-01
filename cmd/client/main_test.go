package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/builder"
)

func TestExchangeProviderCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/iam/exchange" ||
			r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var request map[string]string
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request["provider_id"] != "google" ||
			request["credential"] != "upstream-id-token" {
			http.Error(w, "bad exchange", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "pe1_cli-session",
			"token_type":   "Bearer", "expires_in": 600,
		})
	}))
	defer server.Close()
	result, err := exchangeProviderCredential(
		server.Client(), server.URL, "google", "upstream-id-token",
	)
	if err != nil || result.AccessToken != "pe1_cli-session" ||
		result.ExpiresIn != 600 {
		t.Fatalf("token exchange = %+v, %v", result, err)
	}
}

func TestDeviceAuthorizationPollingHonorsServerInterval(t *testing.T) {
	const rawDeviceCode = "ped1_high-entropy-device-capability"
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" {
			t.Errorf("credential leaked into header/query: %s %q auth=%q",
				r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/v1/iam/device/authorization":
			if r.Method != http.MethodPost {
				t.Errorf("authorization method=%s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": rawDeviceCode, "user_code": "ABCD-EFGH",
				"verification_uri":          "https://dashboard.example.test/device",
				"verification_uri_complete": "https://dashboard.example.test/device?user_code=ABCD-EFGH",
				"expires_in":                600, "interval": 5,
			})
		case "/api/v1/iam/device/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
			}
			if r.PostForm.Get("device_code") != rawDeviceCode ||
				r.PostForm.Get("grant_type") != deviceGrantType {
				t.Errorf("token form=%v", r.PostForm)
			}
			polls++
			switch polls {
			case 1:
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "authorization_pending", "interval": 5,
				})
			case 2:
				w.Header().Set("Retry-After", "10")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "pe1_device-session", "token_type": "Bearer",
					"expires_in": 900,
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	authorization, err := startDeviceAuthorization(server.Client(), server.URL)
	if err != nil || authorization.UserCode != "ABCD-EFGH" {
		t.Fatalf("start = %+v, %v", authorization, err)
	}
	var waits []time.Duration
	result, err := pollDeviceAuthorization(
		context.Background(), server.Client(), server.URL,
		authorization.DeviceCode, authorization.Interval,
		func(_ context.Context, duration time.Duration) error {
			waits = append(waits, duration)
			return nil
		},
	)
	if err != nil || result.AccessToken != "pe1_device-session" {
		t.Fatalf("poll = %+v, %v", result, err)
	}
	wantWaits := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}
	if len(waits) != len(wantWaits) {
		t.Fatalf("waits=%v", waits)
	}
	for index := range wantWaits {
		if waits[index] != wantWaits[index] {
			t.Fatalf("waits=%v", waits)
		}
	}
}

// The public edge answers a tripped rate limit with a 429 whose body is nginx
// HTML, and a 5xx or a dropped connection says nothing about the device code
// either. Only RFC 8628's terminal error codes may end the flow; anything else
// has to keep polling until expires_in runs out, or one unlucky poll discards a
// device code the user is still in the middle of approving.
func TestDeviceAuthorizationPollingSurvivesNonOAuthFailures(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		polls++
		switch polls {
		case 1:
			w.Header().Set("Retry-After", "7")
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("<html><head><title>429 Too Many Requests</title></head></html>"))
		case 2:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502</html>"))
		case 3:
			// An OAuth error object outside the terminal set is still not a
			// reason to stop polling.
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "server_error"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "pe1_device-session", "token_type": "Bearer",
				"expires_in": 900,
			})
		}
	}))
	defer server.Close()

	var waits []time.Duration
	result, err := pollDeviceAuthorization(
		context.Background(), server.Client(), server.URL, "ped1_secret", 5,
		func(_ context.Context, duration time.Duration) error {
			waits = append(waits, duration)
			return nil
		},
	)
	if err != nil || result.AccessToken != "pe1_device-session" {
		t.Fatalf("poll = %+v, %v", result, err)
	}
	// Retry-After moves the cadence to 7s and it stays there: neither the 502
	// nor the non-terminal OAuth code advertises an interval of its own.
	wantWaits := []time.Duration{
		5 * time.Second, 7 * time.Second, 7 * time.Second, 7 * time.Second,
	}
	if len(waits) != len(wantWaits) {
		t.Fatalf("waits=%v, want %v", waits, wantWaits)
	}
	for index := range wantWaits {
		if waits[index] != wantWaits[index] {
			t.Fatalf("waits=%v, want %v", waits, wantWaits)
		}
	}
}

// Retry-After on a body that is not an OAuth error object comes from whatever
// answered — an edge, a CDN, a proxy — not from the authorization server, so it
// may only slow the cadence down. An edge answering its own tripped rate limit
// with Retry-After: 1 would otherwise drive the CLI five times faster than the
// published interval for the rest of expires_in, re-tripping the very limiter
// that emitted the 429.
func TestDeviceAuthorizationRetryAfterOnlySlowsPolling(t *testing.T) {
	const published = 5
	retryAfter := []string{"1", "900", "1"}
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		polls++
		if polls > len(retryAfter) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "pe1_device-session", "token_type": "Bearer",
				"expires_in": 900,
			})
			return
		}
		w.Header().Set("Retry-After", retryAfter[polls-1])
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("<html><head><title>429 Too Many Requests</title></head></html>"))
	}))
	defer server.Close()

	var waits []time.Duration
	result, err := pollDeviceAuthorization(
		context.Background(), server.Client(), server.URL, "ped1_secret", published,
		func(_ context.Context, duration time.Duration) error {
			waits = append(waits, duration)
			return nil
		},
	)
	if err != nil || result.AccessToken != "pe1_device-session" {
		t.Fatalf("poll = %+v, %v", result, err)
	}
	// Retry-After: 1 is ignored twice — once below the published 5s, once below
	// the raised 60s — and Retry-After: 900 is capped at the same 60s ceiling
	// slow_down uses rather than parking the CLI for a quarter of an hour.
	wantWaits := []time.Duration{
		published * time.Second, published * time.Second,
		60 * time.Second, 60 * time.Second,
	}
	if len(waits) != len(wantWaits) {
		t.Fatalf("waits=%v, want %v", waits, wantWaits)
	}
	for index := range wantWaits {
		if waits[index] != wantWaits[index] {
			t.Fatalf("waits=%v, want %v", waits, wantWaits)
		}
	}
	for _, wait := range waits {
		if wait < published*time.Second {
			t.Fatalf("wait %v is below the server-published %ds interval; a "+
				"middlebox header sped the client up", wait, published)
		}
	}
}

// invalid_grant is terminal even though it never reaches the retry path above.
func TestDeviceAuthorizationStopsOnTerminalOAuthCodes(t *testing.T) {
	for _, code := range []string{
		"access_denied", "expired_token", "invalid_grant",
		"invalid_request", "unauthorized_client",
	} {
		polls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			polls++
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": code})
		}))
		_, err := pollDeviceAuthorization(
			context.Background(), server.Client(), server.URL, "ped1_secret", 5,
			func(context.Context, time.Duration) error { return nil },
		)
		server.Close()
		var oauthError *deviceTokenError
		if !errors.As(err, &oauthError) || oauthError.Code != code {
			t.Errorf("%s: err=%v", code, err)
		}
		if polls != 1 {
			t.Errorf("%s: polled %d times, want 1", code, polls)
		}
	}
}

func TestDeviceAuthorizationRejectsMismatchedCompleteURI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "ped1_high-entropy-device-capability",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          "https://dashboard.example.test/device",
			"verification_uri_complete": "https://dashboard.example.test/device?user_code=ABCD-EFGH&device_code=unsafe",
			"expires_in":                600, "interval": 5,
		})
	}))
	defer server.Close()
	if _, err := startDeviceAuthorization(server.Client(), server.URL); err == nil {
		t.Fatal("complete URI containing an extra capability parameter was accepted")
	}
}

func TestDeviceAuthorizationDeniedAndTokenOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "access_denied", "error_description": "reflected pe1_do-not-log",
		})
	}))
	defer server.Close()
	_, err := requestDeviceToken(server.Client(), server.URL, "ped1_secret")
	var denied *deviceTokenError
	if !errors.As(err, &denied) || denied.Code != "access_denied" ||
		strings.Contains(err.Error(), "pe1_") {
		t.Fatalf("denied error=%v", err)
	}

	result := tokenExchangeResult{AccessToken: "pe1_output-once", ExpiresIn: 60}
	var stdout, stderr strings.Builder
	if err := emitAccessToken(result, "", &stdout, &stderr); err != nil ||
		stdout.String() != "pe1_output-once\n" || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q err=%v", stdout.String(), stderr.String(), err)
	}

	path := filepath.Join(t.TempDir(), "session-token")
	if err := os.WriteFile(path, []byte("old-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := emitAccessToken(result, path, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "pe1_output-once\n" || info.Mode().Perm() != 0o600 {
		t.Fatalf("content=%q mode=%#o", content, info.Mode().Perm())
	}

	target := filepath.Join(t.TempDir(), "do-not-overwrite")
	if err := os.WriteFile(target, []byte("preserved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(filepath.Dir(target), "session-link")
	if err := os.Symlink(target, symlink); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if err := emitAccessToken(result, symlink, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	preserved, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := os.ReadFile(symlink)
	if err != nil {
		t.Fatal(err)
	}
	if string(preserved) != "preserved\n" || string(replaced) != "pe1_output-once\n" {
		t.Fatalf("symlink target=%q replacement=%q", preserved, replaced)
	}
}

func TestFetchBinhostProfile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/binhosts" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"binhosts":[
			{"profile_id":"pe/base","arch":"amd64","binhost_path":"releases/amd64/binpackages/23.0/x86-64_pe-base","default":true,"sync_path":"/binpkgs/releases/amd64/binpackages/23.0/x86-64_pe-base"},
			{"profile_id":"pe/desktop","arch":"amd64","binhost_path":"releases/amd64/binpackages/23.0/x86-64_pe-desktop","sync_path":"/binpkgs/releases/amd64/binpackages/23.0/x86-64_pe-desktop"}
		]}`))
	}))
	defer server.Close()

	selected, err := fetchBinhostProfile(server.Client(), server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if selected.ProfileID != "pe/base" {
		t.Fatalf("default profile = %q", selected.ProfileID)
	}
	selected, err = fetchBinhostProfile(server.Client(), server.URL, "pe/desktop")
	if err != nil {
		t.Fatal(err)
	}
	if selected.SyncPath != "/binpkgs/releases/amd64/binpackages/23.0/x86-64_pe-desktop" {
		t.Fatalf("desktop sync path = %q", selected.SyncPath)
	}
	if _, err := fetchBinhostProfile(server.Client(), server.URL, "pe/missing"); err == nil {
		t.Fatal("unknown profile was accepted")
	}
}

func TestNormalizeFingerprintRequiresFullHex(t *testing.T) {
	input := "0123 4567 89ab cdef 0123 4567 89ab cdef 0123 4567"
	got, err := normalizeFingerprint(input)
	if err != nil || got != "0123456789ABCDEF0123456789ABCDEF01234567" {
		t.Fatalf("fingerprint=%q err=%v", got, err)
	}
	for _, invalid := range []string{"DEADBEEF", strings.Repeat("Z", 40)} {
		if _, err := normalizeFingerprint(invalid); err == nil {
			t.Errorf("invalid fingerprint %q accepted", invalid)
		}
	}
}

func TestFetchReleasePublicKeyIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		_, _ = w.Write([]byte("PUBLIC KEY"))
	}))
	defer server.Close()
	document, err := fetchReleasePublicKey(
		&http.Client{Timeout: time.Second}, server.URL,
	)
	if err != nil || string(document) != "PUBLIC KEY" {
		t.Fatalf("document=%q err=%v", document, err)
	}
}

func TestLoadConfigFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	configData := `{
  "package_use": {
    "dev-lang/python": ["ssl", "threads"]
  },
  "package_keywords": {
    "dev-lang/rust": ["~amd64"]
  },
  "make_conf": {
    "CFLAGS": "-O2 -pipe",
    "MAKEOPTS": "-j4"
  },
  "environment": {
    "LC_ALL": "C"
  }
}`

	if err := os.WriteFile(configPath, []byte(configData), 0600); err != nil {
		t.Fatalf("Failed to create test config: %v", err)
	}

	config, err := loadConfigFromFile(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if len(config.PackageUse) != 1 {
		t.Errorf("Expected 1 package.use entry, got %d", len(config.PackageUse))
	}

	if len(config.PackageKeywords) != 1 {
		t.Errorf("Expected 1 package.keywords entry, got %d", len(config.PackageKeywords))
	}

	if len(config.MakeConf) != 2 {
		t.Errorf("Expected 2 make.conf entries, got %d", len(config.MakeConf))
	}
}

func TestLoadConfigFromFileInvalid(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.json")

	configData := `invalid json`

	if err := os.WriteFile(configPath, []byte(configData), 0600); err != nil {
		t.Fatalf("Failed to create test config: %v", err)
	}

	_, err := loadConfigFromFile(configPath)
	if err == nil {
		t.Error("Expected error for invalid JSON, got nil")
	}
}

func TestLoadConfigFromFileNotExist(t *testing.T) {
	_, err := loadConfigFromFile("/nonexistent/path/config.json")
	if err == nil {
		t.Error("Expected error for nonexistent file, got nil")
	}
}

func TestCreateConfigBundle(t *testing.T) {
	config := &builder.PortageConfig{
		PackageUse: map[string][]string{
			"dev-lang/python": {"ssl", "threads"},
		},
		PackageKeywords: map[string][]string{
			"dev-lang/rust": {"~amd64"},
		},
		MakeConf: map[string]string{
			"CFLAGS":   "-O2 -pipe",
			"MAKEOPTS": "-j4",
		},
		Environment: map[string]string{
			"LC_ALL": "C",
		},
	}

	packages := &builder.BuildPackageSpec{
		Packages: []builder.PackageSpec{
			{
				Atom:     "dev-lang/python",
				Version:  "3.11",
				UseFlags: []string{"ssl", "threads"},
			},
		},
	}

	metadata := builder.BundleMetadata{
		UserID:      "test-user",
		TargetArch:  "amd64",
		Profile:     "default/linux/amd64/23.0",
		Description: "Test build",
	}

	transfer := builder.NewConfigTransfer("")
	bundle, err := transfer.CreateConfigBundle(config, packages, metadata)
	if err != nil {
		t.Fatalf("Failed to create config bundle: %v", err)
	}

	if bundle == nil {
		t.Fatal("Expected non-nil bundle")
	}

	if bundle.Config == nil {
		t.Error("Expected non-nil bundle.Config")
	}

	if bundle.Packages == nil {
		t.Error("Expected non-nil bundle.Packages")
	}

	if bundle.Metadata.UserID != "test-user" {
		t.Errorf("Expected UserID 'test-user', got '%s'", bundle.Metadata.UserID)
	}
}

func TestParseFlagsAndKeywords(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "Single flag",
			input:    "ssl",
			expected: []string{"ssl"},
		},
		{
			name:     "Multiple flags",
			input:    "ssl,threads,ipv6",
			expected: []string{"ssl", "threads", "ipv6"},
		},
		{
			name:     "Flags with spaces",
			input:    "ssl, threads, ipv6",
			expected: []string{"ssl", "threads", "ipv6"},
		},
		{
			name:     "Empty string",
			input:    "",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Exercise the production parser directly.
			result := parseCSV(tt.input)

			if len(result) != len(tt.expected) {
				t.Errorf("Expected %d items, got %d", len(tt.expected), len(result))
				return
			}

			for i, exp := range tt.expected {
				if result[i] != exp {
					t.Errorf("Expected item %d to be '%s', got '%s'", i, exp, result[i])
				}
			}
		})
	}
}
