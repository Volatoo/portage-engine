package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// TestCloudSettingsSaveKeepsTheStoredSecretAcrossAnEndpointChange guards the
// asymmetry between the two backfills, which look alike and are not.
//
// The test handler binds the stored credential to the stored endpoint, because
// lending it to a caller-named host is a way to read a secret this API
// deliberately redacts. Applying that same rule here would be a plausible
// tidy-up and would break the deployment shape this product ships: where the
// secret provider supplies the credential, the operator has no way to re-enter
// one through this route — containsCloudSettingSecrets refuses it — so a saved
// endpoint change would silently blank the credential and the next build would
// fail to reach any cluster at all.
func TestCloudSettingsSaveKeepsTheStoredSecretAcrossAnEndpointChange(t *testing.T) {
	s := settingsTestServer(t)
	if got := s.builder.CloudSettings().PVETokenSecret; got != "initial-secret" {
		t.Fatalf("fixture did not inject the credential: %q", got)
	}
	body, _ := json.Marshal(map[string]any{
		"provider":     "pve",
		"pve_endpoint": "https://pve-replacement.internal:8006",
	})
	w := httptest.NewRecorder()
	s.handleCloudSettings(w,
		httptest.NewRequest(http.MethodPut, "/api/v1/settings/cloud", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("saving a new endpoint: %d %s", w.Code, w.Body.String())
	}
	saved := s.builder.CloudSettings()
	if saved.PVEEndpoint != "https://pve-replacement.internal:8006" {
		t.Fatalf("endpoint was not applied: %q", saved.PVEEndpoint)
	}
	if saved.PVETokenSecret != "initial-secret" {
		t.Fatalf("moving the endpoint blanked the injected credential (%q), which the secret "+
			"provider is the only source of — the next build would reach no cluster at all",
			saved.PVETokenSecret)
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

// recordingPVE stands in for a host the caller names. It fails the test if a
// request ever arrives carrying one of the stored credentials, and reports what
// it did see so a regression names the leak.
func recordingPVE(t *testing.T, forbidden map[string]string) (*httptest.Server, *int32) {
	t.Helper()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		body, _ := io.ReadAll(r.Body)
		wire := r.Header.Get("Authorization") + " " + r.Header.Get("Cookie") + " " + string(body)
		for name, secret := range forbidden {
			if strings.Contains(wire, secret) {
				t.Errorf("stored %s reached %s %s: %q", name, r.Method, r.URL.RequestURI(), wire)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func postCloudSettingsTest(t *testing.T, s *Server, body map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleCloudSettingsTest(w, httptest.NewRequest(http.MethodPost, "/api/v1/settings/cloud/test", bytes.NewReader(encoded)))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

// TestCloudSettingsTestRefusesToLendStoredSecretToForeignEndpoint is the attack:
// the token secret and the password are unreadable through this API, so a caller
// who names a host of their own must not have them replayed to it. Both
// credential shapes are covered — the API token rides in an Authorization
// header, the password goes out as a cleartext form body on the ticket path.
func TestCloudSettingsTestRefusesToLendStoredSecretToForeignEndpoint(t *testing.T) {
	for _, stored := range []struct {
		name      string
		settings  config.CloudSettings
		forbidden map[string]string
	}{
		{
			name: "api token",
			settings: config.CloudSettings{
				Provider: "pve", PVEEndpoint: "https://pve.internal:8006",
				PVETokenID: "root@pam!terraform", PVETokenSecret: "stored-token-secret",
				PVETemplate: "gentoo-template",
			},
			forbidden: map[string]string{"token secret": "stored-token-secret"},
		},
		{
			name: "ticket password",
			settings: config.CloudSettings{
				Provider: "pve", PVEEndpoint: "https://pve.internal:8006",
				PVEUsername: "root@pam", PVEPassword: "stored-cleartext-password",
				PVETemplate: "gentoo-template",
			},
			forbidden: map[string]string{"password": "stored-cleartext-password"},
		},
	} {
		t.Run(stored.name, func(t *testing.T) {
			s := settingsTestServer(t)
			s.builder.UpdateCloudSettings(stored.settings.Clone())
			attacker, requests := recordingPVE(t, stored.forbidden)

			response := postCloudSettingsTest(t, s, map[string]any{
				"pve_endpoint": attacker.URL,
				"pve_insecure": true,
			})
			if ok, _ := response["ok"].(bool); ok {
				t.Fatalf("the foreign endpoint was tested with the stored credential: %v", response)
			}
			if got := atomic.LoadInt32(requests); got != 0 {
				t.Fatalf("%d request(s) reached the caller-named host", got)
			}
			if stored := s.builder.CloudSettings(); stored.PVEEndpoint != "https://pve.internal:8006" {
				t.Fatalf("the connectivity test rewrote the stored endpoint: %q", stored.PVEEndpoint)
			}
		})
	}
}

// TestCloudSettingsTestUsesOnlyTheCallersOwnCredentialElsewhere: naming another
// host is legitimate as long as the caller brings its own credential. The
// request may go out — but not with anything the stored settings hold back.
func TestCloudSettingsTestUsesOnlyTheCallersOwnCredentialElsewhere(t *testing.T) {
	s := settingsTestServer(t)
	s.builder.UpdateCloudSettings((&config.CloudSettings{
		Provider: "pve", PVEEndpoint: "https://pve.internal:8006",
		PVETokenID: "root@pam!terraform", PVETokenSecret: "stored-token-secret",
	}).Clone())
	other, requests := recordingPVE(t, map[string]string{"token secret": "stored-token-secret"})

	response := postCloudSettingsTest(t, s, map[string]any{
		"pve_endpoint":     other.URL,
		"pve_token_id":     "audit@pam!probe",
		"pve_token_secret": "callers-own-secret",
		"pve_insecure":     true,
	})
	if ok, _ := response["ok"].(bool); !ok {
		t.Fatalf("a self-credentialed test of another host was refused: %v", response)
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Fatalf("expected exactly one request, got %d", got)
	}
}

// TestCloudSettingsTestKeepsStoredSecretForTheStoredCluster pins the workflow
// the UI depends on: the dashboard never sees the secret, so it posts the
// settings back without one and the stored credential must still be used for
// the stored cluster — including when the endpoint differs only by the trailing
// slash the request builder strips anyway.
func TestCloudSettingsTestKeepsStoredSecretForTheStoredCluster(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	cluster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"type":"node","node":"pve1","status":"online","maxmem":100,"mem":10}]}`))
	}))
	defer cluster.Close()

	s := settingsTestServer(t)
	s.builder.UpdateCloudSettings((&config.CloudSettings{
		Provider: "pve", PVEEndpoint: cluster.URL, PVEInsecure: true,
		PVETokenID: "root@pam!terraform", PVETokenSecret: "stored-token-secret",
		PVETemplate: "gentoo-template",
	}).Clone())

	for _, posted := range []string{"", cluster.URL, cluster.URL + "/"} {
		response := postCloudSettingsTest(t, s, map[string]any{
			"pve_endpoint": posted,
			"pve_insecure": true,
		})
		if ok, _ := response["ok"].(bool); !ok {
			t.Fatalf("posting %q broke the stored-cluster test: %v", posted, response)
		}
		nodes, _ := response["nodes"].([]any)
		if len(nodes) != 1 {
			t.Fatalf("posting %q returned %d nodes", posted, len(nodes))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("expected three cluster queries, got %d", len(seen))
	}
	for _, authorization := range seen {
		if authorization != "PVEAPIToken=root@pam!terraform=stored-token-secret" {
			t.Fatalf("the stored credential was not used for its own cluster: %q", authorization)
		}
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
