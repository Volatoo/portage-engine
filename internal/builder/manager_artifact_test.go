package builder

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/slchris/portage-engine/internal/binpkg"
	artifactstorage "github.com/slchris/portage-engine/internal/storage"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestCollectionFenceRejectsDownloadedOutputBeforeStagingCommit(t *testing.T) {
	parent := t.TempDir()
	mgr := NewManager(&config.ServerConfig{
		MaxWorkers: 0, BinpkgPath: filepath.Join(parent, "binpkgs"),
	})
	defer mgr.Shutdown()
	mgr.jobs["job-fenced"] = &BuildStatus{
		JobID: "job-fenced", Status: "collecting",
		PackageName: "app-misc/jq", Arch: "amd64",
	}
	const rel = "app-misc/jq/jq-1.8.1-1.gpkg.tar"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("remote-derived bytes"))
	}))
	defer server.Close()
	calls := 0
	err := mgr.collectInstanceArtifacts(
		"job-fenced", server.URL, "remote-a", "app-misc/jq",
		&remoteJobSnapshot{Artifacts: []string{rel}},
		func() error {
			calls++
			return errors.New("lease fenced after collection")
		},
	)
	if err == nil || calls != 1 {
		t.Fatalf("post-collection fence err=%v calls=%d", err, calls)
	}
	mgr.jobsMu.RLock()
	status := *mgr.jobs["job-fenced"]
	mgr.jobsMu.RUnlock()
	if status.StagingRoot != "" || status.VerificationToken != "" ||
		len(status.StagedArtifacts) != 0 || status.StagedPrimary != "" {
		t.Fatalf("fenced output advanced into staging: %+v", status)
	}
}

func TestCopyArtifactWithLimitRejectsUnknownLengthOverflow(t *testing.T) {
	var destination bytes.Buffer
	written, err := copyArtifactWithLimit(
		&destination, strings.NewReader("123456"), 5,
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds remaining") {
		t.Fatalf("overflow written=%d err=%v", written, err)
	}
	if written != 6 {
		t.Fatalf("limited stream read %d bytes, want sentinel byte 6", written)
	}
	if err := validateArtifactContentLength(6, 5); err == nil {
		t.Fatal("declared oversized artifact was accepted")
	}
}

func TestArtifactRelCPV(t *testing.T) {
	tests := map[string]string{
		"app-misc/jq/jq-1.8.2-1.gpkg.tar":                "app-misc/jq-1.8.2",
		"sys-apps/portage/portage-3.0.67-r1-42.gpkg.tar": "sys-apps/portage-3.0.67-r1",
		"dev-libs/oniguruma-6.9.10.tbz2":                 "dev-libs/oniguruma-6.9.10",
		"jq-1.8.2-1.gpkg.tar":                            "",
		"app-misc/jq/jq-1.8.2-unsigned.gpkg.tar":         "app-misc/jq-1.8.2-unsigned",
		"../../app-misc/jq/jq-1.8.2-1.gpkg.tar":          "",
	}
	for rel, want := range tests {
		if got := artifactRelCPV(rel); got != want {
			t.Errorf("artifactRelCPV(%q) = %q, want %q", rel, got, want)
		}
	}
}

