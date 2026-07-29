package storage

import (
	"fmt"
	"os"
	"path/filepath"
)

// UploadBytes uses the backend's ordinary immutable upload contract without
// requiring callers to invent persistent hand-off files.
func UploadBytes(store Storage, remotePath string, document []byte, scratchDir string) error {
	if store == nil {
		return fmt.Errorf("object storage is required")
	}
	if scratchDir == "" {
		scratchDir = os.TempDir()
	}
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(scratchDir, ".portage-object-upload-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(document); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return store.Upload(name, remotePath)
}

// DownloadBytes atomically downloads a bounded object into scratch and reads
// it after the backend's integrity verification succeeds.
func DownloadBytes(
	store Storage, remotePath, scratchDir string, maxBytes int64,
) ([]byte, error) {
	if store == nil || maxBytes <= 0 {
		return nil, fmt.Errorf("object storage and a positive byte limit are required")
	}
	if scratchDir == "" {
		scratchDir = os.TempDir()
	}
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp(scratchDir, ".portage-object-download-*")
	if err != nil {
		return nil, err
	}
	name := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	defer func() { _ = os.Remove(name) }()
	if err := store.Download(remotePath, name); err != nil {
		return nil, err
	}
	info, err := os.Stat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, fmt.Errorf("downloaded object exceeds its bounded contract")
	}
	return os.ReadFile(filepath.Clean(name))
}

// DownloadVersionBytes is DownloadBytes plus the backend fencing token used
// for a subsequent channel compare-and-swap.
func DownloadVersionBytes(
	store VersionedStorage, remotePath, scratchDir string, maxBytes int64,
) ([]byte, string, error) {
	if store == nil || maxBytes <= 0 {
		return nil, "", fmt.Errorf("versioned storage and a positive byte limit are required")
	}
	if scratchDir == "" {
		scratchDir = os.TempDir()
	}
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		return nil, "", err
	}
	temp, err := os.CreateTemp(scratchDir, ".portage-version-download-*")
	if err != nil {
		return nil, "", err
	}
	name := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return nil, "", err
	}
	defer func() { _ = os.Remove(name) }()
	version, err := store.DownloadVersion(remotePath, name)
	if err != nil {
		return nil, "", err
	}
	info, err := os.Stat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, "", fmt.Errorf("downloaded versioned object exceeds its bounded contract")
	}
	document, err := os.ReadFile(filepath.Clean(name))
	return document, version, err
}

// CompareAndSwapBytes applies the versioned pointer contract without exposing
// caller-owned persistent files.
func CompareAndSwapBytes(
	store VersionedStorage,
	remotePath string,
	document []byte,
	expectedVersion, scratchDir string,
) (string, error) {
	if store == nil {
		return "", fmt.Errorf("versioned storage is required")
	}
	if scratchDir == "" {
		scratchDir = os.TempDir()
	}
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(scratchDir, ".portage-cas-upload-*")
	if err != nil {
		return "", err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return "", err
	}
	if _, err := temp.Write(document); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	return store.CompareAndSwap(name, remotePath, expectedVersion)
}
