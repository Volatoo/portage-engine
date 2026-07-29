package storage

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPublicationAuditReplicationAndReferenceAwareGC(t *testing.T) {
	source := testVersionedS3Storage()
	destination := testVersionedS3Storage()
	const (
		binhostPath = "releases/amd64/binpackages/23.0/x86-64"
		arch        = "amd64"
	)
	now := time.Now().UTC()
	previousID := uuid.NewString()
	activeID := uuid.NewString()
	garbageID := uuid.NewString()
	seedTestGeneration(t, source, previousID, now.Add(-48*time.Hour))
	activeManifest, activeManifestDocument := seedTestGeneration(
		t, source, activeID, now.Add(-24*time.Hour),
	)
	seedTestGeneration(t, source, garbageID, now.Add(-72*time.Hour))
	manifestKey, _ := PublishedGenerationKey(
		binhostPath, arch, activeID, GenerationManifestName,
	)
	pointer := ChannelPointer{
		SchemaVersion: ChannelPointerSchema, Channel: "stable",
		BinhostPath: binhostPath, Architecture: arch,
		GenerationID: activeID, ManifestKey: manifestKey,
		ManifestSHA256:       digestDocumentBytes(activeManifestDocument),
		PackagesSHA256:       activeManifest.PackagesSHA256,
		PreviousGenerationID: previousID, SelectedAt: now,
	}
	pointerDocument, _ := pointer.Marshal()
	channelKey, _ := StableChannelKey(binhostPath, arch)
	if _, err := CompareAndSwapBytes(
		source, channelKey, pointerDocument, "", t.TempDir(),
	); err != nil {
		t.Fatal(err)
	}
	report, err := AuditPublishedChannel(
		source, binhostPath, arch, t.TempDir(),
	)
	if err != nil || report.Pointer.GenerationID != activeID ||
		report.ObjectCount != 3 {
		t.Fatalf("audit=%#v err=%v", report, err)
	}
	replicated, err := ReplicatePublishedChannel(
		source, destination, binhostPath, arch, t.TempDir(),
	)
	if err != nil || replicated.Pointer.GenerationID != activeID {
		t.Fatalf("replication=%#v err=%v", replicated, err)
	}
	if _, err := AuditPublishedChannel(
		destination, binhostPath, arch, t.TempDir(),
	); err != nil {
		t.Fatalf("destination audit: %v", err)
	}
	deleted, err := GarbageCollectPublishedGenerations(
		source, binhostPath, arch, 36*time.Hour, now, t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(deleted, garbageID) ||
		slices.Contains(deleted, previousID) || slices.Contains(deleted, activeID) {
		t.Fatalf("unexpected deleted generations: %v", deleted)
	}
}

func testVersionedS3Storage() *S3Storage {
	return &S3Storage{
		client: newMemoryS3Client(), bucket: "binpkgs",
		allowDelete: true, requestTimeout: time.Second,
	}
}

func seedTestGeneration(
	t *testing.T,
	store Storage,
	generationID string,
	createdAt time.Time,
) (GenerationManifest, []byte) {
	t.Helper()
	const (
		binhostPath = "releases/amd64/binpackages/23.0/x86-64"
		arch        = "amd64"
	)
	relative := "app-misc/pkg-" + generationID[:8] + ".gpkg.tar"
	payload := []byte("artifact-" + generationID)
	local := filepath.Join(t.TempDir(), "artifact.gpkg.tar")
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	artifactKey, _ := PublishedGenerationKey(
		binhostPath, arch, generationID, relative,
	)
	if err := store.Upload(local, artifactKey); err != nil {
		t.Fatal(err)
	}
	packages := []byte("VERSION: 0\nPACKAGES: 0\n\n")
	packagesKey, _ := PublishedGenerationKey(
		binhostPath, arch, generationID, "Packages",
	)
	if err := UploadBytes(store, packagesKey, packages, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	manifest := GenerationManifest{
		SchemaVersion: GenerationManifestSchema,
		GenerationID:  generationID, BinhostPath: binhostPath,
		Architecture: arch, ProfileID: "pe/test",
		AttemptID: uuid.NewString(), SigningKeyID: "TEST",
		PackagesSHA256: digestDocumentBytes(packages),
		Provenance: GenerationProvenance{
			PackageAtom: "app-misc/pkg", BuildInputSHA256: strings.Repeat("a", 64),
			BuildMode: "native-gentoo",
		},
		Artifacts: []GenerationArtifact{{
			RelativePath: relative, SHA256: digestDocumentBytes(payload),
			Size: int64(len(payload)), ObjectKey: artifactKey,
		}},
		CreatedAt: createdAt,
	}
	document, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	manifestKey, _ := PublishedGenerationKey(
		binhostPath, arch, generationID, GenerationManifestName,
	)
	if err := UploadBytes(store, manifestKey, document, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return manifest, document
}
