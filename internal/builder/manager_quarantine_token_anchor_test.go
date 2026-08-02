package builder

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/pkg/config"
)

// The capability's confinement has to survive the token directory itself being
// replaced, not just entries planted inside it. In compatibility mode the
// quarantine base lives on the volume the isolated signer mounts read-write, so
// whoever can plant a symlink under a live token root can equally unlink that
// root and leave a symlink of the same name in its place. The marker is no
// obstacle either: both the control plane and the signer write it, so the
// attacker can put a valid one wherever the replaced root points. Anchoring the
// root on the token directory would resolve that symlink before any confinement
// existed and hand out an arbitrary read; anchoring on the base and addressing
// the token beneath it resolves the token under the same protection as every
// path below it.
func TestQuarantineCapabilityRefusesASymlinkedTokenDirectory(t *testing.T) {
	const rel = "app-misc/jq/jq-1.8.1-1.gpkg.tar"
	const secretBody = "signed tree bytes"

	// Whatever else shares the volume: the published binhost the quarantine
	// exists to keep unsigned artifacts out of, the server's own data
	// directory, anything the process can reach by name.
	plantDecoy := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(filepath.FromSlash(rel))), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(secretBody), 0o600); err != nil {
			t.Fatal(err)
		}
		// A capability marker the attacker is entitled to write: the signer
		// activates exactly this file on its own output roots.
		marker := strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10)
		if err := os.WriteFile(filepath.Join(dir, verificationCapabilityFile), []byte(marker), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, testCase := range []struct {
		name string
		// target returns the symlink body written in place of the token
		// directory, given the quarantine base and the decoy tree.
		target func(base, decoy string) string
	}{
		{
			name:   "absolute",
			target: func(_, decoy string) string { return decoy },
		},
		{
			name: "relative",
			target: func(base, decoy string) string {
				relative, err := filepath.Rel(base, decoy)
				if err != nil {
					panic(err)
				}
				return relative
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			parent := t.TempDir()
			publicRoot := filepath.Join(parent, "binpkgs")
			mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BinpkgPath: publicRoot})
			defer mgr.Shutdown()
			settings := *mgr.CloudSettings()
			settings.ServerCallbackURL = "http://control-plane.test"
			mgr.UpdateCloudSettings(&settings)

			mgr.jobsMu.Lock()
			mgr.jobs["swapped-job"] = &BuildStatus{JobID: "swapped-job", Arch: "amd64"}
			mgr.jobsMu.Unlock()
			root, err := mgr.beginArtifactQuarantine("swapped-job")
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("unsigned bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			mgr.jobsMu.Lock()
			mgr.jobs["swapped-job"].StagedArtifacts = []string{rel}
			mgr.jobsMu.Unlock()
			if _, err := mgr.prepareVerificationBinhost("swapped-job", "amd64"); err != nil {
				t.Fatal(err)
			}
			mgr.jobsMu.RLock()
			token := mgr.jobs["swapped-job"].VerificationToken
			mgr.jobsMu.RUnlock()

			// The real quarantine serves before the swap, so a later 404 is
			// the confinement refusing the redirect and not a broken fixture.
			request := httptest.NewRequest(http.MethodGet, "/verify-binhost/"+token+"/"+rel, nil)
			response := httptest.NewRecorder()
			mgr.ServeVerificationBinhost(response, request)
			if response.Code != http.StatusOK || response.Body.String() != "unsigned bytes" {
				t.Fatalf("quarantined artifact did not serve before the swap: status=%d body=%q",
					response.Code, response.Body.String())
			}

			decoy := filepath.Join(parent, "decoy-tree")
			plantDecoy(t, decoy)

			base := mgr.artifactQuarantineBase()
			if err := os.RemoveAll(root); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(testCase.target(base, decoy), filepath.Join(base, token)); err != nil {
				t.Fatal(err)
			}

			for _, requested := range []string{rel, "Packages"} {
				request := httptest.NewRequest(http.MethodGet, "/verify-binhost/"+token+"/"+requested, nil)
				response := httptest.NewRecorder()
				mgr.ServeVerificationBinhost(response, request)
				if response.Code != http.StatusNotFound {
					t.Fatalf("swapped token directory served %q: status=%d", requested, response.Code)
				}
				if strings.Contains(response.Body.String(), secretBody) {
					t.Fatalf("swapped token directory leaked bytes from outside the quarantine base for %q", requested)
				}
			}
		})
	}
}

// A symlinked token directory that stays inside the quarantine base is not an
// escape, and the capability must keep serving through it. Compatibility-mode
// deployments relocate a quarantine within the base while a job is live, so a
// rule that rejected every symlinked token would take the binhost down with it.
func TestQuarantineCapabilityServesATokenLinkedInsideTheBase(t *testing.T) {
	parent := t.TempDir()
	publicRoot := filepath.Join(parent, "binpkgs")
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BinpkgPath: publicRoot})
	defer mgr.Shutdown()
	settings := *mgr.CloudSettings()
	settings.ServerCallbackURL = "http://control-plane.test"
	mgr.UpdateCloudSettings(&settings)

	mgr.jobsMu.Lock()
	mgr.jobs["relocated-job"] = &BuildStatus{JobID: "relocated-job", Arch: "amd64"}
	mgr.jobsMu.Unlock()
	root, err := mgr.beginArtifactQuarantine("relocated-job")
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
	mgr.jobs["relocated-job"].StagedArtifacts = []string{rel}
	mgr.jobsMu.Unlock()
	if _, err := mgr.prepareVerificationBinhost("relocated-job", "amd64"); err != nil {
		t.Fatal(err)
	}
	mgr.jobsMu.RLock()
	token := mgr.jobs["relocated-job"].VerificationToken
	mgr.jobsMu.RUnlock()

	base := mgr.artifactQuarantineBase()
	moved := filepath.Join(base, "relocated")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("relocated", filepath.Join(base, token)); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/verify-binhost/"+token+"/"+rel, nil)
	response := httptest.NewRecorder()
	mgr.ServeVerificationBinhost(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "unsigned bytes" {
		t.Fatalf("quarantine relocated inside the base stopped serving: status=%d body=%q",
			response.Code, response.Body.String())
	}
}
