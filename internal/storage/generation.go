package storage

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/internal/catalog"
)

const (
	GenerationManifestSchema = 2
	QuarantineManifestSchema = 1
	ChannelPointerSchema     = 1
	QuarantineCapabilityName = ".portage-engine-capability"
	GenerationManifestName   = "manifest.json"
)

var (
	generationDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	generationTokenPattern  = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// GenerationArtifact binds one immutable object to the manifest that selects
// it. RelativePath uses the ordinary Portage PKGDIR layout.
type GenerationArtifact struct {
	RelativePath string `json:"relative_path"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	ObjectKey    string `json:"object_key,omitempty"`
}

// GenerationRepository records only public, immutable repository identity.
// Internal checkout paths and sync credentials never enter a public manifest.
type GenerationRepository struct {
	ID       string `json:"id"`
	Revision string `json:"revision,omitempty"`
	Digest   string `json:"digest,omitempty"`
}

// GenerationProvenance binds the published bytes to their reviewed build
// inputs without exposing ConfigBundle contents or internal infrastructure.
type GenerationProvenance struct {
	PackageAtom             string                 `json:"package_atom"`
	BuildInputSHA256        string                 `json:"build_input_sha256"`
	CatalogVersion          int                    `json:"catalog_version,omitempty"`
	BuildMode               string                 `json:"build_mode"`
	ImageID                 string                 `json:"image_id,omitempty"`
	ImageGeneration         string                 `json:"image_generation,omitempty"`
	ImageDigest             string                 `json:"image_digest,omitempty"`
	MirrorBundleID          string                 `json:"mirror_bundle_id,omitempty"`
	MirrorBundleDigest      string                 `json:"mirror_bundle_digest,omitempty"`
	EgressPolicyDigest      string                 `json:"egress_policy_digest,omitempty"`
	PackageSetIDs           []string               `json:"package_set_ids,omitempty"`
	PackageSetCatalogDigest string                 `json:"package_set_catalog_digest,omitempty"`
	Repositories            []GenerationRepository `json:"repositories,omitempty"`
}

// GenerationManifest is the immutable commit record for one complete binhost
// generation. A separately fenced channel pointer selects the active manifest.
type GenerationManifest struct {
	SchemaVersion  int                  `json:"schema_version"`
	GenerationID   string               `json:"generation_id"`
	BinhostPath    string               `json:"binhost_path"`
	Architecture   string               `json:"architecture"`
	ProfileID      string               `json:"profile_id"`
	AttemptID      string               `json:"attempt_id"`
	SigningKeyID   string               `json:"signing_key_id"`
	PackagesSHA256 string               `json:"packages_sha256"`
	Provenance     GenerationProvenance `json:"provenance,omitempty"`
	Artifacts      []GenerationArtifact `json:"artifacts"`
	CreatedAt      time.Time            `json:"created_at"`
}

// ChannelPointer is the only mutable publication object. Its backend ETag is
// the fencing token; readers validate the selected immutable manifest before
// serving any bytes.
type ChannelPointer struct {
	SchemaVersion        int       `json:"schema_version"`
	Channel              string    `json:"channel"`
	BinhostPath          string    `json:"binhost_path"`
	Architecture         string    `json:"architecture"`
	GenerationID         string    `json:"generation_id"`
	ManifestKey          string    `json:"manifest_key"`
	ManifestSHA256       string    `json:"manifest_sha256"`
	PackagesSHA256       string    `json:"packages_sha256"`
	PreviousGenerationID string    `json:"previous_generation_id,omitempty"`
	SelectedAt           time.Time `json:"selected_at"`
}

// QuarantineManifest is the revocable commit record for one private
// verification generation. The capability URL is authorized only while this
// exact object exists and has not expired; artifact bytes remain immutable.
type QuarantineManifest struct {
	SchemaVersion  int                  `json:"schema_version"`
	Token          string               `json:"token"`
	Generation     string               `json:"generation"`
	Architecture   string               `json:"architecture"`
	PackagesSHA256 string               `json:"packages_sha256"`
	Artifacts      []GenerationArtifact `json:"artifacts"`
	ExpiresAt      time.Time            `json:"expires_at"`
}

// Validate checks that a quarantine capability selects one bounded, exact
// artifact set and cannot be repurposed as an arbitrary object-store browser.
func (m QuarantineManifest) Validate(now time.Time) error {
	if m.SchemaVersion != QuarantineManifestSchema ||
		!generationTokenPattern.MatchString(m.Token) ||
		(m.Generation != "unsigned" && m.Generation != "signed") ||
		strings.TrimSpace(m.Architecture) == "" ||
		!generationDigestPattern.MatchString(m.PackagesSHA256) ||
		len(m.Artifacts) == 0 || len(m.Artifacts) > 256 ||
		m.ExpiresAt.IsZero() {
		return fmt.Errorf("invalid quarantine manifest identity")
	}
	if !now.IsZero() && !now.Before(m.ExpiresAt) {
		return fmt.Errorf("quarantine manifest expired")
	}
	seen := make(map[string]struct{}, len(m.Artifacts))
	for _, artifact := range m.Artifacts {
		if err := artifact.validate(); err != nil {
			return err
		}
		if _, exists := seen[artifact.RelativePath]; exists {
			return fmt.Errorf("duplicate quarantine artifact %q", artifact.RelativePath)
		}
		seen[artifact.RelativePath] = struct{}{}
	}
	return nil
}

// Marshal returns the canonical JSON representation stored as the revocable
// capability marker.
func (m QuarantineManifest) Marshal() ([]byte, error) {
	if err := m.Validate(time.Time{}); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// ParseQuarantineManifest decodes a bounded capability marker.
func ParseQuarantineManifest(document []byte, now time.Time) (QuarantineManifest, error) {
	if len(document) == 0 || len(document) > 1<<20 {
		return QuarantineManifest{}, fmt.Errorf("invalid quarantine manifest size")
	}
	var manifest QuarantineManifest
	decoder := json.NewDecoder(strings.NewReader(string(document)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return QuarantineManifest{}, fmt.Errorf("decode quarantine manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return QuarantineManifest{}, fmt.Errorf("decode quarantine manifest: trailing JSON value")
		}
		return QuarantineManifest{}, fmt.Errorf("decode quarantine manifest: %w", err)
	}
	if err := manifest.Validate(now); err != nil {
		return QuarantineManifest{}, err
	}
	return manifest, nil
}

// Allows reports whether a relative path is part of the exact capability.
// Packages is generated from and bound to this artifact set.
func (m QuarantineManifest) Allows(relativePath string) bool {
	if relativePath == "Packages" {
		return true
	}
	for _, artifact := range m.Artifacts {
		if artifact.RelativePath == relativePath {
			return true
		}
	}
	return false
}

// Validate checks that a manifest can identify one complete immutable
// generation without relying on caller-selected path inference.
func (m GenerationManifest) Validate() error {
	if m.SchemaVersion != 1 && m.SchemaVersion != GenerationManifestSchema {
		return fmt.Errorf("unsupported generation manifest schema %d", m.SchemaVersion)
	}
	if _, err := uuid.Parse(m.GenerationID); err != nil {
		return fmt.Errorf("invalid generation id: %w", err)
	}
	if _, err := uuid.Parse(m.AttemptID); err != nil {
		return fmt.Errorf("invalid generation attempt id: %w", err)
	}
	if err := catalog.ValidateBinhostPath(m.BinhostPath, m.Architecture); err != nil {
		return err
	}
	if strings.TrimSpace(m.ProfileID) == "" ||
		strings.TrimSpace(m.SigningKeyID) == "" ||
		!generationDigestPattern.MatchString(m.PackagesSHA256) ||
		m.CreatedAt.IsZero() || len(m.Artifacts) == 0 {
		return fmt.Errorf("generation manifest identity, key, Packages digest, timestamp, and artifacts are required")
	}
	if m.SchemaVersion >= 2 {
		if err := m.Provenance.validate(); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(m.Artifacts))
	for _, artifact := range m.Artifacts {
		if err := artifact.validate(); err != nil {
			return err
		}
		if err := artifact.validatePublishedKey(m.BinhostPath); err != nil {
			return err
		}
		if _, exists := seen[artifact.RelativePath]; exists {
			return fmt.Errorf("duplicate generation artifact %q", artifact.RelativePath)
		}
		seen[artifact.RelativePath] = struct{}{}
	}
	return nil
}

func (p GenerationProvenance) validate() error {
	if strings.TrimSpace(p.PackageAtom) == "" ||
		!generationDigestPattern.MatchString(p.BuildInputSHA256) ||
		strings.TrimSpace(p.BuildMode) == "" {
		return fmt.Errorf("generation provenance package, input digest, and build mode are required")
	}
	for name, digest := range map[string]string{
		"image":               p.ImageDigest,
		"mirror bundle":       p.MirrorBundleDigest,
		"egress policy":       p.EgressPolicyDigest,
		"package-set catalog": p.PackageSetCatalogDigest,
	} {
		if digest != "" && !validPrefixedSHA256(digest) {
			return fmt.Errorf("generation %s digest must be sha256:<64 lowercase hex>", name)
		}
	}
	seen := make(map[string]struct{}, len(p.Repositories))
	for _, repository := range p.Repositories {
		if strings.TrimSpace(repository.ID) == "" ||
			(repository.Revision == "" && repository.Digest == "") {
			return fmt.Errorf("generation repository identity and revision or digest are required")
		}
		if repository.Digest != "" && !validPrefixedSHA256(repository.Digest) {
			return fmt.Errorf("generation repository digest must be sha256:<64 lowercase hex>")
		}
		if _, exists := seen[repository.ID]; exists {
			return fmt.Errorf("duplicate generation repository %q", repository.ID)
		}
		seen[repository.ID] = struct{}{}
	}
	return nil
}

func validPrefixedSHA256(digest string) bool {
	const prefix = "sha256:"
	return strings.HasPrefix(digest, prefix) &&
		generationDigestPattern.MatchString(strings.TrimPrefix(digest, prefix))
}

func (m GenerationManifest) Marshal() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func ParseGenerationManifest(document []byte) (GenerationManifest, error) {
	if len(document) == 0 || len(document) > 4<<20 {
		return GenerationManifest{}, fmt.Errorf("invalid generation manifest size")
	}
	var manifest GenerationManifest
	if err := decodeStrictJSON(document, &manifest); err != nil {
		return GenerationManifest{}, fmt.Errorf("decode generation manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return GenerationManifest{}, err
	}
	return manifest, nil
}

func (p ChannelPointer) Validate() error {
	if p.SchemaVersion != ChannelPointerSchema || p.Channel != "stable" ||
		!generationDigestPattern.MatchString(p.ManifestSHA256) ||
		!generationDigestPattern.MatchString(p.PackagesSHA256) ||
		p.SelectedAt.IsZero() {
		return fmt.Errorf("invalid channel pointer identity")
	}
	if _, err := uuid.Parse(p.GenerationID); err != nil {
		return fmt.Errorf("invalid channel generation id: %w", err)
	}
	if p.PreviousGenerationID != "" {
		if _, err := uuid.Parse(p.PreviousGenerationID); err != nil {
			return fmt.Errorf("invalid previous channel generation id: %w", err)
		}
	}
	if err := catalog.ValidateBinhostPath(p.BinhostPath, p.Architecture); err != nil {
		return err
	}
	expected, err := PublishedGenerationKey(
		p.BinhostPath, p.Architecture, p.GenerationID, GenerationManifestName,
	)
	if err != nil || p.ManifestKey != expected {
		return fmt.Errorf("channel manifest key does not match its generation")
	}
	return nil
}

func (p ChannelPointer) Marshal() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

func ParseChannelPointer(document []byte) (ChannelPointer, error) {
	if len(document) == 0 || len(document) > 1<<20 {
		return ChannelPointer{}, fmt.Errorf("invalid channel pointer size")
	}
	var pointer ChannelPointer
	if err := decodeStrictJSON(document, &pointer); err != nil {
		return ChannelPointer{}, fmt.Errorf("decode channel pointer: %w", err)
	}
	if err := pointer.Validate(); err != nil {
		return ChannelPointer{}, err
	}
	return pointer, nil
}

func decodeStrictJSON(document []byte, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(document)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func (a GenerationArtifact) validate() error {
	relative, err := cleanGenerationRelativePath(a.RelativePath)
	if err != nil || relative == "Packages" || strings.HasPrefix(relative, ".") {
		return fmt.Errorf("invalid generation artifact path %q", a.RelativePath)
	}
	if !generationDigestPattern.MatchString(a.SHA256) || a.Size < 0 {
		return fmt.Errorf("invalid generation artifact digest or size for %q", a.RelativePath)
	}
	return nil
}

func (a GenerationArtifact) validatePublishedKey(binhostPath string) error {
	prefix := path.Join(binhostPath, ".generations") + "/"
	if !strings.HasPrefix(a.ObjectKey, prefix) ||
		!strings.HasSuffix(a.ObjectKey, "/"+a.RelativePath) {
		return fmt.Errorf("artifact %q has an invalid published object key", a.RelativePath)
	}
	rest := strings.TrimPrefix(a.ObjectKey, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("artifact %q has an invalid published object key", a.RelativePath)
	}
	if _, err := uuid.Parse(parts[0]); err != nil {
		return fmt.Errorf("artifact %q has an invalid source generation", a.RelativePath)
	}
	return nil
}

// PublishedGenerationKey returns an immutable object key below a reviewed
// Gentoo-compatible target namespace.
func PublishedGenerationKey(
	binhostPath, architecture, generationID, relativePath string,
) (string, error) {
	if err := catalog.ValidateBinhostPath(binhostPath, architecture); err != nil {
		return "", err
	}
	if _, err := uuid.Parse(generationID); err != nil {
		return "", fmt.Errorf("invalid generation id: %w", err)
	}
	relative, err := cleanGenerationRelativePath(relativePath)
	if err != nil {
		return "", err
	}
	return path.Join(binhostPath, ".generations", generationID, relative), nil
}

// StableChannelKey returns the ETag-fenced pointer used by the read gateway.
func StableChannelKey(binhostPath, architecture string) (string, error) {
	if err := catalog.ValidateBinhostPath(binhostPath, architecture); err != nil {
		return "", err
	}
	return path.Join(binhostPath, ".channels", "stable.json"), nil
}

// QuarantineGenerationKey binds private bytes to an unguessable capability
// token. It is never a consumer-visible binhost path.
func QuarantineGenerationKey(token, relativePath string) (string, error) {
	if !generationTokenPattern.MatchString(token) {
		return "", fmt.Errorf("invalid quarantine generation token")
	}
	relative, err := cleanGenerationRelativePath(relativePath)
	if err != nil {
		return "", err
	}
	return path.Join(".quarantine", token, relative), nil
}

// QuarantineGenerationPrefix returns the only object prefix a quarantine
// lifecycle operation may list or delete.
func QuarantineGenerationPrefix(token string) (string, error) {
	if !generationTokenPattern.MatchString(token) {
		return "", fmt.Errorf("invalid quarantine generation token")
	}
	return path.Join(".quarantine", token) + "/", nil
}

// QuarantineCapabilityKey returns the revocable manifest object.
func QuarantineCapabilityKey(token string) (string, error) {
	return QuarantineGenerationKey(token, QuarantineCapabilityName)
}

func cleanGenerationRelativePath(relative string) (string, error) {
	relative = strings.ReplaceAll(relative, "\\", "/")
	clean := path.Clean(relative)
	if relative == "" || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") ||
		clean != relative {
		return "", fmt.Errorf("object path %q must be canonical and relative", relative)
	}
	return clean, nil
}
