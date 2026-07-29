package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// PublicationAudit is a complete, byte-verified view of one selected channel.
type PublicationAudit struct {
	Pointer       ChannelPointer
	Manifest      GenerationManifest
	ObjectCount   int
	ArtifactBytes int64
}

// AuditPublishedChannel verifies pointer, manifest, Packages, and every
// referenced artifact. It is intentionally expensive and suitable for release
// Gates and scheduled reconciliation, not per-request serving.
func AuditPublishedChannel(
	store Storage,
	binhostPath, arch, scratchDir string,
) (PublicationAudit, error) {
	channelKey, err := StableChannelKey(binhostPath, arch)
	if err != nil {
		return PublicationAudit{}, err
	}
	pointerDocument, err := DownloadBytes(store, channelKey, scratchDir, 1<<20)
	if err != nil {
		return PublicationAudit{}, fmt.Errorf("read channel pointer: %w", err)
	}
	pointer, err := ParseChannelPointer(pointerDocument)
	if err != nil || pointer.BinhostPath != binhostPath ||
		pointer.Architecture != arch {
		return PublicationAudit{}, fmt.Errorf("channel pointer validation failed")
	}
	manifestDocument, err := DownloadBytes(
		store, pointer.ManifestKey, scratchDir, 4<<20,
	)
	if err != nil || digestDocumentBytes(manifestDocument) != pointer.ManifestSHA256 {
		return PublicationAudit{}, fmt.Errorf("generation manifest digest validation failed")
	}
	manifest, err := ParseGenerationManifest(manifestDocument)
	if err != nil || manifest.GenerationID != pointer.GenerationID ||
		manifest.PackagesSHA256 != pointer.PackagesSHA256 {
		return PublicationAudit{}, fmt.Errorf("generation manifest identity validation failed")
	}
	packagesKey, err := PublishedGenerationKey(
		binhostPath, arch, pointer.GenerationID, "Packages",
	)
	if err != nil {
		return PublicationAudit{}, err
	}
	packages, err := DownloadBytes(store, packagesKey, scratchDir, 64<<20)
	if err != nil || digestDocumentBytes(packages) != manifest.PackagesSHA256 {
		return PublicationAudit{}, fmt.Errorf("Packages digest validation failed")
	}
	report := PublicationAudit{
		Pointer: pointer, Manifest: manifest, ObjectCount: 2,
	}
	for _, artifact := range manifest.Artifacts {
		local, err := downloadObjectScratch(
			store, artifact.ObjectKey, scratchDir,
		)
		if err != nil {
			return PublicationAudit{}, fmt.Errorf(
				"download artifact %q: %w", artifact.RelativePath, err,
			)
		}
		digest, size, digestErr := digestLocalFile(local)
		_ = os.Remove(local)
		if digestErr != nil || digest != artifact.SHA256 || size != artifact.Size {
			return PublicationAudit{}, fmt.Errorf(
				"artifact %q digest or size validation failed",
				artifact.RelativePath,
			)
		}
		report.ObjectCount++
		report.ArtifactBytes += size
	}
	return report, nil
}

// ReplicatePublishedChannel copies the complete selected closure to another
// immutable store and commits the destination pointer last with CAS.
func ReplicatePublishedChannel(
	source Storage,
	destination VersionedStorage,
	binhostPath, arch, scratchDir string,
) (PublicationAudit, error) {
	report, err := AuditPublishedChannel(
		source, binhostPath, arch, scratchDir,
	)
	if err != nil {
		return PublicationAudit{}, err
	}
	keys := make([]string, 0, len(report.Manifest.Artifacts)+2)
	for _, artifact := range report.Manifest.Artifacts {
		keys = append(keys, artifact.ObjectKey)
	}
	packagesKey, _ := PublishedGenerationKey(
		binhostPath, arch, report.Pointer.GenerationID, "Packages",
	)
	keys = append(keys, packagesKey, report.Pointer.ManifestKey)
	for _, key := range keys {
		if err := copyImmutableObject(
			source, destination, key, scratchDir,
		); err != nil {
			return PublicationAudit{}, err
		}
	}
	channelKey, _ := StableChannelKey(binhostPath, arch)
	expectedVersion := ""
	if exists, existsErr := destination.Exists(channelKey); existsErr != nil {
		return PublicationAudit{}, existsErr
	} else if exists {
		current, version, readErr := DownloadVersionBytes(
			destination, channelKey, scratchDir, 1<<20,
		)
		if readErr != nil {
			return PublicationAudit{}, readErr
		}
		pointer, parseErr := ParseChannelPointer(current)
		if parseErr == nil && pointer.GenerationID == report.Pointer.GenerationID &&
			pointer.ManifestSHA256 == report.Pointer.ManifestSHA256 {
			return report, nil
		}
		expectedVersion = version
	}
	pointerDocument, err := report.Pointer.Marshal()
	if err != nil {
		return PublicationAudit{}, err
	}
	if _, err := CompareAndSwapBytes(
		destination, channelKey, pointerDocument,
		expectedVersion, scratchDir,
	); err != nil {
		return PublicationAudit{}, fmt.Errorf(
			"commit replicated channel pointer: %w", err,
		)
	}
	return report, nil
}

