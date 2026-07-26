package imagefactory

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/catalog"
)

const operationsAlgorithm = "ed25519"

type OperationsPrivateKey struct {
	SchemaVersion int    `json:"schema_version"`
	KeyID         string `json:"key_id"`
	Algorithm     string `json:"algorithm"`
	PrivateKey    string `json:"private_key"`
}

type OperationsPublicKey struct {
	SchemaVersion int    `json:"schema_version"`
	KeyID         string `json:"key_id"`
	Algorithm     string `json:"algorithm"`
	PublicKey     string `json:"public_key"`
}

type DetachedSignature struct {
	SchemaVersion int    `json:"schema_version"`
	PayloadType   string `json:"payload_type"`
	KeyID         string `json:"key_id"`
	PayloadDigest string `json:"payload_digest"`
	Signature     string `json:"signature"`
}

type BundleObjectEvidence struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
	Executable bool   `json:"executable,omitempty"`
}

type BundleManifest struct {
	SchemaVersion     int                    `json:"schema_version"`
	BundleID          string                 `json:"bundle_id"`
	CreatedAt         time.Time              `json:"created_at"`
	FreshUntil        time.Time              `json:"fresh_until"`
	AdvisoryWatermark string                 `json:"advisory_watermark"`
	InputLockDigest   string                 `json:"input_lock_digest"`
	Objects           []BundleObjectEvidence `json:"objects"`
}

type PromotionPlan struct {
	SchemaVersion             int                    `json:"schema_version"`
	ReleaseID                 string                 `json:"release_id"`
	Alias                     string                 `json:"alias"`
	ExpectedPreviousReleaseID string                 `json:"expected_previous_release_id,omitempty"`
	CandidateCatalogDigest    string                 `json:"candidate_catalog_digest"`
	BundleManifestDigest      string                 `json:"bundle_manifest_digest"`
	Bundles                   []PromotionBundleRef   `json:"bundles,omitempty"`
	MinimumFreshHours         int                    `json:"minimum_fresh_hours"`
	Evidence                  []PromotionEvidenceRef `json:"evidence"`
}

// PromotionBundleRef points at one immutable, signed input bundle used by a
// member of a release group. Paths are relative to the promotion evidence
// root so a reviewed evidence tree can be moved without rewriting the plan.
type PromotionBundleRef struct {
	BundleID    string `json:"bundle_id"`
	Manifest    string `json:"manifest"`
	Signature   string `json:"signature"`
	InputLock   string `json:"input_lock"`
	OfflineRoot string `json:"offline_root"`
}

type PromotionEvidenceRef struct {
	ImageID           string `json:"image_id"`
	ImageManifest     string `json:"image_manifest"`
	SmokeResult       string `json:"smoke_result"`
	OutputStamp       string `json:"output_stamp"`
	DesktopResult     string `json:"desktop_result,omitempty"`
	DesktopScenarioID string `json:"desktop_scenario_id,omitempty"`
}

type SmokeResult struct {
	SchemaVersion            int       `json:"schema_version"`
	Target                   string    `json:"target"`
	CandidateManifest        string    `json:"candidate_manifest"`
	InstanceName             string    `json:"instance_name"`
	VMID                     string    `json:"vmid"`
	Node                     string    `json:"node"`
	GuestIP                  string    `json:"guest_ip"`
	CloudInitRuns            int       `json:"cloud_init_runs"`
	TerraformDestroyRequired bool      `json:"terraform_destroy_required"`
	TerraformDestroyed       bool      `json:"terraform_destroyed"`
	OutputProvenanceStamped  bool      `json:"output_provenance_stamped"`
	CompletedAt              time.Time `json:"completed_at"`
}

type PromotedImageEvidence struct {
	ImageID             string    `json:"image_id"`
	ImageManifestDigest string    `json:"image_manifest_digest"`
	SmokeResultDigest   string    `json:"smoke_result_digest"`
	OutputStampDigest   string    `json:"output_stamp_digest"`
	DesktopResultDigest string    `json:"desktop_result_digest,omitempty"`
	CompletedAt         time.Time `json:"completed_at"`
}

