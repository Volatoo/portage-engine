package storage

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestS3StorageIntegration exercises conditional immutable writes against a
// real S3-compatible endpoint. It is opt-in so the normal unit suite remains
// hermetic:
//
//	PORTAGE_S3_INTEGRATION=1 \
//	STORAGE_S3_ENDPOINT=http://127.0.0.1:29000 \
//	STORAGE_S3_BUCKET=portage-engine-artifacts \
//	STORAGE_S3_REGION=us-east-1 \
//	AWS_ACCESS_KEY_ID=portage-minio-local \
//	AWS_SECRET_ACCESS_KEY=portage-minio-secret-local \
//	go test ./internal/storage -run TestS3StorageIntegration -v
func TestS3StorageIntegration(t *testing.T) {
	store := integrationS3Store(t)
	const key = "app-misc/jq/jq-1.8.2.gpkg.tar"
	source, payload := integrationFixture(t, "jq-1.8.2.gpkg.tar", "signed-binpkg-integration")
	if err := store.Upload(source, key); err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Delete(key); err != nil {
			t.Logf("cleanup object: %v", err)
		}
	})
	assertS3ImmutableRetry(t, store, key, source)
	assertS3RoundTrip(t, store, key, payload)
	assertS3QuarantineCapabilityLifecycle(t, store)
	assertS3ChannelCompareAndSwap(t, store)
	assertS3PublicationLifecycle(t, store)
}

