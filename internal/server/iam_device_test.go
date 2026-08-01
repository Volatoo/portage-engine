package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/iam"
	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/pkg/config"
)

type fakeDeviceAuthorizationStore struct {
	create func(context.Context, string, string, int, int) (bool, error)
	decide func(
		context.Context, string, iam.Principal, bool,
		persistence.IAMSessionPolicy,
	) (persistence.DeviceAuthorization, error)
	poll func(
		context.Context, string, string, persistence.IAMSessionPolicy,
	) (persistence.DeviceAuthorizationPoll, error)
}

func (f *fakeDeviceAuthorizationStore) CreateDeviceAuthorization(
	ctx context.Context, digest, code string, ttl, interval int,
) (bool, error) {
	return f.create(ctx, digest, code, ttl, interval)
}

func (f *fakeDeviceAuthorizationStore) DecideDeviceAuthorization(
	ctx context.Context, code string, principal iam.Principal, approve bool,
	policy persistence.IAMSessionPolicy,
) (persistence.DeviceAuthorization, error) {
	return f.decide(ctx, code, principal, approve, policy)
}

func (f *fakeDeviceAuthorizationStore) PollDeviceAuthorization(
	ctx context.Context, deviceDigest, tokenDigest string,
	policy persistence.IAMSessionPolicy,
) (persistence.DeviceAuthorizationPoll, error) {
	return f.poll(ctx, deviceDigest, tokenDigest, policy)
}

func TestDeviceAuthorizationStartStoresOnlyDigest(t *testing.T) {
	var storedDigest, storedUserCode string
	store := &fakeDeviceAuthorizationStore{create: func(
		_ context.Context, digest, code string, ttl, interval int,
	) (bool, error) {
		storedDigest, storedUserCode = digest, code
		if ttl != int(deviceAuthorizationTTL/time.Second) ||
			interval != deviceAuthorizationInterval {
			t.Fatalf("ttl=%d interval=%d", ttl, interval)
		}
		return true, nil
	}}
	server := New(&config.ServerConfig{
		AuthMode:                           "oidc",
		DeviceAuthorizationVerificationURI: "https://dashboard.example.test/device",
	})
	server.deviceAuthorizations = store
	request := httptest.NewRequest(
		http.MethodPost, "/api/v1/iam/device/authorization", strings.NewReader(""),
	)
	response := httptest.NewRecorder()
	server.handleIAMDeviceAuthorization(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body deviceAuthorizationResultFixture
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(body.DeviceCode))
	if storedDigest != hex.EncodeToString(digest[:]) ||
		strings.Contains(storedDigest, body.DeviceCode) ||
		storedUserCode != body.UserCode {
		t.Fatalf("stored digest/code mismatch digest=%q user=%q body=%+v",
			storedDigest, storedUserCode, body)
	}
	complete, err := url.Parse(body.VerificationURIComplete)
	if err != nil || complete.Query().Get("user_code") != body.UserCode ||
		complete.Query().Get("device_code") != "" || body.ExpiresIn != 600 ||
		body.Interval != deviceAuthorizationInterval {
		t.Fatalf("device response=%+v parse=%v", body, err)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control=%q", response.Header().Get("Cache-Control"))
	}
}

func TestDeviceAuthorizationStartRejectsUnsafeVerificationURI(t *testing.T) {
	called := false
	store := &fakeDeviceAuthorizationStore{create: func(
		_ context.Context, _, _ string, _, _ int,
	) (bool, error) {
		called = true
		return true, nil
	}}
	server := New(&config.ServerConfig{
		AuthMode:                           "oidc",
		DeviceAuthorizationVerificationURI: "https://dashboard.example.test/device?unsafe=1",
	})
	server.deviceAuthorizations = store
	request := httptest.NewRequest(
		http.MethodPost, "/api/v1/iam/device/authorization", strings.NewReader(""),
	)
	response := httptest.NewRecorder()
	server.handleIAMDeviceAuthorization(response, request)
	if response.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("unsafe verification URI status=%d called=%t body=%s",
			response.Code, called, response.Body.String())
	}
}

type deviceAuthorizationResultFixture struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int    `json:"interval"`
}

