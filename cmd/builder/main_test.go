package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/pkg/config"
)

func reusableBuilderTestConfig(t *testing.T, workers int) *config.BuilderConfig {
	t.Helper()
	root := t.TempDir()
	return &config.BuilderConfig{
		Workers:            workers,
		NativeJobPolicy:    "unsafe-reuse",
		DataDir:            filepath.Join(root, "data"),
		WorkDir:            filepath.Join(root, "work"),
		ArtifactDir:        filepath.Join(root, "artifacts"),
		PersistenceEnabled: false,
	}
}

func TestHealthEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	if w.Body.String() != "OK" {
		t.Errorf("Expected body 'OK', got '%s'", w.Body.String())
	}
}

func TestNativeSingleUseHealthAndBuildEndpointDrain(t *testing.T) {
	root := t.TempDir()
	cfg := &config.BuilderConfig{
		Workers:            0,
		NativeJobPolicy:    "single-use",
		DataDir:            filepath.Join(root, "data"),
		WorkDir:            filepath.Join(root, "work"),
		ArtifactDir:        filepath.Join(root, "artifacts"),
		PersistenceEnabled: false,
	}
	bldr := builder.NewLocalBuilder(0, cfg)
	handler := setupHTTPHandlers(bldr)

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("fresh native builder health = %d, want 200", health.Code)
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/api/v1/build",
		strings.NewReader(`{"package_name":"app-misc/hello"}`)))
	if first.Code != http.StatusOK {
		t.Fatalf("first native build = %d: %s", first.Code, first.Body.String())
	}

	draining := httptest.NewRecorder()
	handler.ServeHTTP(draining, httptest.NewRequest(http.MethodGet, "/health", nil))
	if draining.Code != http.StatusServiceUnavailable {
		t.Fatalf("consumed native builder health = %d, want 503", draining.Code)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/api/v1/build",
		strings.NewReader(`{"package_name":"app-misc/jq"}`)))
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second native build = %d, want 503: %s", second.Code, second.Body.String())
	}
}

func TestStatusEndpoint(t *testing.T) {
	cfg := reusableBuilderTestConfig(t, 2)
	bldr := builder.NewLocalBuilder(cfg.Workers, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		status := bldr.GetStatus()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var status map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if _, ok := status["workers"]; !ok {
		t.Error("Expected 'workers' field in status response")
	}
}

func TestBuildEndpointInvalidMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/build", nil)
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
	})

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestBuildEndpointInvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/build", bytes.NewBufferString("invalid json"))
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req builder.LocalBuildRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
	})

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}

func TestBuildEndpointRejectsUnknownFields(t *testing.T) {
	bldr := builder.NewLocalBuilder(1, reusableBuilderTestConfig(t, 1))
	handler := setupHTTPHandlers(bldr)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/build", strings.NewReader(
		`{"package_name":"app-misc/hello","unexpected":"value"}`,
	))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown field, got %d", w.Code)
	}
}

func TestBuildEndpointRejectsOversizedBody(t *testing.T) {
	bldr := builder.NewLocalBuilder(1, reusableBuilderTestConfig(t, 1))
	handler := setupHTTPHandlers(bldr)
	body := `{"package_name":"app-misc/hello","padding":"` +
		strings.Repeat("x", int(builder.MaxBuildRequestBodyBytes)) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/build", strings.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d", w.Code)
	}
}

func TestJobStatusEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/test-job-id", nil)
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jobID := r.URL.Path[len("/api/v1/jobs/"):]
		if jobID == "" {
			http.Error(w, "Job ID required", http.StatusBadRequest)
			return
		}

		response := map[string]interface{}{
			"job_id": jobID,
			"status": "running",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response["job_id"] != "test-job-id" {
		t.Errorf("Expected job_id 'test-job-id', got '%v'", response["job_id"])
	}
}

func TestConfigLoad(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "builder.conf")

	configData := `port=9090
workers=4
gpg_enabled=false
work_dir=/tmp/portage-work
`

	if err := os.WriteFile(configPath, []byte(configData), 0600); err != nil {
		t.Fatalf("Failed to create test config: %v", err)
	}

	cfg, err := config.LoadBuilderConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.Port != 9090 {
		t.Errorf("Expected port 9090, got %d", cfg.Port)
	}

	if cfg.Workers != 2 {
		t.Errorf("Expected 2 workers, got %d", cfg.Workers)
	}
}