type PromotionReceipt struct {
	SchemaVersion          int                      `json:"schema_version"`
	ReleaseID              string                   `json:"release_id"`
	Alias                  string                   `json:"alias"`
	PreviousReleaseID      string                   `json:"previous_release_id,omitempty"`
	PromotedAt             time.Time                `json:"promoted_at"`
	CatalogDigest          string                   `json:"catalog_digest"`
	CandidateCatalogDigest string                   `json:"candidate_catalog_digest"`
	BundleID               string                   `json:"bundle_id"`
	BundleManifestDigest   string                   `json:"bundle_manifest_digest"`
	BundleFreshUntil       time.Time                `json:"bundle_fresh_until"`
	AdvisoryWatermark      string                   `json:"advisory_watermark"`
	Bundles                []PromotedBundleEvidence `json:"bundles,omitempty"`
	Images                 []PromotedImageEvidence  `json:"images"`
}

type PromotedBundleEvidence struct {
	BundleID             string    `json:"bundle_id"`
	BundleManifestDigest string    `json:"bundle_manifest_digest"`
	InputLockDigest      string    `json:"input_lock_digest"`
	FreshUntil           time.Time `json:"fresh_until"`
	AdvisoryWatermark    string    `json:"advisory_watermark"`
}

type ReleaseAlias struct {
	SchemaVersion     int       `json:"schema_version"`
	Alias             string    `json:"alias"`
	Revision          int       `json:"revision"`
	ReleaseID         string    `json:"release_id"`
	PreviousReleaseID string    `json:"previous_release_id,omitempty"`
	CatalogDigest     string    `json:"catalog_digest"`
	ReceiptDigest     string    `json:"receipt_digest"`
	UpdatedAt         time.Time `json:"updated_at"`
	Reason            string    `json:"reason"`
}

// SignedReleaseAlias keeps alias state and its signature in one file so the
// mutable cutover can be performed with a single atomic rename.
type SignedReleaseAlias struct {
	SchemaVersion int               `json:"schema_version"`
	Alias         ReleaseAlias      `json:"alias"`
	Signature     DetachedSignature `json:"signature"`
}

type PromotionResult struct {
	Catalog          *catalog.Catalog
	Receipt          *PromotionReceipt
	ReceiptSignature *DetachedSignature
	Alias            *ReleaseAlias
	AliasSignature   *DetachedSignature
	AliasEnvelope    *SignedReleaseAlias
}

type OperationsState struct {
	SchemaVersion      int                `json:"schema_version"`
	CapturedAt         time.Time          `json:"captured_at"`
	ValidUntil         time.Time          `json:"valid_until"`
	RetainNewest       int                `json:"retain_newest"`
	MinRetiredAgeHours int                `json:"min_retired_age_hours"`
	Aliases            []ReleaseAlias     `json:"aliases"`
	Generations        []GenerationRecord `json:"generations"`
	Leases             []GenerationLease  `json:"leases"`
}

type SignedOperationsState struct {
	SchemaVersion int               `json:"schema_version"`
	State         OperationsState   `json:"state"`
	Signature     DetachedSignature `json:"signature"`
}

type GenerationRecord struct {
	ReleaseID     string    `json:"release_id"`
	CatalogPath   string    `json:"catalog_path"`
	CatalogDigest string    `json:"catalog_digest"`
	CreatedAt     time.Time `json:"created_at"`
	RetiredAt     time.Time `json:"retired_at,omitempty"`
}

type GenerationLease struct {
	LeaseID   string    `json:"lease_id"`
	ReleaseID string    `json:"release_id"`
	ExpiresAt time.Time `json:"expires_at"`
	State     string    `json:"state"`
}

type CleanupDecision struct {
	ReleaseID   string `json:"release_id"`
	CatalogPath string `json:"catalog_path"`
	Action      string `json:"action"`
	Reason      string `json:"reason"`
}

type CleanupPlan struct {
	SchemaVersion int               `json:"schema_version"`
	PlannedAt     time.Time         `json:"planned_at"`
	Decisions     []CleanupDecision `json:"decisions"`
}

type RebuildPolicy struct {
	SchemaVersion       int                  `json:"schema_version"`
	IntervalHours       int                  `json:"interval_hours"`
	MinimumFreshHours   int                  `json:"minimum_fresh_hours"`
	Profiles            []string             `json:"profiles"`
	LastSuccessfulBuild map[string]time.Time `json:"last_successful_build,omitempty"`
}

type RebuildDecision struct {
	ProfileID string `json:"profile_id"`
	Due       bool   `json:"due"`
	Reason    string `json:"reason"`
}