func TestDeviceDecisionRequiresFederatedPlatformSession(t *testing.T) {
	called := false
	store := &fakeDeviceAuthorizationStore{decide: func(
		_ context.Context, code string, principal iam.Principal, approve bool,
		_ persistence.IAMSessionPolicy,
	) (persistence.DeviceAuthorization, error) {
		called = true
		if code != "ABCD-EFGH" || !approve || principal.SubjectID != "subject-1" {
			t.Fatalf("decision code=%q approve=%t principal=%+v", code, approve, principal)
		}
		return persistence.DeviceAuthorization{ID: "device-1", Status: "approved"}, nil
	}}
	server := New(&config.ServerConfig{})
	server.deviceAuthorizations = store

	request := httptest.NewRequest(http.MethodPost, "/api/v1/iam/device/decision",
		strings.NewReader(`{"user_code":"abcd efgh","decision":"approve"}`))
	request = request.WithContext(iam.WithPrincipal(request.Context(), iam.Principal{
		Authentication: "legacy-api-key", SystemAdmin: true,
	}))
	response := httptest.NewRecorder()
	server.handleIAMDeviceDecision(response, request)
	if response.Code != http.StatusForbidden || called {
		t.Fatalf("legacy decision status=%d called=%t", response.Code, called)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/iam/device/decision",
		strings.NewReader(`{"user_code":"ABCD-EFGH","decision":"approve"} {}`))
	request = request.WithContext(iam.WithPrincipal(request.Context(), iam.Principal{
		Authentication: "federated-session", SubjectID: "subject-1", SessionID: "session-1",
	}))
	response = httptest.NewRecorder()
	server.handleIAMDeviceDecision(response, request)
	if response.Code != http.StatusBadRequest || called {
		t.Fatalf("trailing decision status=%d called=%t", response.Code, called)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/iam/device/decision",
		strings.NewReader(`{"user_code":"abcd efgh","decision":"approve"}`))
	request = request.WithContext(iam.WithPrincipal(request.Context(), iam.Principal{
		Authentication: "federated-session", SubjectID: "subject-1", SessionID: "session-1",
	}))
	response = httptest.NewRecorder()
	server.handleIAMDeviceDecision(response, request)
	if response.Code != http.StatusOK || !called {
		t.Fatalf("federated decision status=%d body=%s called=%t",
			response.Code, response.Body.String(), called)
	}
}

func TestDeviceTokenRejectsUnexpectedFormFields(t *testing.T) {
	called := false
	store := &fakeDeviceAuthorizationStore{poll: func(
		_ context.Context, _, _ string, _ persistence.IAMSessionPolicy,
	) (persistence.DeviceAuthorizationPoll, error) {
		called = true
		return persistence.DeviceAuthorizationPoll{}, nil
	}}
	server := New(&config.ServerConfig{AuthMode: "oidc"})
	server.deviceAuthorizations = store
	form := url.Values{
		"grant_type": {deviceGrantType}, "device_code": {"ped1_secret"},
		"unexpected": {"value"},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/iam/device/token",
		strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.handleIAMDeviceToken(response, request)
	if response.Code != http.StatusBadRequest || called ||
		!strings.Contains(response.Body.String(), "invalid_request") {
		t.Fatalf("unexpected form status=%d called=%t body=%s",
			response.Code, called, response.Body.String())
	}
}

func TestDeviceTokenProtocolErrorsAndSuccess(t *testing.T) {
	principal := iam.Principal{
		Issuer: "https://issuer.example.test", Subject: "alice",
		SubjectID: "subject-1", SessionID: "session-new",
		TokenExpiresAt: time.Now().Add(time.Hour),
	}
	for _, test := range []struct {
		name       string
		pollStatus string
		wantCode   int
		wantError  string
	}{
		{"pending", persistence.DeviceAuthorizationPending, 400, "authorization_pending"},
		{"slow down", persistence.DeviceAuthorizationSlowDown, 400, "slow_down"},
		{"denied", persistence.DeviceAuthorizationDenied, 400, "access_denied"},
		{"expired", persistence.DeviceAuthorizationExpired, 400, "expired_token"},
		{"approved", persistence.DeviceAuthorizationApproved, 200, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeDeviceAuthorizationStore{poll: func(
				_ context.Context, deviceDigest, tokenDigest string,
				_ persistence.IAMSessionPolicy,
			) (persistence.DeviceAuthorizationPoll, error) {
				if len(deviceDigest) != 64 || len(tokenDigest) != 64 {
					t.Fatalf("digests device=%q token=%q", deviceDigest, tokenDigest)
				}
				return persistence.DeviceAuthorizationPoll{
					Status: test.pollStatus, IntervalSeconds: 10, Principal: principal,
				}, nil
			}}
			server := New(&config.ServerConfig{AuthMode: "oidc"})
			server.deviceAuthorizations = store
			form := url.Values{
				"grant_type": {deviceGrantType}, "device_code": {"ped1_secret"},
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/iam/device/token",
				strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			server.handleIAMDeviceToken(response, request)
			if response.Code != test.wantCode {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if test.wantError != "" {
				var body map[string]any
				_ = json.NewDecoder(response.Body).Decode(&body)
				if body["error"] != test.wantError || strings.Contains(response.Body.String(), "pe1_") {
					t.Fatalf("error body=%v", body)
				}
				return
			}
			var body map[string]any
			_ = json.NewDecoder(response.Body).Decode(&body)
			accessToken, _ := body["access_token"].(string)
			if !strings.HasPrefix(accessToken, "pe1_") ||
				strings.Contains(request.URL.RequestURI(), accessToken) {
				t.Fatalf("success body=%v request_uri=%q", body, request.URL.RequestURI())
			}
		})
	}
}
