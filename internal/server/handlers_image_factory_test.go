package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/pkg/config"
)

func newImageFactoryTestServer(t *testing.T, statusPath string) *Server {
	t.Helper()
	s := New(&config.ServerConfig{BinpkgPath: t.TempDir(), MaxWorkers: 1, ImageFactoryStatusPath: statusPath})
	s.builder.SetBuildCatalog(catalog.NewCompatibility(catalog.CompatibilityOptions{Provider: "pve", BuildMode: "native-gentoo", Template: "9000"}))
	return s
}

func TestImageFactoryStatusCatalogOnly(t *testing.T) {
	s := newImageFactoryTestServer(t, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/image-factory/status", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response imageFactoryStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Configured || response.Catalog.Version == 0 || len(response.Catalog.Profiles) != 1 {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestImageFactoryStatusLoadsStrictSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	data := `{"schema_version":1,"updated_at":"2026-07-23T00:00:00Z","overall_state":"blocked","milestones":[{"id":"IMG-4C","title":"Candidate build","state":"blocked"}],"blockers":[{"code":"OFFLINE_CLOSURE","summary":"Closure required"}],"desktop_e2e":{"state":"planned","strategy":"deterministic-first"}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newImageFactoryTestServer(t, path)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/image-factory/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response imageFactoryStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Configured || response.Status == nil || response.Status.Milestones[0].ID != "IMG-4C" {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestImageFactoryStatusRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	data := `{"schema_version":1,"updated_at":"2026-07-23T00:00:00Z","overall_state":"passed","milestones":[],"desktop_e2e":{"state":"planned"},"secret":"must-not-pass"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newImageFactoryTestServer(t, path)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/image-factory/status", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
}
