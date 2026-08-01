package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestRequestURIForLogRedactsVerificationCapability(t *testing.T) {
	token := strings.Repeat("0", 32)
	request := httptest.NewRequest(http.MethodGet, "/verify-binhost/"+token+"/app-misc/jq.gpkg.tar?ignored=yes", nil)

	logged := requestURIForLog(request)
	if strings.Contains(logged, token) {
		t.Fatalf("verification capability leaked into access log path: %s", logged)
	}
	if logged != "/verify-binhost/<redacted>/app-misc/jq.gpkg.tar" {
		t.Fatalf("unexpected redacted path: %s", logged)
	}
}

func TestRequestURIForLogRedactsDeviceQuery(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/iam/device/token?access_token=pe1_must-not-be-logged", nil)
	logged := requestURIForLog(request)
	if logged != "/api/v1/iam/device/token?<redacted>" ||
		strings.Contains(logged, "pe1_") {
		t.Fatalf("device query leaked into access log path: %s", logged)
	}
}

func TestWriteAdmissionErrorIsMachineReadable(t *testing.T) {
	w := httptest.NewRecorder()
	err := builder.NewAdmissionError(
		"queued_limit_reached", 10, 10, 1500*time.Millisecond,
		fmt.Errorf("queue full"),
	)
	admission, ok := builder.AsAdmissionError(err)
	if !ok {
		t.Fatal("admission error was not typed")
	}
	writeAdmissionError(w, admission)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("admission response status=%d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "queued_limit_reached" || body["limit"] != float64(10) ||
		body["used"] != float64(10) {
		t.Fatalf("admission response=%v", body)
	}
}

func TestRequestURIForLogLeavesOrdinaryPathIntact(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/builds/status?job_id=job-1", nil)
	if got := requestURIForLog(request); got != request.RequestURI {
		t.Fatalf("ordinary request URI changed: %q", got)
	}
}

func TestAuthenticationRateLimitedRequest(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodGet, "/api/v1/builds/status", true},
		{http.MethodPost, "/api/v1/builds/submit", true},
		// Anonymous but credential-issuing: nothing else bounds them.
		{http.MethodPost, "/api/v1/iam/device/authorization", true},
		{http.MethodPost, "/api/v1/iam/device/token", true},
		{http.MethodPost, "/api/v1/iam/exchange", true},
		// Anonymous and read-only: the binhost surface must not share a
		// pre-auth budget with a login brute force.
		{http.MethodGet, "/api/v1/binhosts", false},
		{http.MethodGet, "/api/v1/gpg/public-key", false},
		{http.MethodGet, "/api/v1/public/packages", false},
		{http.MethodGet, "/api/v1/public/status", false},
		{http.MethodGet, "/health", false},
		{http.MethodOptions, "/api/v1/builds/submit", false},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, nil)
		if got := authenticationRateLimitedRequest(request); got != test.want {
			t.Errorf("%s %s rate limited=%v, want %v",
				test.method, test.path, got, test.want)
		}
	}
}

// `emerge --getbinpkg` reads these anonymously from every consumer of the
// service. Behind an edge TRUSTED_PROXY_CIDRS does not declare they would all
// key on the proxy's own address, so a pre-auth budget over them is a global
// outage of the binhost triggered by a single busy client.
func TestAuthenticationRateLimitExemptsAnonymousBinhostReads(t *testing.T) {
	for _, path := range []string{
		"/api/v1/binhosts",
		"/api/v1/gpg/public-key",
		"/api/v1/public/packages",
		"/api/v1/public/status",
	} {
		if !publicControlPlanePath(path) {
			t.Fatalf("%s is no longer served anonymously", path)
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if authenticationRateLimitedRequest(request) {
			t.Errorf("%s is inside the pre-auth rate-limit budget", path)
		}
	}
}

// The exemption above must not swallow the routes the budget exists for: these
// are anonymous too, but they issue credentials.
func TestAuthenticationRateLimitCoversAnonymousCredentialRoutes(t *testing.T) {
	for _, path := range []string{
		"/api/v1/iam/device/authorization",
		"/api/v1/iam/device/token",
		"/api/v1/iam/exchange",
	} {
		if !publicControlPlanePath(path) {
			t.Fatalf("%s is no longer served anonymously", path)
		}
		request := httptest.NewRequest(http.MethodPost, path, nil)
		if !authenticationRateLimitedRequest(request) {
			t.Errorf("%s escaped the pre-auth rate-limit budget", path)
		}
	}
}

func TestClientAddressUsesRightmostUntrustedForwardedHop(t *testing.T) {
	tests := []struct {
		name      string
		proxies   []string
		remote    string
		forwarded []string
		want      string
	}{
		{
			name:   "no trusted proxy ignores the header",
			remote: "10.9.0.7:41234", forwarded: []string{"203.0.113.9"},
			want: "10.9.0.7",
		},
		{
			name:    "edge hop is believed",
			proxies: []string{"10.9.0.0/16"},
			remote:  "10.9.0.7:41234", forwarded: []string{"203.0.113.9"},
			want: "203.0.113.9",
		},
		{
			name:    "client-appended hops are ignored",
			proxies: []string{"10.9.0.0/16"},
			remote:  "10.9.0.7:41234",
			// A caller can prepend anything; only the rightmost hop was
			// observed by the trusted edge.
			forwarded: []string{"198.51.100.1, 203.0.113.9"},
			want:      "203.0.113.9",
		},
		{
			name:    "chained trusted proxies fall through to the client",
			proxies: []string{"10.9.0.0/16", "10.8.0.0/16"},
			remote:  "10.9.0.7:41234",
			forwarded: []string{
				"203.0.113.9, 10.8.0.4", "10.9.0.2",
			},
			want: "203.0.113.9",
		},
		{
			name:    "IPv6 hop keeps one spelling",
			proxies: []string{"10.9.0.0/16"},
			remote:  "10.9.0.7:41234", forwarded: []string{"[2001:db8::5]:9000"},
			want: "2001:db8::5",
		},
		{
			name:    "untrusted client cannot forge its own bucket",
			proxies: []string{"10.9.0.0/16"},
			remote:  "203.0.113.20:5555", forwarded: []string{"10.9.0.7"},
			want: "203.0.113.20",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := New(&config.ServerConfig{
				BinpkgPath: t.TempDir(), TrustedProxyCIDRs: test.proxies,
			})
			defer s.builder.Shutdown()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/iam/device/token", nil)
			request.RemoteAddr = test.remote
			for _, value := range test.forwarded {
				request.Header.Add("X-Forwarded-For", value)
			}
			if got := s.clientAddress(request); got != test.want {
				t.Fatalf("clientAddress = %q, want %q", got, test.want)
			}
		})
	}
}
