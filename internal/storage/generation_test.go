package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGenerationManifestAndKeys(t *testing.T) {
	const (
		arch        = "amd64"
		binhostPath = "releases/amd64/binpackages/23.0/x86-64_pe-systemd-base-v1"
		relative    = "app-misc/jq/jq-1.8.2.gpkg.tar"
		digest      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	generationID := uuid.NewString()
	attemptID := uuid.NewString()
	objectKey, err := PublishedGenerationKey(
		binhostPath, arch, generationID, relative,
	)
	if err != nil {
		t.Fatal(err)
	}
	manifest := GenerationManifest{
		SchemaVersion:  GenerationManifestSchema,
		GenerationID:   generationID,
		BinhostPath:    binhostPath,
		Architecture:   arch,
		ProfileID:      "pe/amd64/glibc/systemd/base-v1",
		AttemptID:      attemptID,
		SigningKeyID:   "0123456789ABCDEF",
		PackagesSHA256: digest,
		Provenance: GenerationProvenance{
			PackageAtom:      "app-misc/jq-1.8.2",
			BuildInputSHA256: digest,
			BuildMode:        "native-gentoo",
			Repositories: []GenerationRepository{{
				ID: "gentoo", Revision: "0123456789abcdef",
			}},
		},
		Artifacts: []GenerationArtifact{{
			RelativePath: relative,
			SHA256:       digest,
			Size:         42,
			ObjectKey:    objectKey,
		}},
		CreatedAt: time.Now().UTC(),
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	key, err := PublishedGenerationKey(binhostPath, arch, generationID, relative)
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix := "/.generations/" + generationID + "/" + relative
	if !strings.HasSuffix(key, wantSuffix) {
		t.Fatalf("generation key = %q, want suffix %q", key, wantSuffix)
	}
	channel, err := StableChannelKey(binhostPath, arch)
	if err != nil {
		t.Fatal(err)
	}
	if channel != binhostPath+"/.channels/stable.json" {
		t.Fatalf("channel key = %q", channel)
	}
	manifestDocument, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseGenerationManifest(manifestDocument); err != nil {
		t.Fatal(err)
	}
	legacy := manifest
	legacy.SchemaVersion = 1
	legacy.Provenance = GenerationProvenance{}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("schema v1 read compatibility rejected: %v", err)
	}
	manifestKey, _ := PublishedGenerationKey(
		binhostPath, arch, generationID, GenerationManifestName,
	)
	pointer := ChannelPointer{
		SchemaVersion: ChannelPointerSchema, Channel: "stable",
		BinhostPath: binhostPath, Architecture: arch,
		GenerationID: generationID, ManifestKey: manifestKey,
		ManifestSHA256: digest, PackagesSHA256: digest,
		SelectedAt: time.Now().UTC(),
	}
	pointerDocument, err := pointer.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseChannelPointer(pointerDocument); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationKeysRejectUntrustedPaths(t *testing.T) {
	generationID := uuid.NewString()
	if _, err := PublishedGenerationKey(
		"releases/amd64/binpackages/23.0/x86-64",
		"amd64", generationID, "../escape",
	); err == nil {
		t.Fatal("invalid publication namespace was accepted")
	}
	if _, err := QuarantineGenerationKey(
		"not-a-token", "app-misc/jq/jq-1.8.2.gpkg.tar",
	); err == nil {
		t.Fatal("invalid quarantine token was accepted")
	}
	if _, err := QuarantineGenerationKey(
		strings.Repeat("a", 32), "../escape",
	); err == nil {
		t.Fatal("traversing quarantine path was accepted")
	}
}

func TestGenerationProvenanceRejectsMalformedDigests(t *testing.T) {
	base := GenerationProvenance{
		PackageAtom:      "app-misc/jq-1.8.2",
		BuildInputSHA256: strings.Repeat("a", 64),
		BuildMode:        "native-gentoo",
		Repositories: []GenerationRepository{{
			ID:       "gentoo",
			Revision: "0123456789abcdef",
		}},
	}
	tests := []struct {
		name   string
		mutate func(*GenerationProvenance)
	}{
		{
			name: "short image digest",
			mutate: func(provenance *GenerationProvenance) {
				provenance.ImageDigest = "sha256:abcd"
			},
		},
		{
			name: "uppercase mirror digest",
			mutate: func(provenance *GenerationProvenance) {
				provenance.MirrorBundleDigest = "sha256:" + strings.Repeat("A", 64)
			},
		},
		{
			name: "repository digest without algorithm",
			mutate: func(provenance *GenerationProvenance) {
				provenance.Repositories = []GenerationRepository{{
					ID:     "gentoo",
					Digest: strings.Repeat("b", 64),
				}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provenance := base
			provenance.Repositories = append(
				[]GenerationRepository(nil), base.Repositories...,
			)
			test.mutate(&provenance)
			if err := provenance.validate(); err == nil {
				t.Fatal("malformed digest was accepted")
			}
		})
	}
}

func TestQuarantineManifestBindsExactArtifactSet(t *testing.T) {
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	manifest := QuarantineManifest{
		SchemaVersion:  QuarantineManifestSchema,
		Token:          strings.Repeat("a", 32),
		Generation:     "unsigned",
		Architecture:   "amd64",
		PackagesSHA256: strings.Repeat("c", 64),
		ExpiresAt:      now.Add(30 * time.Minute),
		Artifacts: []GenerationArtifact{{
			RelativePath: "app-misc/jq/jq-1.8.2-1.gpkg.tar",
			SHA256:       strings.Repeat("b", 64),
			Size:         123,
		}},
	}
	document, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseQuarantineManifest(document, now)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Allows("Packages") ||
		!parsed.Allows("app-misc/jq/jq-1.8.2-1.gpkg.tar") ||
		parsed.Allows("private/other.gpkg.tar") {
		t.Fatalf("unexpected capability selection: %#v", parsed)
	}
	if _, err := ParseQuarantineManifest(document, manifest.ExpiresAt); err == nil {
		t.Fatal("expired quarantine capability was accepted")
	}
}

func TestQuarantinePrefixAndCapabilityAreConfined(t *testing.T) {
	token := strings.Repeat("c", 32)
	prefix, err := QuarantineGenerationPrefix(token)
	if err != nil || prefix != ".quarantine/"+token+"/" {
		t.Fatalf("prefix=%q err=%v", prefix, err)
	}
	key, err := QuarantineCapabilityKey(token)
	if err != nil || key != prefix+QuarantineCapabilityName {
		t.Fatalf("key=%q err=%v", key, err)
	}
	for _, invalid := range []string{"", "../escape", strings.Repeat("x", 31)} {
		if _, err := QuarantineGenerationPrefix(invalid); err == nil {
			t.Errorf("invalid token %q accepted", invalid)
		}
	}
}
