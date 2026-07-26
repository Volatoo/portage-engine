package builder

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestMirrorUploaderDoesNotReplayCredentialsAcrossRedirects(t *testing.T) {
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalled = true
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	uploader := newMirrorUploader(&config.CloudSettings{
		UploadURL:      origin.URL,
		UploadUser:     "operator",
		UploadPassword: "must-not-be-replayed",
	})
	if uploader == nil {
		t.Fatal("configured mirror uploader is nil")
	}
	if err := uploader.login(); err == nil {
		t.Fatal("mirror login redirect was accepted")
	}
	if targetCalled {
		t.Fatal("mirror credentials were replayed to the redirect target")
	}
}

func TestMirrorUploadKeepsArtifactAndIndexInProfileNamespace(t *testing.T) {
	var uploaded []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			w.WriteHeader(http.StatusOK)
		case "/api/artifacts":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			files := r.MultipartForm.File["file"]
			if len(files) != 1 {
				http.Error(w, "one file required", http.StatusBadRequest)
				return
			}
			uploaded = append(uploaded, r.FormValue("directory")+"/"+files[0].Filename)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"artifact": map[string]string{"url": "http://mirror.test/" + files[0].Filename},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer target.Close()

	root := t.TempDir()
	const namespace = "releases/amd64/binpackages/23.0/x86-64_pe-base"
	rel := namespace + "/app-misc/jq/jq-1.8.1-1.gpkg.tar"
	artifact := filepath.Join(root, filepath.FromSlash(rel))
	index := filepath.Join(root, filepath.FromSlash(namespace), "Packages")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("signed"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, []byte("PACKAGES: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(&config.ServerConfig{MaxWorkers: 0, BinpkgPath: root})
	defer manager.Shutdown()
	manager.jobsMu.Lock()
	manager.jobs["job-1"] = &BuildStatus{
		JobID: "job-1", Arch: "amd64", ArtifactPath: artifact,
		ArtifactURL: "/binpkgs/" + rel,
		ResolvedContext: &catalog.ResolvedBuildContext{
			Arch: "amd64", ProfileID: "pe/base", ProfileChannel: "stable",
			BinhostPath: namespace,
		},
	}
	manager.jobsMu.Unlock()

	uploader := newMirrorUploader(&config.CloudSettings{
		UploadURL: target.URL, UploadDir: "portage-engine",
	})
	if err := manager.uploadJobToMirror("job-1", uploader); err != nil {
		t.Fatal(err)
	}
	wantArtifact := "portage-engine/" + rel
	wantIndex := "portage-engine/" + namespace + "/Packages"
	if !slices.Contains(uploaded, wantArtifact) || !slices.Contains(uploaded, wantIndex) {
		t.Fatalf("namespaced mirror uploads = %#v, want %q and %q", uploaded, wantArtifact, wantIndex)
	}
}
