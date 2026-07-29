package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slchris/portage-engine/internal/binpkg"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestPublicPackagesFiltersAndRedactsInternalMetadata(t *testing.T) {
	server := New(&config.ServerConfig{BinpkgPath: t.TempDir()})
	defer server.builder.Shutdown()
	if err := server.binpkgStore.Add(&binpkg.Package{
		Name: "app-misc/jq", Version: "1.8.2", Arch: "amd64",
		UseFlags: []string{"oniguruma"}, Dependencies: []string{"secret/dependency"},
		Path: "app-misc/jq-1.8.2.gpkg.tar",
		Metadata: map[string]string{
			"BUILD_ID": "internal-build-id",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.binpkgStore.Add(&binpkg.Package{
		Name: "sys-apps/coreutils", Version: "9.7", Arch: "amd64",
		Path: "sys-apps/coreutils-9.7.gpkg.tar",
	}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(
		http.MethodGet, "/api/v1/public/packages?q=jq&limit=10", nil,
	)
	response := httptest.NewRecorder()
	server.handlePublicPackages(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "internal-build-id") ||
		strings.Contains(response.Body.String(), "secret/dependency") {
		t.Fatalf("internal package metadata leaked: %s", response.Body.String())
	}
	var result publicPackageResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || len(result.Packages) != 1 {
		t.Fatalf("unexpected search result: %#v", result)
	}
	pkg := result.Packages[0]
	if pkg.Name != "app-misc/jq" ||
		pkg.DownloadPath != "/binpkgs/app-misc/jq-1.8.2.gpkg.tar" {
		t.Fatalf("unexpected public package: %#v", pkg)
	}
	if response.Header().Get("Cache-Control") != "public, max-age=30" {
		t.Fatalf("unexpected cache policy: %q", response.Header().Get("Cache-Control"))
	}
}

func TestPublicPackagesRejectsInvalidPaging(t *testing.T) {
	server := New(&config.ServerConfig{BinpkgPath: t.TempDir()})
	defer server.builder.Shutdown()
	for _, target := range []string{
		"/api/v1/public/packages?limit=201",
		"/api/v1/public/packages?offset=-1",
		"/api/v1/public/packages?q=" + strings.Repeat("x", 201),
		"/api/v1/public/packages?profile_id=not-published",
	} {
		response := httptest.NewRecorder()
		server.handlePublicPackages(
			response, httptest.NewRequest(http.MethodGet, target, nil),
		)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d, want 400", target, response.Code)
		}
	}
}

func TestPublicStatusIsCoarseAndAlwaysReadable(t *testing.T) {
	server := New(&config.ServerConfig{
		BinpkgPath: t.TempDir(),
		APIKey:     "private-control-plane-key",
	})
	defer server.builder.Shutdown()
	router := server.Router()

	response := httptest.NewRecorder()
	router.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, forbidden := range []string{
		"schema_version", "active_issuers", "registered", "last_error",
		"private-control-plane-key",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("public status leaked %q: %s", forbidden, body)
		}
	}
	var status publicServiceStatus
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != "operational" || len(status.Components) != 3 {
		t.Fatalf("unexpected public status: %#v", status)
	}
}

func TestPublicReadEndpointsBypassAPIAuthentication(t *testing.T) {
	server := New(&config.ServerConfig{
		BinpkgPath: t.TempDir(),
		APIKey:     "required-for-private-routes",
	})
	defer server.builder.Shutdown()
	router := server.Router()
	for _, target := range []string{
		"/api/v1/public/packages",
		"/api/v1/public/status",
		"/api/v1/binhosts",
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code == http.StatusUnauthorized {
			t.Errorf("%s unexpectedly required authentication", target)
		}
	}
}
