package storage

import (
	"os"
	"path/filepath"
	"testing"
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