// TestFetchArtifactToBinhost verifies that a completed remote build's artifact
// is downloaded into the binhost PKGDIR under its category (previously only a
// path reference on the soon-destroyed VM was recorded, losing the artifact),
// and that the stored hook fires so the Packages index refreshes.
func TestFetchArtifactToBinhost(t *testing.T) {
	artifact := []byte("fake gpkg bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/artifacts/download/rjob-1" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="jq-1.7-1.gpkg.tar"`)
		_, _ = w.Write(artifact)
	}))
	defer srv.Close()

	binhost := t.TempDir()
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 1, BinpkgPath: binhost})
	defer mgr.Shutdown()

	var hookCalled atomic.Bool
	mgr.SetArtifactStoredHook(func() { hookCalled.Store(true) })

	dest, webPath, err := mgr.fetchArtifactToBinhost(srv.URL, "rjob-1", "app-misc/jq", "/var/tmp/portage-artifacts/jq-1.7-1.gpkg.tar")
	if err != nil {
		t.Fatalf("fetchArtifactToBinhost: %v", err)
	}

	want := filepath.Join(binhost, "app-misc", "jq-1.7-1.gpkg.tar")
	if dest != want {
		t.Errorf("dest = %q, want %q", dest, want)
	}
	if webPath != "/binpkgs/app-misc/jq-1.7-1.gpkg.tar" {
		t.Errorf("webPath = %q", webPath)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("artifact not stored: %v", err)
	}
	if string(data) != string(artifact) {
		t.Error("stored artifact content mismatch")
	}
	if !hookCalled.Load() {
		t.Error("artifact-stored hook was not called")
	}
}

// TestArtifactFilename covers header parsing and the fallback path.
func TestArtifactFilename(t *testing.T) {
	cases := []struct {
		disposition, remote, want string
	}{
		{`attachment; filename="jq-1.7-1.gpkg.tar"`, "", "jq-1.7-1.gpkg.tar"},
		{"", "/var/tmp/artifacts/vim-9.0-1.gpkg.tar", "vim-9.0-1.gpkg.tar"},
		{`attachment; filename="../../etc/passwd"`, "", "passwd"}, // path stripped
		{`attachment; filename=".hidden"`, "/x/fallback.tar", "fallback.tar"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := artifactFilename(c.disposition, c.remote); got != c.want {
			t.Errorf("artifactFilename(%q, %q) = %q, want %q", c.disposition, c.remote, got, c.want)
		}
	}
}

func TestVerifyAndPublishKeepsArtifactPrivateUntilVerification(t *testing.T) {
	parent := t.TempDir()
	publicRoot := filepath.Join(parent, "binpkgs")
	const binhostPath = "releases/amd64/binpackages/23.0/x86-64"
	store := binpkg.NewStore(filepath.Join(publicRoot, filepath.FromSlash(binhostPath)))
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BinpkgPath: publicRoot})
	defer mgr.Shutdown()
	mgr.SetArtifactPromotionHook(func(root string, rels []string, arch, gotPath string) ([]string, error) {
		if gotPath != binhostPath {
			t.Fatalf("promotion binhost path = %q", gotPath)
		}
		return store.PromoteStaged(root, rels, arch)
	})

	mgr.jobsMu.Lock()
	mgr.jobs["job-quarantine"] = &BuildStatus{
		JobID:       "job-quarantine",
		Status:      "collecting",
		PackageName: "app-misc/jq",
		Arch:        "amd64",
	}
	mgr.jobsMu.Unlock()

	verificationBinhost := httptest.NewServer(http.HandlerFunc(mgr.ServeVerificationBinhost))
	defer verificationBinhost.Close()
	settings := *mgr.CloudSettings()
	settings.ServerCallbackURL = verificationBinhost.URL
	mgr.UpdateCloudSettings(&settings)

	const rel = "app-misc/jq/jq-1.8.1-1.gpkg.tar"
	var verifiedIndex, verifiedArtifact atomic.Bool
	builderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/artifacts/download/"):
			_, _ = w.Write([]byte("quarantined package bytes"))
		case r.URL.Path == "/api/v1/verify":
			var request struct {
				BinhostURL string `json:"binhost_url"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if resp, err := http.Get(request.BinhostURL + "/Packages"); err == nil {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				verifiedIndex.Store(resp.StatusCode == http.StatusOK && strings.Contains(string(body), "app-misc/jq-1.8.1"))
			}
			if resp, err := http.Get(request.BinhostURL + "/" + rel); err == nil {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				verifiedArtifact.Store(resp.StatusCode == http.StatusOK && string(body) == "quarantined package bytes")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": verifiedIndex.Load() && verifiedArtifact.Load()})
		default:
			http.NotFound(w, r)
		}
	}))
	defer builderServer.Close()

	snap := &remoteJobSnapshot{Artifacts: []string{rel}}
	if err := mgr.collectInstanceArtifacts(
		"job-quarantine", builderServer.URL, "remote-1", "app-misc/jq", snap, nil,
	); err != nil {
		t.Fatalf("collectInstanceArtifacts: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.BasePath(), filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Fatalf("artifact became public before verification: %v", err)
	}
	status, err := mgr.GetStatus("job-quarantine")
	if err != nil {
		t.Fatal(err)
	}
	if status.ArtifactURL != "" || len(status.Artifacts) != 0 {
		t.Fatalf("quarantined artifact leaked through job API: %#v", status)
	}

	if stage, err := mgr.verifyAndPublish("job-quarantine", builderServer.URL, "static-1", &BuildRequest{
		PackageName: "app-misc/jq",
		Arch:        "amd64",
	}); err != nil {
		t.Fatalf("verifyAndPublish failed at %s: %v", stage, err)
	}
	if !verifiedIndex.Load() || !verifiedArtifact.Load() {
		t.Fatal("builder did not verify both quarantine index and artifact")
	}
	if _, err := os.Stat(filepath.Join(store.BasePath(), filepath.FromSlash(rel))); err != nil {
		t.Fatalf("verified artifact was not promoted: %v", err)
	}
	status, err = mgr.GetStatus("job-quarantine")
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "completed" || status.ArtifactURL != "/binpkgs/"+binhostPath+"/"+rel {
		t.Fatalf("unexpected published job state: %#v", status)
	}
}

