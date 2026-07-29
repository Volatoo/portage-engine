package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/internal/binpkg"
	artifactstorage "github.com/slchris/portage-engine/internal/storage"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestObjectBinhostGatewayFollowsValidatedChannel(t *testing.T) {
	root := t.TempDir()
	store, err := artifactstorage.NewLocalStorage(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	const (
		binhostPath = "releases/amd64/binpackages/23.0/x86-64"
		relative    = "app-misc/jq/jq-1.8.2.gpkg.tar"
	)
	generationID := uuid.NewString()
	payload := []byte("published package")
	local := filepath.Join(root, "jq.gpkg.tar")
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	artifactKey, _ := artifactstorage.PublishedGenerationKey(
		binhostPath, "amd64", generationID, relative,
	)
	if err := store.Upload(local, artifactKey); err != nil {
		t.Fatal(err)
	}
	packages := []byte(
		"ARCH: amd64\nTIMESTAMP: 1\nVERSION: 0\nPACKAGES: 1\n\n" +
			"CPV: app-misc/jq-1.8.2\nSIZE: 17\nPATH: " + relative + "\n\n",
	)
	packagesKey, _ := artifactstorage.PublishedGenerationKey(
		binhostPath, "amd64", generationID, "Packages",
	)
	if err := artifactstorage.UploadBytes(store, packagesKey, packages, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	manifest := artifactstorage.GenerationManifest{
		SchemaVersion: artifactstorage.GenerationManifestSchema,
		GenerationID:  generationID, BinhostPath: binhostPath,
		Architecture: "amd64", ProfileID: "pe/base",
		AttemptID: uuid.NewString(), SigningKeyID: "TEST",
		PackagesSHA256: digestDocument(packages),
		Provenance: artifactstorage.GenerationProvenance{
			PackageAtom:      "app-misc/jq-1.8.2",
			BuildInputSHA256: strings.Repeat("a", 64),
			BuildMode:        "native-gentoo",
		},
		Artifacts: []artifactstorage.GenerationArtifact{{
			RelativePath: relative, SHA256: digestDocument(payload),
			Size: int64(len(payload)), ObjectKey: artifactKey,
		}},
		CreatedAt: time.Now().UTC(),
	}
	manifestDocument, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	manifestKey, _ := artifactstorage.PublishedGenerationKey(
		binhostPath, "amd64", generationID, artifactstorage.GenerationManifestName,
	)
	if err := artifactstorage.UploadBytes(
		store, manifestKey, manifestDocument, t.TempDir(),
	); err != nil {
		t.Fatal(err)
	}
	pointer := artifactstorage.ChannelPointer{
		SchemaVersion: artifactstorage.ChannelPointerSchema,
		Channel:       "stable", BinhostPath: binhostPath, Architecture: "amd64",
		GenerationID: generationID, ManifestKey: manifestKey,
		ManifestSHA256: digestDocument(manifestDocument),
		PackagesSHA256: manifest.PackagesSHA256,
		SelectedAt:     time.Now().UTC(),
	}
	pointerDocument, _ := pointer.Marshal()
	channelKey, _ := artifactstorage.StableChannelKey(binhostPath, "amd64")
	if err := artifactstorage.UploadBytes(
		store, channelKey, pointerDocument, t.TempDir(),
	); err != nil {
		t.Fatal(err)
	}

	server := New(&config.ServerConfig{
		StorageType: "s3", DataDir: filepath.Join(root, "server"),
		BinpkgPath: filepath.Join(root, "cache"), MaxWorkers: 0,
	})
	defer server.Shutdown()
	server.artifactStorage = store
	profile := binhostProfile{
		ID: "pe/base", Arch: "amd64", BinhostPath: binhostPath,
	}
	server.binhostProfiles[profile.ID] = profile
	server.binpkgStores[binhostPath] = binpkg.NewStore(filepath.Join(root, "index"))

	handler := server.objectBinhostHandler()
	request := httptest.NewRequest(
		http.MethodGet, "/binpkgs/"+binhostPath+"/"+relative, nil,
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != string(payload) ||
		response.Header().Get("ETag") != `"`+digestDocument(payload)+`"` {
		t.Fatalf("artifact response status=%d body=%q headers=%v",
			response.Code, response.Body.String(), response.Header())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/binpkgs/"+binhostPath+"/Packages", nil,
	))
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "app-misc/jq-1.8.2") {
		t.Fatalf("Packages response status=%d body=%q",
			response.Code, response.Body.String())
	}
	if n, err := server.refreshObjectBinhostIndex(
		server.binpkgStores[binhostPath], profile,
	); err != nil || n != 1 ||
		len(server.binpkgStores[binhostPath].Snapshot()) != 1 {
		t.Fatalf("search projection n=%d err=%v snapshot=%v",
			n, err, server.binpkgStores[binhostPath].Snapshot())
	}
}
