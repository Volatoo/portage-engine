package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/pkg/config"
)

func settingsTestServer(t *testing.T) *Server {
	t.Helper()
	return New(&config.ServerConfig{
		MaxWorkers:          1,
		DataDir:             t.TempDir(),
		BinpkgPath:          t.TempDir(),
		CloudPVETokenSecret: "initial-secret",
		CloudAWSSecretKey:   "initial-aws-secret",
	})
}

// TestCloudSettingsGetRedactsSecrets: secrets never leave the server; the UI
// only learns whether one is stored.
func TestCloudSettingsGetRedactsSecrets(t *testing.T) {
	s := settingsTestServer(t)

	w := httptest.NewRecorder()
	s.handleCloudSettings(w, httptest.NewRequest(http.MethodGet, "/api/v1/settings/cloud", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if v, ok := resp["pve_token_secret"]; ok && v != "" {
		t.Errorf("pve_token_secret leaked in GET response: %v", v)
	}
	if v, ok := resp["aws_secret_key"]; ok && v != "" {
		t.Errorf("aws_secret_key leaked in GET response: %v", v)
	}
	if resp["has_pve_token_secret"] != true || resp["has_aws_secret_key"] != true {
		t.Errorf("has_* flags wrong: %v / %v", resp["has_pve_token_secret"], resp["has_aws_secret_key"])
	}
}

// TestCloudSettingsPutAppliesAndPersists: a PUT with empty secrets keeps the
// stored ones, applies immediately (RemoteBuilders visible to the manager),
// and persists to DATA_DIR/cloud-settings.json.
func TestCloudSettingsPutAppliesAndPersists(t *testing.T) {
	s := settingsTestServer(t)

	body, _ := json.Marshal(map[string]any{
		"provider":        "pve",
		"pve_endpoint":    "https://pve.lan:8006",
		"remote_builders": []string{"http://b1:9090", "http://b2:9090"},
		// secrets intentionally empty -> keep stored values
	})
	w := httptest.NewRecorder()
	s.handleCloudSettings(w, httptest.NewRequest(http.MethodPut, "/api/v1/settings/cloud", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	cs := s.builder.CloudSettings()
	if cs.PVETokenSecret != "initial-secret" {
		t.Errorf("empty PUT secret should keep the stored one, got %q", cs.PVETokenSecret)
	}
	if cs.AWSSecretKey != "initial-aws-secret" {
		t.Errorf("empty PUT aws secret should keep the stored one, got %q", cs.AWSSecretKey)
	}
	if len(cs.RemoteBuilders) != 2 || cs.RemoteBuilders[0] != "http://b1:9090" {
		t.Errorf("remote builders not applied: %v", cs.RemoteBuilders)
	}
	if cs.PVEEndpoint != "https://pve.lan:8006" {
		t.Errorf("endpoint not applied: %q", cs.PVEEndpoint)
	}
	if cs.BuildMode != "native-gentoo" {
		t.Errorf("settings were not normalized to native-gentoo: %q", cs.BuildMode)
	}

	// Persisted file exists, contains the secret (mode 0600), and loads back.
	path := filepath.Join(s.config.DataDir, "cloud-settings.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("settings not persisted: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected 0600 permissions, got %v", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	var persisted config.CloudSettings
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.PVETokenSecret != "initial-secret" {
		t.Errorf("persisted file lost the kept secret")
	}

	// A fresh server over the same DataDir picks the override up at startup.
	s2 := New(&config.ServerConfig{MaxWorkers: 1, DataDir: s.config.DataDir, BinpkgPath: t.TempDir()})
	s2.loadCloudSettingsOverride()
	if got := s2.builder.CloudSettings().PVEEndpoint; got != "https://pve.lan:8006" {
		t.Errorf("override not applied on startup: %q", got)
	}
}

// TestCloudSettingsRejectsBadProvider guards the provider whitelist.
func TestCloudSettingsRejectsBadProvider(t *testing.T) {
	s := settingsTestServer(t)
	body, _ := json.Marshal(map[string]any{"provider": "digitalocean"})
	w := httptest.NewRecorder()
	s.handleCloudSettings(w, httptest.NewRequest(http.MethodPut, "/api/v1/settings/cloud", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unsupported provider, got %d", w.Code)
	}
}

func TestCloudSettingsDatabaseModeRejectsCredentialPersistence(t *testing.T) {
	s := settingsTestServer(t)
	// The rejection happens before any repository operation, so a zero-value
	// repository is sufficient to model PostgreSQL authority mode.
	s.jobLedger = &persistence.JobRepository{}
	body, _ := json.Marshal(map[string]any{
		"provider":         "pve",
		"pve_token_secret": "must-not-be-persisted",
	})
	w := httptest.NewRecorder()
	s.handleCloudSettings(w, httptest.NewRequest(http.MethodPut, "/api/v1/settings/cloud", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a shared secret value, got %d: %s", w.Code, w.Body.String())
	}
	if got := s.builder.CloudSettings().PVETokenSecret; got != "initial-secret" {
		t.Fatalf("rejected update changed the injected credential: %q", got)
	}

	w = httptest.NewRecorder()
	s.handleCloudSettings(w, httptest.NewRequest(http.MethodGet, "/api/v1/settings/cloud", nil))
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if managed, _ := response["secret_values_managed_externally"].(bool); !managed {
		t.Fatal("database mode did not advertise externally managed credentials")
	}
}

func TestCloudSettingsRejectsVerificationBypass(t *testing.T) {
	s := settingsTestServer(t)
	body, _ := json.Marshal(map[string]any{
		"provider":            "pve",
		"skip_verify_install": true,
	})
	w := httptest.NewRecorder()
	s.handleCloudSettings(w, httptest.NewRequest(http.MethodPut, "/api/v1/settings/cloud", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for verification bypass, got %d: %s", w.Code, w.Body.String())
	}
	if s.builder.CloudSettings().SkipVerifyInstall {
		t.Fatal("verification bypass was applied despite rejection")
	}
}

func TestCloudSettingsRejectsRemovedDockerMode(t *testing.T) {
	s := settingsTestServer(t)
	body, _ := json.Marshal(map[string]any{
		"provider":   "pve",
		"build_mode": "docker",
	})
	w := httptest.NewRecorder()
	s.handleCloudSettings(w, httptest.NewRequest(http.MethodPut, "/api/v1/settings/cloud", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for removed Docker mode, got %d: %s", w.Code, w.Body.String())
	}
}
