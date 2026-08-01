package storage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/slchris/portage-engine/internal/metrics"
)

// ErrObjectNotFound reports that a key a caller was told about is gone. Age
// sweeps run against a listing another replica is free to delete from, so
// "vanished" must be distinguishable from "the backend could not answer".
var ErrObjectNotFound = errors.New("storage object not found")

// recordWrite counts every object write attempt that reached the backend, and
// its failures. The severity=critical storage-error alert and the storage
// panels read these two counters, so the backends themselves must feed them:
// no HTTP handler sees an artifact upload made by a phase executor. Callers
// defer this only after the remote path has validated, so a rejected path --
// which never touched the backend -- is neither a write nor an error. A key a
// concurrent replica already removed is a write, but not a fault.
func recordWrite(err error) {
	registry := metrics.Default()
	registry.IncStorageWrites()
	if err != nil && !errors.Is(err, ErrObjectNotFound) {
		registry.IncStorageErrors()
	}
}

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

// TimestampedStorage reports the backend's own write time for an object. It is
// deliberately optional: age is only ever used to authorize deletion, so a
// backend that cannot answer must leave objects alone rather than have callers
// invent a timestamp.
type TimestampedStorage interface {
	Storage
	ObjectModified(remotePath string) (time.Time, error)
}

// ObjectInfo is one listed object together with the backend's own write time.
type ObjectInfo struct {
	Key      string
	Modified time.Time
}

// ListedStorage carries write times in the listing itself. Every S3 list page
// already reports LastModified for each key, so a backend that implements this
// answers "how old is this prefix?" in one paginated call instead of one
// HeadObject per object on every sweep.
type ListedStorage interface {
	Storage
	ListModified(prefix string) ([]ObjectInfo, error)
}

// NewestObjectTime returns the most recent backend write time under prefix.
// known is false when the backend cannot report object times, and callers that
// delete on age must then keep the objects.
func NewestObjectTime(store Storage, prefix string) (time.Time, bool, error) {
	if listed, ok := store.(ListedStorage); ok {
		objects, err := listed.ListModified(prefix)
		if err != nil {
			return time.Time{}, false, err
		}
		newest := newestModified(objects)
		return newest, !newest.IsZero(), nil
	}
	timestamped, ok := store.(TimestampedStorage)
	if !ok {
		return time.Time{}, false, nil
	}
	keys, err := store.List(prefix)
	if err != nil {
		return time.Time{}, false, err
	}
	var newest time.Time
	for _, key := range keys {
		modified, err := timestamped.ObjectModified(key)
		if errors.Is(err, ErrObjectNotFound) || errors.Is(err, fs.ErrNotExist) {
			// A replica finishing its own cleanup removed the key between the
			// listing and this read. It is no longer part of the generation and
			// cannot make it younger, so abandoning the whole sweep for it would
			// leave real orphans alive for as long as any concurrent deletion
			// keeps overlapping the scan.
			continue
		}
		if err != nil {
			return time.Time{}, false, err
		}
		if modified.After(newest) {
			newest = modified
		}
	}
	return newest, !newest.IsZero(), nil
}

func newestModified(objects []ObjectInfo) time.Time {
	var newest time.Time
	for _, object := range objects {
		if object.Modified.After(newest) {
			newest = object.Modified
		}
	}
	return newest
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
