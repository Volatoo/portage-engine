package builder

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slchris/portage-engine/pkg/config"
)

// In compatibility mode the quarantine lives on the volume the isolated signer
// also mounts, so a compromised signer can create directory entries inside a
// live token root. The capability must keep serving quarantine bytes and
// nothing else, however those entries are pointed.
func TestQuarantineCapabilityRefusesAPlantedSymlink(t *testing.T) {
	parent := t.TempDir()
	publicRoot := filepath.Join(parent, "binpkgs")
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BinpkgPath: publicRoot})
	defer mgr.Shutdown()
	settings := *mgr.CloudSettings()
	settings.ServerCallbackURL = "http://control-plane.test"
	mgr.UpdateCloudSettings(&settings)

	mgr.jobsMu.Lock()
	mgr.jobs["planted-job"] = &BuildStatus{JobID: "planted-job", Arch: "amd64"}
	mgr.jobsMu.Unlock()
	root, err := mgr.beginArtifactQuarantine("planted-job")
	if err != nil {
		t.Fatal(err)
	}
	const rel = "app-misc/jq/jq-1.8.1-1.gpkg.tar"
	file := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("unsigned bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr.jobsMu.Lock()
	mgr.jobs["planted-job"].StagedArtifacts = []string{rel}
	mgr.jobsMu.Unlock()
	if _, err := mgr.prepareVerificationBinhost("planted-job", "amd64"); err != nil {
		t.Fatal(err)
	}
	mgr.jobsMu.RLock()
	token := mgr.jobs["planted-job"].VerificationToken
	mgr.jobsMu.RUnlock()

	// The signed tree is what this quarantine exists to keep artifacts out of.
	signed := filepath.Join(parent, "signed-tree", "app-misc", "jq")
	if err := os.MkdirAll(signed, 0o750); err != nil {
		t.Fatal(err)
	}
	const secretBody = "signed tree bytes"
	if err := os.WriteFile(filepath.Join(signed, "jq-1.8.1-1.gpkg.tar"), []byte(secretBody), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory component that leaves the token root.
	if err := os.Symlink(filepath.Join(parent, "signed-tree"), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	// The same redirect one level down, where the cleaned request path looks
	// exactly like an ordinary category/package/file artifact request.
	if err := os.Symlink(filepath.Join(parent, "signed-tree", "app-misc"), filepath.Join(root, "app-misc", "elsewhere")); err != nil {
		t.Fatal(err)
	}

	for _, requested := range []string{
		"escape/app-misc/jq/jq-1.8.1-1.gpkg.tar",
		"app-misc/elsewhere/jq/jq-1.8.1-1.gpkg.tar",
	} {
		request := httptest.NewRequest(http.MethodGet, "/verify-binhost/"+token+"/"+requested, nil)
		response := httptest.NewRecorder()
		mgr.ServeVerificationBinhost(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("planted symlink %q was served: status=%d", requested, response.Code)
		}
		if strings.Contains(response.Body.String(), secretBody) {
			t.Fatalf("planted symlink %q leaked bytes from outside the quarantine", requested)
		}
	}

	// Real quarantined artifacts must still be reachable through the same
	// capability: this closes an escape, it does not close the binhost.
	request := httptest.NewRequest(http.MethodGet, "/verify-binhost/"+token+"/"+rel, nil)
	response := httptest.NewRecorder()
	mgr.ServeVerificationBinhost(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "unsigned bytes" {
		t.Fatalf("quarantined artifact stopped serving: status=%d body=%q",
			response.Code, response.Body.String())
	}
	index := httptest.NewRequest(http.MethodGet, "/verify-binhost/"+token+"/Packages", nil)
	response = httptest.NewRecorder()
	mgr.ServeVerificationBinhost(response, index)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "app-misc/jq") {
		t.Fatalf("quarantine Packages index stopped serving: status=%d", response.Code)
	}
}
