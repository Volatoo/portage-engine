package storage

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LocalStorage implements Storage interface for local filesystem.
type LocalStorage struct {
	baseDir string
}

// NewLocalStorage creates a new local storage backend.
func NewLocalStorage(baseDir string) (*LocalStorage, error) {
	if err := os.MkdirAll(baseDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %w", err)
	}

	return &LocalStorage{baseDir: baseDir}, nil
}

// Upload uploads a file to local storage.
func (ls *LocalStorage) Upload(localPath, remotePath string) error {
	name, err := cleanRemotePath(remotePath)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(ls.baseDir)
	if err != nil {
		return fmt.Errorf("failed to open storage root: %w", err)
	}
	defer func() { _ = root.Close() }()
	destDir := filepath.Dir(name)

	if err := root.MkdirAll(destDir, 0750); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	src, err := os.Open(localPath) // #nosec G304 -- localPath is the caller-selected upload source.
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer func() { _ = src.Close() }()

	dst, err := root.Create(name)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer func() { _ = dst.Close() }()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	return nil
}

// Download downloads a file from local storage.
func (ls *LocalStorage) Download(remotePath, localPath string) error {
	name, err := cleanRemotePath(remotePath)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(ls.baseDir)
	if err != nil {
		return fmt.Errorf("failed to open storage root: %w", err)
	}
	defer func() { _ = root.Close() }()

	src, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer func() { _ = src.Close() }()

	localDir := filepath.Dir(localPath)
	if err := os.MkdirAll(localDir, 0750); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	dst, err := os.Create(localPath) // #nosec G304 -- localPath is the caller-selected download destination.
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer func() { _ = dst.Close() }()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	return nil
}

// Delete removes a file from local storage.
func (ls *LocalStorage) Delete(remotePath string) error {
	name, err := cleanRemotePath(remotePath)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(ls.baseDir)
	if err != nil {
		return fmt.Errorf("failed to open storage root: %w", err)
	}
	defer func() { _ = root.Close() }()
	return root.Remove(name)
}

// List lists files in local storage.
func (ls *LocalStorage) List(prefix string) ([]string, error) {
	var files []string
	name, err := cleanRemotePath(prefix)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(ls.baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open storage root: %w", err)
	}
	defer func() { _ = root.Close() }()

	err = fs.WalkDir(root.FS(), filepath.ToSlash(name), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() {
			files = append(files, filepath.FromSlash(path))
		}
		return nil
	})

	return files, err
}

// GetURL returns the URL for a file.
func (ls *LocalStorage) GetURL(remotePath string) (string, error) {
	name, err := cleanRemotePath(remotePath)
	if err != nil {
		return "", err
	}
	return "file://" + filepath.Join(ls.baseDir, name), nil
}

// Exists checks if a file exists.
func (ls *LocalStorage) Exists(remotePath string) (bool, error) {
	name, err := cleanRemotePath(remotePath)
	if err != nil {
		return false, err
	}
	root, err := os.OpenRoot(ls.baseDir)
	if err != nil {
		return false, fmt.Errorf("failed to open storage root: %w", err)
	}
	defer func() { _ = root.Close() }()
	_, err = root.Stat(name)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func cleanRemotePath(remotePath string) (string, error) {
	if filepath.IsAbs(remotePath) {
		return "", fmt.Errorf("remote path must be relative")
	}
	name := filepath.Clean(remotePath)
	slashName := filepath.ToSlash(name)
	if slashName == ".." || strings.HasPrefix(slashName, "../") || !fs.ValidPath(slashName) {
		return "", fmt.Errorf("remote path escapes storage root")
	}
	return name, nil
}