type RebuildPlan struct {
	SchemaVersion int               `json:"schema_version"`
	PlannedAt     time.Time         `json:"planned_at"`
	BundleID      string            `json:"bundle_id"`
	Decisions     []RebuildDecision `json:"decisions"`
}

func NewOperationsKeyPair() (*OperationsPrivateKey, *OperationsPublicKey, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	keyID := operationsKeyID(public)
	return &OperationsPrivateKey{SchemaVersion: 1, KeyID: keyID, Algorithm: operationsAlgorithm, PrivateKey: base64.StdEncoding.EncodeToString(private)},
		&OperationsPublicKey{SchemaVersion: 1, KeyID: keyID, Algorithm: operationsAlgorithm, PublicKey: base64.StdEncoding.EncodeToString(public)}, nil
}

func SealBundle(lockPath, offlineRoot, privateKeyPath string, now time.Time, freshness time.Duration) (*BundleManifest, *DetachedSignature, error) {
	if freshness < time.Hour || freshness > 90*24*time.Hour {
		return nil, nil, fmt.Errorf("bundle freshness must be between one hour and 90 days")
	}
	lock, err := LoadInputLock(lockPath)
	if err != nil {
		return nil, nil, err
	}
	cutoff, err := time.Parse(time.RFC3339, lock.AdvisoryCutoff)
	if err != nil || cutoff.IsZero() || cutoff.After(now) {
		return nil, nil, fmt.Errorf("input lock requires a non-future RFC3339 advisory_cutoff")
	}
	root, err := filepath.Abs(offlineRoot)
	if err != nil {
		return nil, nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil, err
	}
	if inside, err := pathWithin(root, privateKeyPath); err != nil || inside {
		return nil, nil, fmt.Errorf("bundle signing private key must be outside the offline root")
	}
	objects := make([]BundleObjectEvidence, 0, len(lock.Objects))
	for _, object := range lock.Objects {
		if object.SHA256 == strings.Repeat("0", 64) {
			return nil, nil, fmt.Errorf("object %q still has a placeholder digest", object.ID)
		}
		if finding := verifyObject(root, object); finding != nil {
			return nil, nil, fmt.Errorf("object %q: %s", object.ID, finding.Reason)
		}
		objects = append(objects, BundleObjectEvidence{ID: object.ID, Kind: object.Kind, Path: object.Path, Digest: "sha256:" + object.SHA256, Size: object.Size, Executable: object.Executable})
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].ID < objects[j].ID })
	lockDigest, err := digestFile(lockPath)
	if err != nil {
		return nil, nil, err
	}
	manifest := &BundleManifest{SchemaVersion: 1, BundleID: lock.BundleID, CreatedAt: now.UTC(), FreshUntil: now.UTC().Add(freshness),
		AdvisoryWatermark: cutoff.UTC().Format(time.RFC3339), InputLockDigest: "sha256:" + lockDigest, Objects: objects}
	signature, err := SignOperationsPayload("mirror-bundle-manifest", manifest, privateKeyPath)
	if err != nil {
		return nil, nil, err
	}
	return manifest, signature, nil
}

func VerifyBundle(manifestPath, signaturePath, publicKeyPath, lockPath, offlineRoot string, now time.Time) (*BundleManifest, error) {
	var manifest BundleManifest
	if err := decodeStrictFile(manifestPath, &manifest); err != nil {
		return nil, err
	}
	validity := manifest.FreshUntil.Sub(manifest.CreatedAt)
	if manifest.SchemaVersion != 1 || !objectIDPattern.MatchString(manifest.BundleID) || manifest.CreatedAt.IsZero() || manifest.FreshUntil.IsZero() || validity < time.Hour || validity > 90*24*time.Hour || manifest.AdvisoryWatermark == "" || !prefixedSHA256Pattern.MatchString(manifest.InputLockDigest) || len(manifest.Objects) == 0 {
		return nil, fmt.Errorf("bundle manifest is incomplete")
	}
	if manifest.CreatedAt.After(now) {
		return nil, fmt.Errorf("bundle %q was created in the future", manifest.BundleID)
	}
	if !now.Before(manifest.FreshUntil) {
		return nil, fmt.Errorf("bundle %q expired at %s", manifest.BundleID, manifest.FreshUntil.Format(time.RFC3339))
	}
	if err := VerifyOperationsPayload("mirror-bundle-manifest", &manifest, signaturePath, publicKeyPath); err != nil {
		return nil, err
	}
	lock, err := LoadInputLock(lockPath)
	if err != nil {
		return nil, err
	}
	lockDigest, err := digestFile(lockPath)
	if err != nil {
		return nil, err
	}
	advisoryCutoff, cutoffErr := time.Parse(time.RFC3339, manifest.AdvisoryWatermark)
	if cutoffErr != nil || advisoryCutoff.After(manifest.CreatedAt) || manifest.BundleID != lock.BundleID || manifest.InputLockDigest != "sha256:"+lockDigest || manifest.AdvisoryWatermark != lock.AdvisoryCutoff {
		return nil, fmt.Errorf("bundle manifest does not match the input lock")
	}
	root, err := filepath.Abs(offlineRoot)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	expected := make([]BundleObjectEvidence, 0, len(lock.Objects))
	for _, object := range lock.Objects {
		if finding := verifyObject(root, object); finding != nil {
			return nil, fmt.Errorf("object %q: %s", object.ID, finding.Reason)
		}
		expected = append(expected, BundleObjectEvidence{ID: object.ID, Kind: object.Kind, Path: object.Path, Digest: "sha256:" + object.SHA256, Size: object.Size, Executable: object.Executable})
	}
	sort.Slice(expected, func(i, j int) bool { return expected[i].ID < expected[j].ID })
	if !equalCanonical(manifest.Objects, expected) {
		return nil, fmt.Errorf("bundle manifest object inventory drifted from the input lock")
	}
	return &manifest, nil
}

