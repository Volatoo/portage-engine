package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type memoryS3Object struct {
	body     []byte
	metadata map[string]string
	etag     string
}

type memoryS3Client struct {
	mu      sync.Mutex
	objects map[string]memoryS3Object
}

func newMemoryS3Client() *memoryS3Client {
	return &memoryS3Client{objects: make(map[string]memoryS3Object)}
}

func (m *memoryS3Client) PutObject(
	_ context.Context,
	input *s3.PutObjectInput,
	_ ...func(*s3.Options),
) (*s3.PutObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := aws.ToString(input.Key)
	existing, exists := m.objects[key]
	if exists && aws.ToString(input.IfNoneMatch) == "*" {
		return nil, &smithy.GenericAPIError{
			Code:    "PreconditionFailed",
			Message: "object already exists",
		}
	}
	if expected := aws.ToString(input.IfMatch); expected != "" &&
		(!exists || existing.etag != expected) {
		return nil, &smithy.GenericAPIError{
			Code: "PreconditionFailed", Message: "etag changed",
		}
	}
	body, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	metadata := make(map[string]string, len(input.Metadata))
	for name, value := range input.Metadata {
		metadata[name] = value
	}
	digest := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(digest[:]) + `"`
	m.objects[key] = memoryS3Object{body: body, metadata: metadata, etag: etag}
	return &s3.PutObjectOutput{ETag: aws.String(etag)}, nil
}

func (m *memoryS3Client) GetObject(
	_ context.Context,
	input *s3.GetObjectInput,
	_ ...func(*s3.Options),
) (*s3.GetObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[aws.ToString(input.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing"}
	}
	return &s3.GetObjectOutput{
		Body:     io.NopCloser(bytes.NewReader(object.body)),
		Metadata: object.metadata,
		ETag:     aws.String(object.etag),
	}, nil
}

func (m *memoryS3Client) HeadObject(
	_ context.Context,
	input *s3.HeadObjectInput,
	_ ...func(*s3.Options),
) (*s3.HeadObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[aws.ToString(input.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NotFound", Message: "missing"}
	}
	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(int64(len(object.body))),
		Metadata:      object.metadata,
		ETag:          aws.String(object.etag),
	}, nil
}

func (m *memoryS3Client) ListObjectsV2(
	_ context.Context,
	input *s3.ListObjectsV2Input,
	_ ...func(*s3.Options),
) (*s3.ListObjectsV2Output, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := aws.ToString(input.Prefix)
	output := &s3.ListObjectsV2Output{}
	for key, object := range m.objects {
		if strings.HasPrefix(key, prefix) {
			output.Contents = append(output.Contents, types.Object{
				Key:  aws.String(key),
				Size: aws.Int64(int64(len(object.body))),
			})
		}
	}
	return output, nil
}

func (m *memoryS3Client) DeleteObject(
	_ context.Context,
	input *s3.DeleteObjectInput,
	_ ...func(*s3.Options),
) (*s3.DeleteObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, aws.ToString(input.Key))
	return &s3.DeleteObjectOutput{}, nil
}

// TestNewS3Storage verifies constructor validation without contacting S3.
func TestNewS3Storage(t *testing.T) {
	storage, err := NewS3Storage("test-bucket", "us-east-1", "prefix")
	if err != nil {
		t.Fatalf("NewS3Storage failed: %v", err)
	}
	if storage == nil {
		t.Fatal("expected an S3 storage instance")
	}
	if _, err := NewS3Storage("", "us-east-1", ""); err == nil {
		t.Fatal("expected empty bucket to fail")
	}
	if _, err := NewS3Storage("bucket", "", ""); err == nil {
		t.Fatal("expected empty region to fail")
	}
	if _, err := NewS3StorageWithConfig(S3Config{
		Bucket:   "bucket",
		Region:   "us-east-1",
		Endpoint: "ftp://storage.example",
	}); err == nil {
		t.Fatal("expected unsupported endpoint scheme to fail")
	}
}

// TestNewHTTPStorage verifies the HTTP constructor fails fast (backend not yet
// implemented), so a STORAGE_TYPE=http misconfiguration is caught at startup.
func TestNewHTTPStorage(t *testing.T) {
	storage, err := NewHTTPStorage("http://localhost:8080/storage")
	if err == nil {
		t.Fatal("expected an error: HTTP backend is not implemented")
	}
	if storage != nil {
		t.Error("expected nil storage on constructor failure")
	}
}

