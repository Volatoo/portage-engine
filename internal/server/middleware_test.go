package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestRequestURIForLogLeavesOrdinaryPathIntact(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/builds/status?job_id=job-1", nil)
	if got := requestURIForLog(request); got != request.RequestURI {
		t.Fatalf("ordinary request URI changed: %q", got)
	}
}
