package builder

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	artifactstorage "github.com/slchris/portage-engine/internal/storage"
	"github.com/slchris/portage-engine/pkg/config"
)

type versionedMemoryObject struct {
	body    []byte
	version string
}

type versionedMemoryStorage struct {
	mu      sync.Mutex
	objects map[string]versionedMemoryObject
	next    int
}

func newVersionedMemoryStorage() *versionedMemoryStorage {
	return &versionedMemoryStorage{objects: make(map[string]versionedMemoryObject)}
}

func (s *versionedMemoryStorage) Upload(localPath, remotePath string) error {
	document, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.objects[remotePath]; exists {
		if string(current.body) == string(document) {
			return nil
		}
		return artifactstorage.ErrObjectConflict
	}
	s.next++
	s.objects[remotePath] = versionedMemoryObject{
		body: append([]byte(nil), document...), version: fmt.Sprintf("v%d", s.next),
	}
	return nil
}

func (s *versionedMemoryStorage) Download(remotePath, localPath string) error {
	_, err := s.DownloadVersion(remotePath, localPath)
	return err
}

func (s *versionedMemoryStorage) DownloadVersion(remotePath, localPath string) (string, error) {
	s.mu.Lock()
	object, exists := s.objects[remotePath]
	s.mu.Unlock()
	if !exists {
		return "", os.ErrNotExist
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o750); err != nil {
		return "", err
	}
	return object.version, os.WriteFile(localPath, object.body, 0o600)
}

func (s *versionedMemoryStorage) CompareAndSwap(
	localPath, remotePath, expectedVersion string,
) (string, error) {
	document, err := os.ReadFile(localPath)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.objects[remotePath]
	if (!exists && expectedVersion != "") ||
		(exists && current.version != expectedVersion) {
		return "", artifactstorage.ErrCompareAndSwapConflict
	}
	if exists && expectedVersion == "" {
		return "", artifactstorage.ErrCompareAndSwapConflict
	}
	s.next++
	version := fmt.Sprintf("v%d", s.next)
	s.objects[remotePath] = versionedMemoryObject{
		body: append([]byte(nil), document...), version: version,
	}
	return version, nil
}

func (s *versionedMemoryStorage) Delete(remotePath string) error {
	s.mu.Lock()
	delete(s.objects, remotePath)
	s.mu.Unlock()
	return nil
}

func (s *versionedMemoryStorage) List(prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *versionedMemoryStorage) GetURL(remotePath string) (string, error) {
	return "memory://" + remotePath, nil
}

func (s *versionedMemoryStorage) Exists(remotePath string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.objects[remotePath]
	return exists, nil
}

func TestObjectPublicationMergesGenerationAndFencesChannel(t *testing.T) {
	store := newVersionedMemoryStorage()
	manager := NewManager(&config.ServerConfig{
		MaxWorkers: 0, StorageType: "s3",
		BinpkgPath: filepath.Join(t.TempDir(), "binpkgs"),
		DataDir:    filepath.Join(t.TempDir(), "data"),
	})
	manager.SetArtifactStorage(store)
	defer manager.Shutdown()
	const (
		binhostPath = "releases/amd64/binpackages/23.0/x86-64"
		arch        = "amd64"
	)
	first := publishObjectFixture(
		t, manager, store, strings.Repeat("a", 32),
		"app-misc/jq/jq-1.8.2.gpkg.tar", "app-misc/jq-1.8.2",
	)
	second := publishObjectFixture(
		t, manager, store, strings.Repeat("b", 32),
		"app-editors/vim/vim-9.1.gpkg.tar", "app-editors/vim-9.1",
	)
	if first.GenerationID == second.GenerationID ||
		second.PreviousGenerationID != first.GenerationID {
		t.Fatalf("unexpected channel lineage: first=%#v second=%#v", first, second)
	}
	manifestDocument, err := artifactstorage.DownloadBytes(
		store, second.ManifestKey, t.TempDir(), 4<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := artifactstorage.ParseGenerationManifest(manifestDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Artifacts) != 2 {
		t.Fatalf("generation lost a prior artifact: %#v", manifest.Artifacts)
	}
	packagesKey, _ := artifactstorage.PublishedGenerationKey(
		binhostPath, arch, second.GenerationID, "Packages",
	)
	packages, err := artifactstorage.DownloadBytes(
		store, packagesKey, t.TempDir(), 64<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(packages), "PACKAGES: 2") ||
		!strings.Contains(string(packages), "app-misc/jq-1.8.2") ||
		!strings.Contains(string(packages), "app-editors/vim-9.1") {
		t.Fatalf("merged Packages is incomplete:\n%s", packages)
	}
}

func publishObjectFixture(
	t *testing.T,
	manager *Manager,
	store artifactstorage.Storage,
	token, relative, cpv string,
) artifactstorage.ChannelPointer {
	t.Helper()
	root := t.TempDir()
	payload := []byte("package:" + cpv)
	local := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(local), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	artifactKey, _ := artifactstorage.QuarantineGenerationKey(token, relative)
	if err := store.Upload(local, artifactKey); err != nil {
		t.Fatal(err)
	}
	packages := []byte(fmt.Sprintf(
		"ARCH: amd64\nTIMESTAMP: 1\nVERSION: 0\nPACKAGES: 1\n\n"+
			"CPV: %s\nSIZE: %d\nPATH: %s\n\n",
		cpv, len(payload), relative,
	))
	packagesKey, _ := artifactstorage.QuarantineGenerationKey(token, "Packages")
	if err := artifactstorage.UploadBytes(store, packagesKey, packages, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	digest, size, err := digestRegularFileAt(root, relative)
	if err != nil {
		t.Fatal(err)
	}
	capability := artifactstorage.QuarantineManifest{
		SchemaVersion: artifactstorage.QuarantineManifestSchema,
		Token:         token, Generation: "unsigned", Architecture: "amd64",
		PackagesSHA256: digestBytes(packages),
		Artifacts: []artifactstorage.GenerationArtifact{{
			RelativePath: relative, SHA256: digest, Size: size,
		}},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	document, _ := capability.Marshal()
	capabilityKey, _ := artifactstorage.QuarantineCapabilityKey(token)
	if err := artifactstorage.UploadBytes(store, capabilityKey, document, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	status := &BuildStatus{
		JobID: uuid.NewString(), AttemptID: uuid.NewString(),
		CreatedAt: time.Now().UTC(), Arch: "amd64",
	}
	paths, err := manager.publishObjectGeneration(
		status, root, token, []string{relative}, "amd64",
		"releases/amd64/binpackages/23.0/x86-64",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] == "" {
		t.Fatalf("publication paths=%v", paths)
	}
	channelKey, _ := artifactstorage.StableChannelKey(
		"releases/amd64/binpackages/23.0/x86-64", "amd64",
	)
	channelDocument, err := artifactstorage.DownloadBytes(
		store, channelKey, t.TempDir(), 1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	pointer, err := artifactstorage.ParseChannelPointer(channelDocument)
	if err != nil {
		t.Fatal(err)
	}
	return pointer
}

var _ artifactstorage.VersionedStorage = (*versionedMemoryStorage)(nil)