// GarbageCollectPublishedGenerations deletes only old, complete generations
// outside the active/rollback/reference closure. Incomplete uploads are left
// to a bucket age-based lifecycle policy because Storage.List has no trusted
// creation timestamp.
func GarbageCollectPublishedGenerations(
	store Storage,
	binhostPath, arch string,
	retention time.Duration,
	now time.Time,
	scratchDir string,
) ([]string, error) {
	if retention <= 0 || now.IsZero() {
		return nil, fmt.Errorf("positive retention and current time are required")
	}
	report, err := AuditPublishedChannel(store, binhostPath, arch, scratchDir)
	if err != nil {
		return nil, err
	}
	retained := map[string]struct{}{
		report.Pointer.GenerationID: {},
	}
	retainArtifactGenerations(retained, report.Manifest.Artifacts)
	if report.Pointer.PreviousGenerationID != "" {
		retained[report.Pointer.PreviousGenerationID] = struct{}{}
		previousKey, keyErr := PublishedGenerationKey(
			binhostPath, arch, report.Pointer.PreviousGenerationID,
			GenerationManifestName,
		)
		if keyErr != nil {
			return nil, keyErr
		}
		document, downloadErr := DownloadBytes(
			store, previousKey, scratchDir, 4<<20,
		)
		if downloadErr != nil {
			return nil, fmt.Errorf("read rollback manifest: %w", downloadErr)
		}
		previous, parseErr := ParseGenerationManifest(document)
		if parseErr != nil {
			return nil, fmt.Errorf("validate rollback manifest: %w", parseErr)
		}
		retainArtifactGenerations(retained, previous.Artifacts)
	}
	generationsPrefix := path.Join(binhostPath, ".generations") + "/"
	keys, err := store.List(generationsPrefix)
	if err != nil {
		return nil, err
	}
	candidates := make(map[string][]string)
	for _, key := range keys {
		generationID, ok := generationFromPublishedKey(
			generationsPrefix, key,
		)
		if ok {
			candidates[generationID] = append(candidates[generationID], key)
		}
	}
	deleted := make([]string, 0, len(keys))
	for generationID, generationKeys := range candidates {
		if _, keep := retained[generationID]; keep {
			continue
		}
		manifestKey, keyErr := PublishedGenerationKey(
			binhostPath, arch, generationID, GenerationManifestName,
		)
		if keyErr != nil {
			continue
		}
		document, downloadErr := DownloadBytes(
			store, manifestKey, scratchDir, 4<<20,
		)
		if downloadErr != nil {
			continue
		}
		manifest, parseErr := ParseGenerationManifest(document)
		if parseErr != nil || now.Sub(manifest.CreatedAt) < retention {
			continue
		}
		for _, candidatePath := range generationKeys {
			if !strings.HasPrefix(candidatePath, generationsPrefix+generationID+"/") {
				return deleted, fmt.Errorf("GC object escaped its generation prefix")
			}
			if err := store.Delete(candidatePath); err != nil {
				return deleted, err
			}
		}
		deleted = append(deleted, generationID)
	}
	return deleted, nil
}

func retainArtifactGenerations(
	retained map[string]struct{},
	artifacts []GenerationArtifact,
) {
	for _, artifact := range artifacts {
		parts := strings.Split(artifact.ObjectKey, "/")
		for index, part := range parts {
			if part == ".generations" && index+1 < len(parts) {
				retained[parts[index+1]] = struct{}{}
				break
			}
		}
	}
}

func generationFromPublishedKey(prefix, key string) (string, bool) {
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(key, prefix), "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

func copyImmutableObject(
	source Storage,
	destination Storage,
	key, scratchDir string,
) error {
	local, err := downloadObjectScratch(source, key, scratchDir)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(local) }()
	if err := destination.Upload(local, key); err != nil {
		return fmt.Errorf("replicate immutable object %q: %w", key, err)
	}
	return nil
}

func downloadObjectScratch(
	store Storage,
	key, scratchDir string,
) (string, error) {
	if scratchDir == "" {
		scratchDir = os.TempDir()
	}
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(scratchDir, ".publication-object-*")
	if err != nil {
		return "", err
	}
	name := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	_ = os.Remove(name)
	if err := store.Download(key, name); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return filepath.Clean(name), nil
}

func digestDocumentBytes(document []byte) string {
	digest := sha256.Sum256(document)
	return hex.EncodeToString(digest[:])
}

func digestLocalFile(name string) (string, int64, error) {
	file, err := os.Open(name) // #nosec G304 -- lifecycle scratch path.
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("object is not a regular file")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