func TestVerifyFailureRevokesQuarantineWithoutPublishing(t *testing.T) {
	parent := t.TempDir()
	publicRoot := filepath.Join(parent, "binpkgs")
	const binhostPath = "releases/amd64/binpackages/23.0/x86-64"
	store := binpkg.NewStore(filepath.Join(publicRoot, filepath.FromSlash(binhostPath)))
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BinpkgPath: publicRoot})
	defer mgr.Shutdown()
	mgr.SetArtifactPromotionHook(func(root string, rels []string, arch, gotPath string) ([]string, error) {
		if gotPath != binhostPath {
			t.Fatalf("promotion binhost path = %q", gotPath)
		}
		return store.PromoteStaged(root, rels, arch)
	})

	mgr.jobsMu.Lock()
	mgr.jobs["job-rejected"] = &BuildStatus{
		JobID:       "job-rejected",
		Status:      "collecting",
		PackageName: "app-misc/jq",
		Arch:        "amd64",
	}
	mgr.jobsMu.Unlock()

	verificationBinhost := httptest.NewServer(http.HandlerFunc(mgr.ServeVerificationBinhost))
	defer verificationBinhost.Close()
	settings := *mgr.CloudSettings()
	settings.ServerCallbackURL = verificationBinhost.URL
	mgr.UpdateCloudSettings(&settings)

	const rel = "app-misc/jq/jq-1.8.1-1.gpkg.tar"
	builderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/artifacts/download/") {
			_, _ = w.Write([]byte("broken package"))
			return
		}
		if r.URL.Path == "/api/v1/verify" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "install rejected"})
			return
		}
		http.NotFound(w, r)
	}))
	defer builderServer.Close()

	if err := mgr.collectInstanceArtifacts("job-rejected", builderServer.URL, "remote-2", "app-misc/jq", &remoteJobSnapshot{
		Artifacts: []string{rel},
	}, nil); err != nil {
		t.Fatal(err)
	}
	mgr.jobsMu.RLock()
	token := mgr.jobs["job-rejected"].VerificationToken
	mgr.jobsMu.RUnlock()

	if stage, err := mgr.verifyAndPublish("job-rejected", builderServer.URL, "static-2", &BuildRequest{
		PackageName: "app-misc/jq",
		Arch:        "amd64",
	}); err == nil || stage != "verify" {
		t.Fatalf("expected verify failure, got stage=%q err=%v", stage, err)
	}
	if _, err := os.Stat(filepath.Join(publicRoot, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Fatalf("failed artifact was published: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/verify-binhost/"+token+"/Packages", nil)
	response := httptest.NewRecorder()
	mgr.ServeVerificationBinhost(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("revoked quarantine remained accessible: status=%d", response.Code)
	}
}

func TestVerificationQuarantineIsServedByAnotherReplica(t *testing.T) {
	parent := t.TempDir()
	publicRoot := filepath.Join(parent, "binpkgs")
	owner := NewManager(&config.ServerConfig{MaxWorkers: 0, BinpkgPath: publicRoot})
	replica := NewManager(&config.ServerConfig{MaxWorkers: 0, BinpkgPath: publicRoot})
	defer owner.Shutdown()
	defer replica.Shutdown()
	settings := *owner.CloudSettings()
	settings.ServerCallbackURL = "http://control-plane.test"
	owner.UpdateCloudSettings(&settings)

	owner.jobsMu.Lock()
	owner.jobs["shared-job"] = &BuildStatus{JobID: "shared-job", Arch: "amd64"}
	owner.jobsMu.Unlock()
	root, err := owner.beginArtifactQuarantine("shared-job")
	if err != nil {
		t.Fatal(err)
	}
	const rel = "app-misc/jq/jq-1.8.1-1.gpkg.tar"
	file := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("shared bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner.jobsMu.Lock()
	owner.jobs["shared-job"].StagedArtifacts = []string{rel}
	owner.jobsMu.Unlock()
	if _, err := owner.prepareVerificationBinhost("shared-job", "amd64"); err != nil {
		t.Fatal(err)
	}
	owner.jobsMu.RLock()
	token := owner.jobs["shared-job"].VerificationToken
	owner.jobsMu.RUnlock()

	request := httptest.NewRequest(http.MethodGet, "/verify-binhost/"+token+"/"+rel, nil)
	response := httptest.NewRecorder()
	replica.ServeVerificationBinhost(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "shared bytes" {
		t.Fatalf("replica response: status=%d body=%q", response.Code, response.Body.String())
	}

	owner.cleanupArtifactQuarantine("shared-job")
	response = httptest.NewRecorder()
	replica.ServeVerificationBinhost(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("revoked shared capability status=%d", response.Code)
	}
}

func TestObjectVerificationQuarantineNeedsNoSharedFilesystem(t *testing.T) {
	parent := t.TempDir()
	objectStore, err := artifactstorage.NewLocalStorage(filepath.Join(parent, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	owner := NewManager(&config.ServerConfig{
		MaxWorkers: 0, BinpkgPath: filepath.Join(parent, "owner-binpkgs"),
		DataDir: filepath.Join(parent, "owner"), StorageType: "s3",
	})
	replica := NewManager(&config.ServerConfig{
		MaxWorkers: 0, BinpkgPath: filepath.Join(parent, "replica-binpkgs"),
		DataDir: filepath.Join(parent, "replica"), StorageType: "s3",
	})
	owner.SetArtifactStorage(objectStore)
	replica.SetArtifactStorage(objectStore)
	defer owner.Shutdown()
	defer replica.Shutdown()
	settings := *owner.CloudSettings()
	settings.ServerCallbackURL = "http://control-plane.test"
	owner.UpdateCloudSettings(&settings)

	owner.jobsMu.Lock()
	owner.jobs["object-job"] = &BuildStatus{JobID: "object-job", Arch: "amd64"}
	owner.jobsMu.Unlock()
	root, err := owner.beginArtifactQuarantine("object-job")
	if err != nil {
		t.Fatal(err)
	}
	const rel = "app-misc/jq/jq-1.8.1-1.gpkg.tar"
	file := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("object-only bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner.jobsMu.Lock()
	owner.jobs["object-job"].StagedArtifacts = []string{rel}
	owner.jobsMu.Unlock()
	if err := owner.persistJobQuarantine("object-job", root, []string{rel}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.prepareVerificationBinhost("object-job", "amd64"); err != nil {
		t.Fatal(err)
	}
	owner.jobsMu.RLock()
	token := owner.jobs["object-job"].VerificationToken
	owner.jobsMu.RUnlock()

	request := httptest.NewRequest(http.MethodGet, "/verify-binhost/"+token+"/"+rel, nil)
	response := httptest.NewRecorder()
	replica.ServeVerificationBinhost(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "object-only bytes" {
		t.Fatalf("replica response: status=%d body=%q", response.Code, response.Body.String())
	}
	unlisted := httptest.NewRequest(
		http.MethodGet, "/verify-binhost/"+token+"/private/other.gpkg.tar", nil,
	)
	response = httptest.NewRecorder()
	replica.ServeVerificationBinhost(response, unlisted)
	if response.Code != http.StatusNotFound {
		t.Fatalf("capability exposed an unlisted object: status=%d", response.Code)
	}

	owner.cleanupArtifactQuarantine("object-job")
	response = httptest.NewRecorder()
	replica.ServeVerificationBinhost(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("revoked object capability status=%d", response.Code)
	}
}

// TestPromotionSurvivesAProjectionFailureAfterPublication covers the window
// after the packages are already public: persistJobSnapshot revalidates the
// worker lease, which FinalizePhaseWork does not, so a lease that lapsed
// during a long publish makes the projection write lose while the publication
// itself was perfectly fenced. Turning that into a promotion error sends the
// caller to failActivePhase and marks a job failed whose packages the world
// can install.
func TestPromotionSurvivesAProjectionFailureAfterPublication(t *testing.T) {
	publicRoot := filepath.Join(t.TempDir(), "binpkgs")
	const (
		binhostPath = "releases/amd64/binpackages/23.0/x86-64"
		rel         = "app-misc/jq/jq-1.8.2.gpkg.tar"
	)
	store := binpkg.NewStore(filepath.Join(publicRoot, filepath.FromSlash(binhostPath)))
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BinpkgPath: publicRoot})
	defer mgr.Shutdown()
	mgr.SetArtifactPromotionHook(func(root string, rels []string, arch, _ string) ([]string, error) {
		return store.PromoteStaged(root, rels, arch)
	})
	mgr.scheduler = &serializationFailureScheduler{
		verdict: errors.New("job publish-projection ledger state is failed, expected publishing"),
	}

	staging := t.TempDir()
	local := filepath.Join(staging, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(local), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("verified package bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr.jobsMu.Lock()
	mgr.jobs["publish-projection"] = &BuildStatus{
		JobID: "publish-projection", Status: "publishing", Arch: "amd64",
		AttemptID: "22222222-2222-4222-8222-222222222222", FenceToken: 5,
		StagingRoot: staging, StagedArtifacts: []string{rel}, StagedPrimary: rel,
	}
	mgr.jobsMu.Unlock()

	if err := mgr.promoteJobArtifacts("publish-projection"); err != nil {
		t.Fatalf("a rejected projection write failed an already published job: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.BasePath(), filepath.FromSlash(rel))); err != nil {
		t.Fatalf("promotion did not publish the artifact: %v", err)
	}
	status, err := mgr.GetStatus("publish-projection")
	if err != nil {
		t.Fatal(err)
	}
	if status.ArtifactURL != "/binpkgs/"+binhostPath+"/"+rel {
		t.Fatalf("published job lost its artifact locations: %#v", status)
	}
	if !strings.Contains(status.Log, "artifacts are public but their locations were not projected") {
		t.Fatalf("the unprojected publication was not reported:\n%s", status.Log)
	}
}
