package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/metrics"
)

// TestNewLocalStorage tests creating new local storage.
func TestNewLocalStorage(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	storage, err := NewLocalStorage(tmpDir)
	if err != nil {
		t.Fatalf("NewLocalStorage failed: %v", err)
	}
	if storage == nil {
		t.Fatal("NewLocalStorage returned nil")
	}
}

// TestUpload tests uploading a file.
func TestUpload(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	storage, err := NewLocalStorage(tmpDir)
	if err != nil {
		t.Fatalf("NewLocalStorage failed: %v", err)
	}

	// Create a test file
	srcFile := filepath.Join(tmpDir, "test-source.txt")
	testData := []byte("test content")
	if err := os.WriteFile(srcFile, testData, 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Upload the file
	destPath := "test-dest.txt"
	err = storage.Upload(srcFile, destPath)
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	// Verify the file exists
	uploadedPath := filepath.Join(tmpDir, destPath)
	if _, err := os.Stat(uploadedPath); os.IsNotExist(err) {
		t.Error("Uploaded file does not exist")
	}

	// Verify content
	content, err := os.ReadFile(uploadedPath)
	if err != nil {
		t.Fatalf("Failed to read uploaded file: %v", err)
	}

	if string(content) != string(testData) {
		t.Errorf("Content mismatch: expected %s, got %s", testData, content)
	}
}

// TestDownload tests downloading a file.
func TestDownload(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	storage, err := NewLocalStorage(tmpDir)
	if err != nil {
		t.Fatalf("NewLocalStorage failed: %v", err)
	}

	// Create a test file
	srcPath := "test-source.txt"
	srcFile := filepath.Join(tmpDir, srcPath)
	testData := []byte("test content")
	if err := os.WriteFile(srcFile, testData, 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Download the file
	destFile := filepath.Join(tmpDir, "downloaded.txt")
	err = storage.Download(srcPath, destFile)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	// Verify content
	content, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("Failed to read downloaded file: %v", err)
	}

	if string(content) != string(testData) {
		t.Errorf("Content mismatch: expected %s, got %s", testData, content)
	}
}

// TestDelete tests deleting a file.
func TestDelete(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	storage, err := NewLocalStorage(tmpDir)
	if err != nil {
		t.Fatalf("NewLocalStorage failed: %v", err)
	}

	// Create a test file
	testPath := "test-file.txt"
	testFile := filepath.Join(tmpDir, testPath)
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Delete the file
	err = storage.Delete(testPath)
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify file is deleted
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Error("File should be deleted")
	}
}

// TestList tests listing files.
func TestList(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	storage, err := NewLocalStorage(tmpDir)
	if err != nil {
		t.Fatalf("NewLocalStorage failed: %v", err)
	}

	// Create test files
	testFiles := []string{"file1.txt", "file2.txt", "file3.txt"}
	for _, file := range testFiles {
		filePath := filepath.Join(tmpDir, file)
		if err := os.WriteFile(filePath, []byte("test"), 0644); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}
	}

	// List files
	files, err := storage.List("")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(files) != len(testFiles) {
		t.Errorf("Expected %d files, got %d", len(testFiles), len(files))
	}
}

// TestObjectWritesFeedTheStorageCounters pins the storage counters to the
// backend rather than to an HTTP handler: an artifact written by a phase
// executor never passes through one.
func TestObjectWritesFeedTheStorageCounters(t *testing.T) {
	registry := metrics.New(&metrics.Config{Enabled: true})
	before := registry.GetSnapshot()

	store, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "package.gpkg.tar")
	if err := os.WriteFile(source, []byte("package"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Upload(source, "app-misc/package.gpkg.tar"); err != nil {
		t.Fatal(err)
	}
	// A path the validator refuses never reaches the backend, so it is neither
	// a write nor a backend error. Counting it as both put a rejected argument
	// on the same critical alert as a failing disk.
	if err := store.Upload(source, "../escape.gpkg.tar"); err == nil {
		t.Fatal("path escape was accepted")
	}
	// The signer's quarantine sweep lists then deletes, so a key a concurrent
	// replica removed first is expected. It is a write attempt, not a fault.
	if err := store.Delete("app-misc/vanished.gpkg.tar"); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("delete of a missing key returned %v, want ErrObjectNotFound", err)
	}

	after := registry.GetSnapshot()
	writes := after["storage_writes"].(int64) - before["storage_writes"].(int64)
	errorCount := after["storage_errors"].(int64) - before["storage_errors"].(int64)
	if writes != 2 || errorCount != 0 {
		t.Fatalf("storage writes=%d errors=%d, want writes=2 errors=0", writes, errorCount)
	}
}