func SignOperationsPayload(payloadType string, value any, privateKeyPath string) (*DetachedSignature, error) {
	privateKey, err := loadOperationsPrivateKey(privateKeyPath)
	if err != nil {
		return nil, err
	}
	payload, err := canonicalJSON(value)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	signature := ed25519.Sign(privateKey, payload)
	return &DetachedSignature{SchemaVersion: 1, PayloadType: payloadType, KeyID: operationsKeyID(privateKey.Public().(ed25519.PublicKey)),
		PayloadDigest: "sha256:" + hex.EncodeToString(digest[:]), Signature: base64.StdEncoding.EncodeToString(signature)}, nil
}

func VerifyOperationsPayload(payloadType string, value any, signaturePath, publicKeyPath string) error {
	var signature DetachedSignature
	if err := decodeStrictFile(signaturePath, &signature); err != nil {
		return err
	}
	return VerifyOperationsDetached(payloadType, value, &signature, publicKeyPath)
}

func VerifyOperationsDetached(payloadType string, value any, signature *DetachedSignature, publicKeyPath string) error {
	if signature == nil {
		return fmt.Errorf("detached signature is missing")
	}
	publicKey, keyID, err := loadOperationsPublicKey(publicKeyPath)
	if err != nil {
		return err
	}
	payload, err := canonicalJSON(value)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	if signature.SchemaVersion != 1 || signature.PayloadType != payloadType || signature.KeyID != keyID || signature.PayloadDigest != "sha256:"+hex.EncodeToString(digest[:]) {
		return fmt.Errorf("detached signature metadata does not match payload")
	}
	decoded, err := base64.StdEncoding.DecodeString(signature.Signature)
	if err != nil || len(decoded) != ed25519.SignatureSize || !ed25519.Verify(publicKey, payload, decoded) {
		return fmt.Errorf("detached signature verification failed")
	}
	return nil
}

