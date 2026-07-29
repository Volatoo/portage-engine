package builder

import (
	"fmt"
	"log"

	"github.com/slchris/portage-engine/internal/storage"
)

// StorageUploader handles uploading build artifacts to storage.
type StorageUploader struct {
	storage storage.Storage
	enabled bool
}

// NewStorageUploader creates a new storage uploader.
func NewStorageUploader(storageType, _ /* localDir */, s3Bucket, s3Region, s3Prefix, httpBase string) (*StorageUploader, error) {
	return NewStorageUploaderWithConfig(storage.Config{
		Type:     storageType,
		S3Bucket: s3Bucket,
		S3Region: s3Region,
		S3Prefix: s3Prefix,
		HTTPBase: httpBase,
	})
}

// NewStorageUploaderWithConfig creates an uploader with the full backend
// configuration, including S3-compatible endpoints.
func NewStorageUploaderWithConfig(cfg storage.Config) (*StorageUploader, error) {
	storageType := cfg.Type
	if storageType == "" || storageType == "local" {
		// Local storage - no upload needed
		return &StorageUploader{
			enabled: false,
		}, nil
	}

	var st storage.Storage
	var err error

	switch storageType {
	case "s3":
		if cfg.S3Bucket == "" {
			return nil, fmt.Errorf("S3 bucket not configured")
		}
		st, err = storage.NewStorage(&cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create S3 storage: %w", err)
		}
	case "http":
		if cfg.HTTPBase == "" {
			return nil, fmt.Errorf("HTTP base URL not configured")
		}
		st, err = storage.NewStorage(&cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP storage: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported storage type: %s", storageType)
	}

	return &StorageUploader{
		storage: st,
		enabled: true,
	}, nil
}

// Upload uploads an artifact to storage.
func (u *StorageUploader) Upload(localPath, remotePath string) error {
	if !u.enabled {
		log.Printf("Storage upload disabled, keeping local file: %s", localPath)
		return nil
	}

	if u.storage == nil {
		return fmt.Errorf("storage not initialized")
	}

	log.Printf("Uploading %s to %s", localPath, remotePath)
	if err := u.storage.Upload(localPath, remotePath); err != nil {
		return fmt.Errorf("failed to upload: %w", err)
	}

	log.Printf("Upload complete: %s", remotePath)
	return nil
}

// GetURL returns the URL for an artifact.
func (u *StorageUploader) GetURL(remotePath string) (string, error) {
	if !u.enabled || u.storage == nil {
		return remotePath, nil
	}

	return u.storage.GetURL(remotePath)
}

// IsEnabled returns whether storage upload is enabled.
func (u *StorageUploader) IsEnabled() bool {
	return u.enabled
}
