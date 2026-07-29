package storage

import (
	"fmt"
	"io"
	"time"
)

// Storage defines the interface for package storage backends
type Storage interface {
	Upload(localPath, remotePath string) error
	Download(remotePath, localPath string) error
	Delete(remotePath string) error
	List(prefix string) ([]string, error)
	GetURL(remotePath string) (string, error)
	Exists(remotePath string) (bool, error)
}

// VersionedStorage adds the compare-and-swap primitive used only for small
// mutable channel pointers. Package bytes and generation manifests continue
// to use Storage.Upload's create-only immutable contract.
type VersionedStorage interface {
	Storage
	DownloadVersion(remotePath, localPath string) (string, error)
	CompareAndSwap(localPath, remotePath, expectedVersion string) (string, error)
}

// Config represents storage configuration
type Config struct {
	Type            string
	LocalDir        string
	S3Bucket        string
	S3Region        string
	S3Prefix        string
	S3Endpoint      string
	S3UsePathStyle  bool
	S3PublicBaseURL string
	S3AllowDelete   bool
	RequestTimeout  time.Duration
	HTTPBase        string
	Options         map[string]string
}

// NewStorage creates a storage backend based on config
func NewStorage(cfg *Config) (Storage, error) {
	switch cfg.Type {
	case "local":
		return NewLocalStorage(cfg.LocalDir)
	case "s3":
		return NewS3StorageWithConfig(S3Config{
			Bucket:         cfg.S3Bucket,
			Region:         cfg.S3Region,
			Prefix:         cfg.S3Prefix,
			Endpoint:       cfg.S3Endpoint,
			UsePathStyle:   cfg.S3UsePathStyle,
			PublicBaseURL:  cfg.S3PublicBaseURL,
			AllowDelete:    cfg.S3AllowDelete,
			RequestTimeout: cfg.RequestTimeout,
		})
	case "http":
		return NewHTTPStorage(cfg.HTTPBase)
	default:
		return nil, fmt.Errorf("unsupported storage type: %s", cfg.Type)
	}
}

// CopyStream copies data with progress callback
func CopyStream(dst io.Writer, src io.Reader, callback func(written int64)) error {
	buf := make([]byte, 32*1024)
	var written int64

	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw > 0 {
				written += int64(nw)
				if callback != nil {
					callback(written)
				}
			}
			if ew != nil {
				return ew
			}
			if nr != nw {
				return io.ErrShortWrite
			}
		}
		if er != nil {
			if er != io.EOF {
				return er
			}
			break
		}
	}
	return nil
}