func TestObjectModifiedReportsBackendWriteTime(t *testing.T) {
	store, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "package.gpkg.tar")
	if err := os.WriteFile(source, []byte("package"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Upload(source, "app-misc/package.gpkg.tar"); err != nil {
		t.Fatal(err)
	}
	modified, err := store.ObjectModified("app-misc/package.gpkg.tar")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(modified) > time.Minute || time.Since(modified) < 0 {
		t.Fatalf("object modification time = %s", modified)
	}
	if _, err := store.ObjectModified("app-misc/missing.gpkg.tar"); err == nil {
		t.Fatal("a missing object reported a modification time")
	}
}

func TestLocalStorageConfinesRemotePaths(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "storage")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(parent, "source")
	if err := os.WriteFile(source, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage, err := NewLocalStorage(base)
	if err != nil {
		t.Fatal(err)
	}

	if err := storage.Upload(source, "../escaped"); err == nil {
		t.Fatal("Upload accepted a path outside the storage root")
	}
	if _, err := storage.GetURL("../escaped"); err == nil {
		t.Fatal("GetURL accepted a path outside the storage root")
	}

	if err := os.Symlink("../outside", filepath.Join(base, "escape-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := storage.Download("escape-link/secret", filepath.Join(parent, "copy")); err == nil {
		t.Fatal("Download followed a symlink outside the storage root")
	}
}

// headCountingStorage reports how often an age sweep falls back to per-object
// reads. A backend whose listing already carries write times must never take
// that path: the orphan sweep runs every five minutes over every object of
// every quarantined generation.
type headCountingStorage struct {
	*LocalStorage
	heads int
}

func (s *headCountingStorage) ObjectModified(remotePath string) (time.Time, error) {
	s.heads++
	return s.LocalStorage.ObjectModified(remotePath)
}

func TestNewestObjectTimeReadsWriteTimesFromTheListing(t *testing.T) {
	local, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &headCountingStorage{LocalStorage: local}
	source := filepath.Join(t.TempDir(), "package.gpkg.tar")
	if err := os.WriteFile(source, []byte("package"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"gen/a.gpkg.tar", "gen/b.gpkg.tar", "gen/Packages"} {
		if err := store.Upload(source, key); err != nil {
			t.Fatal(err)
		}
	}
	newest, known, err := NewestObjectTime(store, "gen")
	if err != nil || !known {
		t.Fatalf("newest=%s known=%v err=%v", newest, known, err)
	}
	if time.Since(newest) > time.Minute || time.Since(newest) < 0 {
		t.Fatalf("newest object time = %s", newest)
	}
	if store.heads != 0 {
		t.Fatalf("the listing was re-read one object at a time: %d per-object reads", store.heads)
	}
}

// perObjectStore is a backend whose listing carries no write times, so every
// key costs its own read and a concurrent replica can delete one mid-sweep.
type perObjectStore struct {
	times map[string]time.Time
	gone  map[string]bool
	fail  map[string]bool
}

func (*perObjectStore) Upload(string, string) error       { return nil }
func (*perObjectStore) Download(string, string) error     { return nil }
func (*perObjectStore) Delete(string) error               { return nil }
func (*perObjectStore) GetURL(string) (string, error)     { return "", nil }
func (s *perObjectStore) Exists(key string) (bool, error) { _, ok := s.times[key]; return ok, nil }

func (s *perObjectStore) List(prefix string) ([]string, error) {
	keys := make([]string, 0, len(s.times))
	for key := range s.times {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *perObjectStore) ObjectModified(key string) (time.Time, error) {
	if s.fail[key] {
		return time.Time{}, errors.New("backend unavailable")
	}
	if s.gone[key] {
		return time.Time{}, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
	}
	return s.times[key], nil
}

func TestNewestObjectTimeSurvivesAKeyDeletedMidScan(t *testing.T) {
	now := time.Now()
	store := &perObjectStore{
		times: map[string]time.Time{
			"gen/a.gpkg.tar": now.Add(-3 * time.Hour),
			"gen/b.gpkg.tar": now.Add(-time.Hour),
			"gen/Packages":   now.Add(-2 * time.Hour),
		},
		gone: map[string]bool{"gen/b.gpkg.tar": true},
	}
	newest, known, err := NewestObjectTime(store, "gen")
	if err != nil || !known {
		t.Fatalf("a concurrent deletion skipped the whole generation: known=%v err=%v", known, err)
	}
	if !newest.Equal(store.times["gen/Packages"]) {
		t.Fatalf("newest object time = %s, want the newest surviving key", newest)
	}

	// A backend that cannot answer is not the same as a key that is gone: the
	// sweep must not age a generation on an incomplete reading.
	store.gone = nil
	store.fail = map[string]bool{"gen/b.gpkg.tar": true}
	if _, known, err := NewestObjectTime(store, "gen"); known || err == nil {
		t.Fatalf("an unreadable object was treated as absent: known=%v err=%v", known, err)
	}
}
