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
		{http.MethodGet, "/api/v1/binhosts", false},
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