func TestBuildRequestSubmission(t *testing.T) {
	cfg := reusableBuilderTestConfig(t, 0)
	bldr := builder.NewLocalBuilder(cfg.Workers, cfg)

	buildReq := &builder.LocalBuildRequest{
		PackageName: "app-editors/vim",
		Version:     "9.0.0",
		UseFlags: map[string]string{
			"python": "enabled",
		},
		Environment: map[string]string{},
	}

	reqBody, err := json.Marshal(buildReq)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/build", bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req builder.LocalBuildRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		jobID, err := bldr.SubmitBuild(&req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		response := map[string]interface{}{
			"job_id": jobID,
			"status": "queued",
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response["status"] != "queued" {
		t.Errorf("Expected status 'queued', got '%v'", response["status"])
	}

	if _, ok := response["job_id"]; !ok {
		t.Error("Expected 'job_id' in response")
	}
}

func TestArtifactInfoEndpointMissingJobID(t *testing.T) {
	cfg := reusableBuilderTestConfig(t, 1)
	bldr := builder.NewLocalBuilder(cfg.Workers, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/info/", nil)
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jobID := r.URL.Path[len("/api/v1/artifacts/info/"):]
		if jobID == "" {
			http.Error(w, "Job ID required", http.StatusBadRequest)
			return
		}

		info, err := bldr.GetArtifactInfo(jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	})

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}

func TestArtifactInfoEndpointNotFound(t *testing.T) {
	cfg := reusableBuilderTestConfig(t, 1)
	bldr := builder.NewLocalBuilder(cfg.Workers, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/info/non-existent-job", nil)
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jobID := r.URL.Path[len("/api/v1/artifacts/info/"):]
		if jobID == "" {
			http.Error(w, "Job ID required", http.StatusBadRequest)
			return
		}

		info, err := bldr.GetArtifactInfo(jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	})

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", w.Code)
	}
}

func TestArtifactDownloadEndpointMissingJobID(t *testing.T) {
	cfg := reusableBuilderTestConfig(t, 1)
	bldr := builder.NewLocalBuilder(cfg.Workers, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/download/", nil)
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jobID := r.URL.Path[len("/api/v1/artifacts/download/"):]
		if jobID == "" {
			http.Error(w, "Job ID required", http.StatusBadRequest)
			return
		}

		artifactPath, err := bldr.GetArtifactPath(jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		http.ServeFile(w, r, artifactPath)
	})

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}

func TestArtifactDownloadEndpointNotFound(t *testing.T) {
	cfg := reusableBuilderTestConfig(t, 1)
	bldr := builder.NewLocalBuilder(cfg.Workers, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/download/non-existent-job", nil)
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jobID := r.URL.Path[len("/api/v1/artifacts/download/"):]
		if jobID == "" {
			http.Error(w, "Job ID required", http.StatusBadRequest)
			return
		}

		artifactPath, err := bldr.GetArtifactPath(jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		http.ServeFile(w, r, artifactPath)
	})

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", w.Code)
	}
}

// TestAuthMiddleware verifies the builder rejects requests without the shared
// token on protected paths, allows /health, and accepts the correct token.
func TestAuthMiddleware(t *testing.T) {
	const token = "shared-builder-secret"
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := authMiddleware(token, ok)

	// No token on a protected path → 401.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/build", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no token: expected 401, got %d", w.Code)
	}

	// Wrong token → 401.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/build", nil)
	req.Header.Set("X-API-Key", "wrong")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: expected 401, got %d", w.Code)
	}

	// Correct token via X-API-Key → passes.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/build", nil)
	req.Header.Set("X-API-Key", token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("X-API-Key: expected 200, got %d", w.Code)
	}

	// Correct token via Authorization: Bearer → passes.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/build", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("Bearer: expected 200, got %d", w.Code)
	}

	// /health is always public even with a token configured.
	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/health: expected 200, got %d", w.Code)
	}

	// Empty token disables auth (still logs a startup warning elsewhere).
	openHandler := authMiddleware("", ok)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/build", nil)
	w = httptest.NewRecorder()
	openHandler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("empty token: expected 200 (auth disabled), got %d", w.Code)
	}
}