func assertS3PublicationLifecycle(t *testing.T, source *S3Storage) {
	t.Helper()
	const (
		binhostPath = "releases/amd64/binpackages/23.0/x86-64"
		arch        = "amd64"
	)
	now := time.Now().UTC()
	activeID := uuid.NewString()
	garbageID := uuid.NewString()
	activeManifest, activeDocument := seedTestGeneration(
		t, source, activeID, now.Add(-time.Hour),
	)
	seedTestGeneration(
		t, source, garbageID, now.Add(-72*time.Hour),
	)
	manifestKey, _ := PublishedGenerationKey(
		binhostPath, arch, activeID, GenerationManifestName,
	)
	pointer := ChannelPointer{
		SchemaVersion: ChannelPointerSchema, Channel: "stable",
		BinhostPath: binhostPath, Architecture: arch,
		GenerationID: activeID, ManifestKey: manifestKey,
		ManifestSHA256: digestDocumentBytes(activeDocument),
		PackagesSHA256: activeManifest.PackagesSHA256,
		SelectedAt:     now,
	}
	pointerDocument, _ := pointer.Marshal()
	channelKey, _ := StableChannelKey(binhostPath, arch)
	if _, err := CompareAndSwapBytes(
		source, channelKey, pointerDocument, "", t.TempDir(),
	); err != nil {
		t.Fatal(err)
	}
	destination := &S3Storage{
		client: source.client, bucket: source.bucket,
		prefix: source.prefix + "replica/", allowDelete: true,
		requestTimeout: source.requestTimeout,
	}
	if _, err := AuditPublishedChannel(
		source, binhostPath, arch, t.TempDir(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplicatePublishedChannel(
		source, destination, binhostPath, arch, t.TempDir(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := AuditPublishedChannel(
		destination, binhostPath, arch, t.TempDir(),
	); err != nil {
		t.Fatal(err)
	}
	deleted, err := GarbageCollectPublishedGenerations(
		source, binhostPath, arch, 24*time.Hour, now, t.TempDir(),
	)
	if err != nil || len(deleted) != 1 || deleted[0] != garbageID {
		t.Fatalf("real S3 GC deleted=%v err=%v", deleted, err)
	}
	for _, target := range []*S3Storage{source, destination} {
		keys, _ := target.List("")
		for _, key := range keys {
			_ = target.Delete(key)
		}
	}
}

func assertS3ChannelCompareAndSwap(t *testing.T, store *S3Storage) {
	t.Helper()
	const key = "_integration-cas/stable.json"
	first, _ := integrationFixture(t, "first.json", `{"generation":"first"}`)
	version, err := store.CompareAndSwap(first, key, "")
	if err != nil || version == "" {
		t.Fatalf("create channel pointer version=%q err=%v", version, err)
	}
	t.Cleanup(func() { _ = store.Delete(key) })
	read := filepath.Join(t.TempDir(), "stable.json")
	readVersion, err := store.DownloadVersion(key, read)
	if err != nil || readVersion != version {
		t.Fatalf("read channel version=%q want=%q err=%v", readVersion, version, err)
	}
	second, _ := integrationFixture(t, "second.json", `{"generation":"second"}`)
	nextVersion, err := store.CompareAndSwap(second, key, version)
	if err != nil || nextVersion == "" || nextVersion == version {
		t.Fatalf("update channel version=%q old=%q err=%v", nextVersion, version, err)
	}
	if _, err := store.CompareAndSwap(first, key, version); !errors.Is(err, ErrCompareAndSwapConflict) {
		t.Fatalf("stale channel update error=%v", err)
	}
}

func assertS3QuarantineCapabilityLifecycle(t *testing.T, store *S3Storage) {
	t.Helper()
	token := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	artifactKey, err := QuarantineGenerationKey(
		token, "app-misc/jq/jq-1.8.2.gpkg.tar",
	)
	if err != nil {
		t.Fatal(err)
	}
	source, payload := integrationFixture(
		t, "quarantine.gpkg.tar", "quarantine-generation",
	)
	if err := store.Upload(source, artifactKey); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	manifest := QuarantineManifest{
		SchemaVersion:  QuarantineManifestSchema,
		Token:          token,
		Generation:     "unsigned",
		Architecture:   "amd64",
		PackagesSHA256: digest,
		Artifacts: []GenerationArtifact{{
			RelativePath: "app-misc/jq/jq-1.8.2.gpkg.tar",
			SHA256:       digest,
			Size:         int64(len(payload)),
		}},
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	document, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	capabilityKey, err := QuarantineCapabilityKey(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := UploadBytes(store, capabilityKey, document, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	downloaded, err := DownloadBytes(store, capabilityKey, t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseQuarantineManifest(downloaded, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(capabilityKey); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists(capabilityKey); err != nil || exists {
		t.Fatalf("revoked capability exists=%v err=%v", exists, err)
	}
	if exists, err := store.Exists(artifactKey); err != nil || !exists {
		t.Fatalf("capability revocation removed immutable bytes: exists=%v err=%v", exists, err)
	}
	if err := store.Delete(artifactKey); err != nil {
		t.Logf("cleanup quarantine artifact: %v", err)
	}
}

func integrationS3Store(t *testing.T) *S3Storage {
	t.Helper()
	if os.Getenv("PORTAGE_S3_INTEGRATION") != "1" {
		t.Skip("set PORTAGE_S3_INTEGRATION=1 to run the real S3 Gate")
	}
	endpoint := os.Getenv("STORAGE_S3_ENDPOINT")
	bucket := os.Getenv("STORAGE_S3_BUCKET")
	region := os.Getenv("STORAGE_S3_REGION")
	if endpoint == "" || bucket == "" || region == "" {
		t.Fatal("STORAGE_S3_ENDPOINT, STORAGE_S3_BUCKET, and STORAGE_S3_REGION are required")
	}
	prefix := "integration/" + time.Now().UTC().Format("20060102T150405.000000000Z") + "/"
	store, err := NewS3StorageWithConfig(S3Config{
		Bucket:         bucket,
		Region:         region,
		Prefix:         prefix,
		Endpoint:       endpoint,
		UsePathStyle:   true,
		AllowDelete:    true,
		RequestTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func integrationFixture(t *testing.T, name, content string) (string, []byte) {
	t.Helper()
	source := filepath.Join(t.TempDir(), name)
	payload := []byte(content)
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return source, payload
}

func assertS3ImmutableRetry(t *testing.T, store *S3Storage, key, source string) {
	t.Helper()
	if err := store.Upload(source, key); err != nil {
		t.Fatalf("idempotent upload: %v", err)
	}

	replacement, _ := integrationFixture(t, "replacement.gpkg.tar", "different")
	if err := store.Upload(replacement, key); !errors.Is(err, ErrObjectConflict) {
		t.Fatalf("replacement error = %v, want ErrObjectConflict", err)
	}
}

func assertS3RoundTrip(t *testing.T, store *S3Storage, key string, payload []byte) {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "downloaded.gpkg.tar")
	if err := store.Download(key, destination); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded bytes = %q, want %q", got, payload)
	}
	files, err := store.List("app-misc/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 1 || files[0] != key {
		t.Fatalf("listed objects = %v, want [%s]", files, key)
	}
}
