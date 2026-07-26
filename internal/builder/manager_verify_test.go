package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/slchris/portage-engine/pkg/config"
)

func TestSignedVerificationFailsClosedWithoutPublicKey(t *testing.T) {
	manager := NewManager(&config.ServerConfig{MaxWorkers: 0})
	defer manager.Shutdown()
	root := t.TempDir()
	const rel = "app-misc/jq/jq-1.8.2.gpkg.tar"
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("signed bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager.jobsMu.Lock()
	manager.jobs["signed-no-key"] = &BuildStatus{
		JobID: "signed-no-key", Status: "signing", Signed: true,
		StagingRoot: root, StagedArtifacts: []string{rel},
	}
	manager.jobsMu.Unlock()

	var called atomic.Bool
	builderServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called.Store(true)
	}))
	defer builderServer.Close()
	err := manager.verifyOnBuilder("signed-no-key", builderServer.URL, "builder-1",
		&BuildRequest{PackageName: "app-misc/jq"}, "http://control/verify-binhost/token")
	if err == nil || !strings.Contains(err.Error(), "refusing unsigned downgrade") {
		t.Fatalf("verifyOnBuilder error = %v, want fail-closed public-key error", err)
	}
	if called.Load() {
		t.Fatal("builder was called after signed verification lost its public key")
	}
}

func TestSignedVerificationRequestBindsGenerationDigestAndKey(t *testing.T) {
	manager := NewManager(&config.ServerConfig{MaxWorkers: 0})
	defer manager.Shutdown()
	root := t.TempDir()
	const rel = "app-misc/jq/jq-1.8.2.gpkg.tar"
	payload := []byte("exact signer output")
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	manager.jobsMu.Lock()
	manager.jobs["signed-bound"] = &BuildStatus{
		JobID: "signed-bound", Status: "signing", Signed: true,
		StagingRoot: root, StagedArtifacts: []string{rel},
	}
	manager.jobsMu.Unlock()
	manager.SetGPGKeyProvider(func() (string, []byte, []byte) {
		return "0123456789ABCDEF", []byte("public key"), nil
	})

	var captured VerifyInstallRequest
	builderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "log": "signed install ok"})
	}))
	defer builderServer.Close()
	if err := manager.verifyOnBuilder("signed-bound", builderServer.URL, "builder-1",
		&BuildRequest{PackageName: "app-misc/jq"}, "http://control/verify-binhost/token"); err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256(payload)
	if captured.Generation != "signed" || !captured.RequireSignature ||
		captured.ExpectedKeyID != "0123456789ABCDEF" || captured.GPGPubkey != "public key" {
		t.Fatalf("signed policy was not preserved: %#v", captured)
	}
	if len(captured.Artifacts) != 1 ||
		captured.Artifacts[0].RelativePath != rel ||
		captured.Artifacts[0].Size != int64(len(payload)) ||
		captured.Artifacts[0].SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("signed artifact proof mismatch: %#v", captured.Artifacts)
	}
}

func TestUnsignedNegativeControlMustBeRejected(t *testing.T) {
	manager := NewManager(&config.ServerConfig{MaxWorkers: 0})
	defer manager.Shutdown()
	root := t.TempDir()
	const rel = "app-misc/jq/jq-1.8.2.gpkg.tar"
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unsigned bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager.jobsMu.Lock()
	manager.jobs["unsigned-negative"] = &BuildStatus{
		JobID: "unsigned-negative", Status: "verifying", Signed: false,
		StagingRoot: root, StagedArtifacts: []string{rel},
	}
	manager.jobsMu.Unlock()
	manager.SetGPGKeyProvider(func() (string, []byte, []byte) {
		return "0123456789ABCDEF", []byte("public key"), nil
	})

	var captured VerifyInstallRequest
	builderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": false, "error": "unsigned GPKG Manifest signature is missing",
		})
	}))
	defer builderServer.Close()
	if err := manager.verifyUnsignedRejectedOnBuilder("unsigned-negative", builderServer.URL,
		&BuildRequest{PackageName: "app-misc/jq"}, "http://control/verify-binhost/token"); err != nil {
		t.Fatal(err)
	}
	if captured.Generation != "signed" || !captured.RequireSignature || len(captured.Artifacts) != 1 {
		t.Fatalf("negative-control contract is not signed and digest-bound: %#v", captured)
	}

	acceptingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer acceptingServer.Close()
	err := manager.verifyUnsignedRejectedOnBuilder("unsigned-negative", acceptingServer.URL,
		&BuildRequest{PackageName: "app-misc/jq"}, "http://control/verify-binhost/token")
	if err == nil || !strings.Contains(err.Error(), "unsigned GPKG was accepted") {
		t.Fatalf("accepting negative-control error = %v", err)
	}
}