func PlanGenerationCleanup(state *OperationsState, now time.Time) (*CleanupPlan, error) {
	if state == nil || state.SchemaVersion != 1 || state.RetainNewest < 1 || state.RetainNewest > 32 || state.MinRetiredAgeHours < 1 || state.MinRetiredAgeHours > 24*365 {
		return nil, fmt.Errorf("invalid operations cleanup state")
	}
	if state.CapturedAt.IsZero() || state.ValidUntil.IsZero() || state.ValidUntil.Sub(state.CapturedAt) <= 0 || state.ValidUntil.Sub(state.CapturedAt) > time.Hour {
		return nil, fmt.Errorf("operations cleanup state must have a validity window of at most one hour")
	}
	if state.CapturedAt.After(now) {
		return nil, fmt.Errorf("operations cleanup state was captured in the future")
	}
	if !now.Before(state.ValidUntil) {
		return nil, fmt.Errorf("operations cleanup state has expired")
	}
	protected := map[string]string{}
	aliasReleases := map[string]struct{}{}
	for _, alias := range state.Aliases {
		if !validOperationsID(alias.ReleaseID) || !validOperationsID(alias.Alias) {
			return nil, fmt.Errorf("invalid release alias")
		}
		protected[alias.ReleaseID] = "selected by alias " + alias.Alias
		aliasReleases[alias.ReleaseID] = struct{}{}
	}
	leaseIDs := map[string]struct{}{}
	leaseReleases := map[string]struct{}{}
	for _, lease := range state.Leases {
		if !validOperationsID(lease.LeaseID) || !validOperationsID(lease.ReleaseID) || lease.ExpiresAt.IsZero() || (lease.State != "active" && lease.State != "released") {
			return nil, fmt.Errorf("invalid generation lease")
		}
		if _, duplicate := leaseIDs[lease.LeaseID]; duplicate {
			return nil, fmt.Errorf("duplicate generation lease %q", lease.LeaseID)
		}
		leaseIDs[lease.LeaseID] = struct{}{}
		leaseReleases[lease.ReleaseID] = struct{}{}
		if lease.State == "active" && now.Before(lease.ExpiresAt) {
			protected[lease.ReleaseID] = "active lease " + lease.LeaseID
		}
	}
	generations := append([]GenerationRecord(nil), state.Generations...)
	sort.Slice(generations, func(i, j int) bool { return generations[i].CreatedAt.After(generations[j].CreatedAt) })
	for index, generation := range generations {
		if index < state.RetainNewest {
			protected[generation.ReleaseID] = "within retain_newest window"
		}
	}
	plan := &CleanupPlan{SchemaVersion: 1, PlannedAt: now.UTC(), Decisions: make([]CleanupDecision, 0, len(generations))}
	minimumAge := time.Duration(state.MinRetiredAgeHours) * time.Hour
	seen := map[string]struct{}{}
	for _, generation := range generations {
		if !validOperationsID(generation.ReleaseID) || generation.CreatedAt.IsZero() || !prefixedSHA256Pattern.MatchString(generation.CatalogDigest) || generation.CatalogPath == "" || filepath.IsAbs(generation.CatalogPath) || filepath.Clean(generation.CatalogPath) != generation.CatalogPath {
			return nil, fmt.Errorf("invalid generation record %q", generation.ReleaseID)
		}
		if _, duplicate := seen[generation.ReleaseID]; duplicate {
			return nil, fmt.Errorf("duplicate generation %q", generation.ReleaseID)
		}
		seen[generation.ReleaseID] = struct{}{}
		decision := CleanupDecision{ReleaseID: generation.ReleaseID, CatalogPath: generation.CatalogPath, Action: "keep"}
		switch reason := protected[generation.ReleaseID]; {
		case reason != "":
			decision.Reason = reason
		case generation.RetiredAt.IsZero():
			decision.Reason = "generation is not retired"
		case now.Sub(generation.RetiredAt) < minimumAge:
			decision.Reason = "retirement safety window has not elapsed"
		default:
			decision.Action = "delete"
			decision.Reason = "retired, unaliased, unleased, and outside retention windows"
		}
		plan.Decisions = append(plan.Decisions, decision)
	}
	for releaseID := range aliasReleases {
		if _, exists := seen[releaseID]; !exists {
			return nil, fmt.Errorf("alias references unknown generation %q", releaseID)
		}
	}
	for releaseID := range leaseReleases {
		if _, exists := seen[releaseID]; !exists {
			return nil, fmt.Errorf("lease references unknown generation %q", releaseID)
		}
	}
	return plan, nil
}