// TestStorageInterfaceCompliance tests interface compliance.
func TestStorageInterfaceCompliance(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage-interface-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	var _ Storage = &LocalStorage{}
	var _ Storage = &S3Storage{}
	var _ VersionedStorage = &S3Storage{}
	var _ Storage = &HTTPStorage{}

	// Test LocalStorage implements interface
	local, err := NewLocalStorage(tmpDir)
	if err != nil {
		t.Fatalf("NewLocalStorage failed: %v", err)
	}
	testStorageInterface(t, local, "test.txt", testFile)

	s3Storage, err := NewS3Storage("bucket", "region", "")
	if err != nil {
		t.Errorf("expected NewS3Storage to succeed: %v", err)
	}
	if s3Storage == nil {
		t.Error("expected S3 storage instance")
	}
	// HTTP remains deliberately unsupported.
	if _, err := NewHTTPStorage("http://localhost:8080"); err == nil {
		t.Error("expected NewHTTPStorage to error (not implemented)")
	}
}

func TestS3ChannelCompareAndSwap(t *testing.T) {
	client := newMemoryS3Client()
	storage := &S3Storage{
		client: client, bucket: "binpkgs", requestTimeout: time.Second,
	}
	first := filepath.Join(t.TempDir(), "first.json")
	second := filepath.Join(t.TempDir(), "second.json")
	if err := os.WriteFile(first, []byte(`{"generation":"first"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte(`{"generation":"second"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const key = "target/.channels/stable.json"
	version, err := storage.CompareAndSwap(first, key, "")
	if err != nil || version == "" {
		t.Fatalf("create pointer version=%q err=%v", version, err)
	}
	read := filepath.Join(t.TempDir(), "read.json")
	readVersion, err := storage.DownloadVersion(key, read)
	if err != nil || readVersion != version {
		t.Fatalf("read pointer version=%q want=%q err=%v", readVersion, version, err)
	}
	next, err := storage.CompareAndSwap(second, key, version)
	if err != nil || next == version {
		t.Fatalf("update pointer version=%q old=%q err=%v", next, version, err)
	}
	if _, err := storage.CompareAndSwap(first, key, version); !errors.Is(err, ErrCompareAndSwapConflict) {
		t.Fatalf("stale update error=%v", err)
	}
}

func TestS3StorageImmutableRoundTrip(t *testing.T) {
	client := newMemoryS3Client()
	storage := &S3Storage{
		client:         client,
		bucket:         "binpkgs",
		prefix:         "gentoo/releases/amd64/binpackages/23.0/x86-64/",
		requestTimeout: time.Second,
	}
	source := filepath.Join(t.TempDir(), "jq-1.8.2.gpkg.tar")
	payload := []byte("signed-binpkg")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	const remotePath = "app-misc/jq/jq-1.8.2.gpkg.tar"
	if err := storage.Upload(source, remotePath); err != nil {
		t.Fatalf("first upload failed: %v", err)
	}
	if err := storage.Upload(source, remotePath); err != nil {
		t.Fatalf("idempotent upload failed: %v", err)
	}

	replacement := filepath.Join(t.TempDir(), "replacement.gpkg.tar")
	if err := os.WriteFile(replacement, []byte("different"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := storage.Upload(replacement, remotePath); !errors.Is(err, ErrObjectConflict) {
		t.Fatalf("replacement error = %v, want ErrObjectConflict", err)
	}

	destination := filepath.Join(t.TempDir(), "downloaded.gpkg.tar")
	if err := storage.Download(remotePath, destination); err != nil {
		t.Fatalf("download failed: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("download = %q, want %q", got, payload)
	}

	exists, err := storage.Exists(remotePath)
	if err != nil || !exists {
		t.Fatalf("Exists() = %v, %v", exists, err)
	}
	files, err := storage.List("app-misc/")
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(files) != 1 || files[0] != remotePath {
		t.Fatalf("List() = %v, want [%s]", files, remotePath)
	}
	if err := storage.Delete(remotePath); !errors.Is(err, ErrDeleteDisabled) {
		t.Fatalf("Delete() = %v, want ErrDeleteDisabled", err)
	}
}

// testStorageInterface is a helper to test Storage interface methods.
func testStorageInterface(t *testing.T, s Storage, remotePath, localPath string) {
	// Test Upload
	if err := s.Upload(localPath, remotePath); err != nil {
		t.Errorf("Upload failed: %v", err)
	}

	// Test GetURL
	if _, err := s.GetURL(remotePath); err != nil {
		t.Errorf("GetURL failed: %v", err)
	}

	// Test Exists
	if exists, err := s.Exists(remotePath); err != nil || !exists {
		t.Errorf("Exists failed or returned false: %v", err)
	}

	// Test List
	if _, err := s.List(""); err != nil {
		t.Errorf("List failed: %v", err)
	}
}