func PlanRebuild(policy *RebuildPolicy, bundle *BundleManifest, now time.Time) (*RebuildPlan, error) {
	if policy == nil || policy.SchemaVersion != 1 || policy.IntervalHours < 1 || policy.IntervalHours > 24*365 || policy.MinimumFreshHours < 1 || policy.MinimumFreshHours > policy.IntervalHours || len(policy.Profiles) == 0 || bundle == nil || bundle.SchemaVersion != 1 || !validOperationsID(bundle.BundleID) || bundle.FreshUntil.IsZero() {
		return nil, fmt.Errorf("invalid rebuild policy or bundle manifest")
	}
	seen := map[string]struct{}{}
	plan := &RebuildPlan{SchemaVersion: 1, PlannedAt: now.UTC(), BundleID: bundle.BundleID, Decisions: make([]RebuildDecision, 0, len(policy.Profiles))}
	for _, profileID := range policy.Profiles {
		if !objectIDPattern.MatchString(profileID) {
			return nil, fmt.Errorf("invalid rebuild profile %q", profileID)
		}
		if _, duplicate := seen[profileID]; duplicate {
			return nil, fmt.Errorf("duplicate rebuild profile %q", profileID)
		}
		seen[profileID] = struct{}{}
		last := policy.LastSuccessfulBuild[profileID]
		if last.After(now) {
			return nil, fmt.Errorf("last successful build for %q is in the future", profileID)
		}
		decision := RebuildDecision{ProfileID: profileID}
		switch {
		case !now.Before(bundle.FreshUntil):
			decision.Reason = "bundle is expired; sync and seal a new bundle before rebuilding"
		case bundle.FreshUntil.Sub(now) < time.Duration(policy.MinimumFreshHours)*time.Hour:
			decision.Reason = "bundle freshness remaining is below policy; sync first"
		case last.IsZero():
			decision.Due, decision.Reason = true, "profile has no successful rebuild record"
		case now.Sub(last) >= time.Duration(policy.IntervalHours)*time.Hour:
			decision.Due, decision.Reason = true, "rebuild interval elapsed"
		default:
			decision.Reason = "rebuild interval has not elapsed"
		}
		plan.Decisions = append(plan.Decisions, decision)
	}
	return plan, nil
}

func LoadBundleManifest(path string) (*BundleManifest, error) {
	var manifest BundleManifest
	if err := decodeStrictFile(path, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func LoadOperationsState(path string) (*OperationsState, error) {
	var state OperationsState
	if err := decodeStrictFile(path, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func LoadSignedOperationsState(path, publicKeyPath string) (*OperationsState, error) {
	var envelope SignedOperationsState
	if err := decodeStrictFile(path, &envelope); err != nil {
		return nil, err
	}
	if envelope.SchemaVersion != 1 || envelope.State.SchemaVersion != 1 {
		return nil, fmt.Errorf("signed operations state is invalid")
	}
	if err := VerifyOperationsDetached("operations-state", &envelope.State, &envelope.Signature, publicKeyPath); err != nil {
		return nil, err
	}
	return &envelope.State, nil
}

func LoadRebuildPolicy(path string) (*RebuildPolicy, error) {
	var policy RebuildPolicy
	if err := decodeStrictFile(path, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

func CanonicalDigest(value any) (string, error) {
	return canonicalDigest(value)
}

func canonicalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

func canonicalDigest(value any) (string, error) {
	payload, err := canonicalJSON(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func equalCanonical(left, right any) bool {
	leftJSON, leftErr := canonicalJSON(left)
	rightJSON, rightErr := canonicalJSON(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func operationsKeyID(public ed25519.PublicKey) string {
	digest := sha256.Sum256(public)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validOperationsID(value string) bool {
	return objectIDPattern.MatchString(value) && !filepath.IsAbs(value) && value != "." && value != ".." && filepath.Clean(value) == value && !strings.HasPrefix(value, "../")
}

func loadOperationsPrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("operations private key must be a regular owner-only file")
	}
	var key OperationsPrivateKey
	if err := decodeStrictFile(path, &key); err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(key.PrivateKey)
	if err != nil || len(decoded) != ed25519.PrivateKeySize || key.SchemaVersion != 1 || key.Algorithm != operationsAlgorithm {
		return nil, fmt.Errorf("invalid operations private key")
	}
	private := ed25519.PrivateKey(decoded)
	if key.KeyID != operationsKeyID(private.Public().(ed25519.PublicKey)) {
		return nil, fmt.Errorf("operations private key ID mismatch")
	}
	return private, nil
}

func loadOperationsPublicKey(path string) (ed25519.PublicKey, string, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("operations public key must be a regular file")
	}
	var key OperationsPublicKey
	if err := decodeStrictFile(path, &key); err != nil {
		return nil, "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(key.PublicKey)
	if err != nil || len(decoded) != ed25519.PublicKeySize || key.SchemaVersion != 1 || key.Algorithm != operationsAlgorithm {
		return nil, "", fmt.Errorf("invalid operations public key")
	}
	public := ed25519.PublicKey(decoded)
	if key.KeyID != operationsKeyID(public) {
		return nil, "", fmt.Errorf("operations public key ID mismatch")
	}
	return public, key.KeyID, nil
}

func pathWithin(root, candidate string) (bool, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return false, err
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(root, resolvedCandidate)
	if err != nil {
		return false, err
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}
