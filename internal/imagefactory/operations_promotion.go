package imagefactory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/desktop"
)

// PromoteRelease verifies a complete candidate release group and returns the
// stable catalog plus signed receipt/alias objects. Callers write the immutable
// release directory first and update the alias files last.
func PromoteRelease(candidateCatalogPath, bundleManifestPath, bundleSignaturePath, bundlePublicKeyPath, lockPath, offlineRoot,
	evidenceRoot, promotionPlanPath, releasePrivateKeyPath, releasePublicKeyPath, currentAliasPath string, now time.Time) (*PromotionResult, error) {
	var plan PromotionPlan
	if err := decodeStrictFile(promotionPlanPath, &plan); err != nil {
		return nil, err
	}
	if err := validatePromotionPlan(&plan); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(evidenceRoot)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	bundles, promotedBundles, bundleRoots, err := verifyPromotionBundles(plan, root, bundleManifestPath, bundleSignaturePath,
		bundlePublicKeyPath, lockPath, offlineRoot, now)
	if err != nil {
		return nil, err
	}
	candidate, err := catalog.Load(candidateCatalogPath)
	if err != nil {
		return nil, err
	}
	candidateDigest, err := canonicalDigest(candidate)
	if err != nil || candidateDigest != plan.CandidateCatalogDigest {
		return nil, fmt.Errorf("candidate catalog digest does not match promotion plan")
	}
	if err := validatePromotionCatalog(candidate, plan, bundles); err != nil {
		return nil, err
	}
	if inside, err := pathWithin(root, releasePrivateKeyPath); err != nil || inside {
		return nil, fmt.Errorf("release signing private key must be outside the evidence root")
	}
	for _, bundleRoot := range bundleRoots {
		if inside, err := pathWithin(bundleRoot, releasePrivateKeyPath); err != nil || inside {
			return nil, fmt.Errorf("release signing private key must be outside every offline root")
		}
	}
	promotedEvidence, err := collectPromotionEvidence(plan, candidate, bundles, root)
	if err != nil {
		return nil, err
	}
	sort.Slice(promotedEvidence, func(i, j int) bool { return promotedEvidence[i].ImageID < promotedEvidence[j].ImageID })
	stable, err := copyCatalog(candidate)
	if err != nil {
		return nil, err
	}
	stable.Version++
	for index := range stable.Profiles {
		stable.Profiles[index].Channel = "stable"
	}
	for index := range stable.Repositories {
		stable.Repositories[index].Channel = "stable"
	}
	for index := range stable.Images {
		stable.Images[index].Channel = "stable"
	}
	for index := range stable.MirrorBundles {
		bundle := bundles[stable.MirrorBundles[index].ID]
		stable.MirrorBundles[index].Channel = "stable"
		stable.MirrorBundles[index].Digest = bundle.digest
		stable.MirrorBundles[index].CreatedAt = bundle.manifest.CreatedAt
		stable.MirrorBundles[index].FreshUntil = bundle.manifest.FreshUntil
		stable.MirrorBundles[index].AdvisoryWatermark = bundle.manifest.AdvisoryWatermark
	}
	for index := range stable.EgressPolicies {
		stable.EgressPolicies[index].Channel = "stable"
	}
	if err := stable.Validate(); err != nil {
		return nil, fmt.Errorf("promoted catalog is invalid: %w", err)
	}
	stableDigest, err := canonicalDigest(stable)
	if err != nil {
		return nil, err
	}
	previousReleaseID, revision, err := verifyCurrentAlias(plan, currentAliasPath, releasePublicKeyPath)
	if err != nil {
		return nil, err
	}
	primaryBundle := promotedBundles[0]
	receipt := &PromotionReceipt{SchemaVersion: 1, ReleaseID: plan.ReleaseID, Alias: plan.Alias, PreviousReleaseID: previousReleaseID, PromotedAt: now.UTC(),
		CatalogDigest: stableDigest, CandidateCatalogDigest: candidateDigest, BundleID: primaryBundle.BundleID, BundleManifestDigest: primaryBundle.BundleManifestDigest,
		BundleFreshUntil: primaryBundle.FreshUntil, AdvisoryWatermark: primaryBundle.AdvisoryWatermark, Bundles: promotedBundles, Images: promotedEvidence}
	receiptSignature, err := SignOperationsPayload("promotion-receipt", receipt, releasePrivateKeyPath)
	if err != nil {
		return nil, err
	}
	_, releaseKeyID, err := loadOperationsPublicKey(releasePublicKeyPath)
	if err != nil || receiptSignature.KeyID != releaseKeyID {
		return nil, fmt.Errorf("release signing private/public keys do not match")
	}
	receiptDigest, err := canonicalDigest(receipt)
	if err != nil {
		return nil, err
	}
	alias := &ReleaseAlias{SchemaVersion: 1, Alias: plan.Alias, Revision: revision + 1, ReleaseID: plan.ReleaseID, PreviousReleaseID: previousReleaseID,
		CatalogDigest: stableDigest, ReceiptDigest: receiptDigest, UpdatedAt: now.UTC(), Reason: "promotion"}
	aliasSignature, err := SignOperationsPayload("release-alias", alias, releasePrivateKeyPath)
	if err != nil {
		return nil, err
	}
	envelope := &SignedReleaseAlias{SchemaVersion: 1, Alias: *alias, Signature: *aliasSignature}
	return &PromotionResult{Catalog: stable, Receipt: receipt, ReceiptSignature: receiptSignature, Alias: alias, AliasSignature: aliasSignature, AliasEnvelope: envelope}, nil
}

func validatePromotionPlan(plan *PromotionPlan) error {
	legacyBundle := len(plan.Bundles) == 0
	if plan.SchemaVersion != 1 ||
		!validOperationsID(plan.ReleaseID) ||
		!validOperationsID(plan.Alias) ||
		plan.Alias == plan.ReleaseID ||
		!prefixedSHA256Pattern.MatchString(plan.CandidateCatalogDigest) ||
		(legacyBundle && !prefixedSHA256Pattern.MatchString(plan.BundleManifestDigest)) ||
		plan.MinimumFreshHours < 1 ||
		plan.MinimumFreshHours > 24*30 ||
		len(plan.Evidence) == 0 {
		return fmt.Errorf("promotion plan is incomplete")
	}
	if plan.ExpectedPreviousReleaseID != "" &&
		!validOperationsID(plan.ExpectedPreviousReleaseID) {
		return fmt.Errorf("promotion plan has an invalid previous release")
	}
	return nil
}

func validatePromotionCatalog(candidate *catalog.Catalog, plan PromotionPlan, bundles map[string]verifiedPromotionBundle) error {
	if len(candidate.Images) != len(plan.Evidence) || len(candidate.Profiles) != len(candidate.Images) {
		return fmt.Errorf("promotion requires evidence for every catalog image/profile")
	}
	for _, profile := range candidate.Profiles {
		if profile.Channel != "candidate" {
			return fmt.Errorf("profile %q is not a candidate", profile.ID)
		}
	}
	for _, repository := range candidate.Repositories {
		if repository.Channel != "candidate" {
			return fmt.Errorf("repository %q is not a candidate", repository.ID)
		}
	}
	for _, image := range candidate.Images {
		if image.Channel != "candidate" {
			return fmt.Errorf("image %q is not a candidate", image.ID)
		}
	}
	if len(candidate.MirrorBundles) != len(bundles) {
		return fmt.Errorf("candidate catalog does not contain exactly the verified bundle set")
	}
	for _, mirror := range candidate.MirrorBundles {
		verified, ok := bundles[mirror.ID]
		if !ok || mirror.Channel != "candidate" || mirror.Digest != verified.digest || !mirror.CreatedAt.Equal(verified.manifest.CreatedAt) ||
			!mirror.FreshUntil.Equal(verified.manifest.FreshUntil) || mirror.AdvisoryWatermark != verified.manifest.AdvisoryWatermark {
			return fmt.Errorf("candidate mirror bundle does not match the signed bundle manifest")
		}
	}
	return nil
}

func collectPromotionEvidence(plan PromotionPlan, candidate *catalog.Catalog, bundles map[string]verifiedPromotionBundle, root string) ([]PromotedImageEvidence, error) {
	imageByID := make(map[string]catalog.ImageManifest, len(candidate.Images))
	profileByID := make(map[string]catalog.ProfileDefinition, len(candidate.Profiles))
	for _, image := range candidate.Images {
		imageByID[image.ID] = image
	}
	for _, profile := range candidate.Profiles {
		profileByID[profile.ID] = profile
	}
	seen := make(map[string]struct{}, len(plan.Evidence))
	result := make([]PromotedImageEvidence, 0, len(plan.Evidence))
	for _, reference := range plan.Evidence {
		if !validOperationsID(reference.ImageID) {
			return nil, fmt.Errorf("invalid promotion evidence image ID")
		}
		if _, duplicate := seen[reference.ImageID]; duplicate {
			return nil, fmt.Errorf("duplicate promotion evidence for %q", reference.ImageID)
		}
		seen[reference.ImageID] = struct{}{}
		record, err := validatePromotionImageEvidence(reference, candidate, bundles, imageByID, profileByID, root)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

func validatePromotionImageEvidence(reference PromotionEvidenceRef, candidate *catalog.Catalog,
	bundles map[string]verifiedPromotionBundle, imageByID map[string]catalog.ImageManifest,
	profileByID map[string]catalog.ProfileDefinition, root string) (PromotedImageEvidence, error) {
	catalogImage, exists := imageByID[reference.ImageID]
	if !exists {
		return PromotedImageEvidence{}, fmt.Errorf("promotion evidence references unknown image %q", reference.ImageID)
	}
	manifestPath, err := confinedEvidencePath(root, reference.ImageManifest)
	if err != nil {
		return PromotedImageEvidence{}, err
	}
	smokePath, err := confinedEvidencePath(root, reference.SmokeResult)
	if err != nil {
		return PromotedImageEvidence{}, err
	}
	stampPath, err := confinedEvidencePath(root, reference.OutputStamp)
	if err != nil {
		return PromotedImageEvidence{}, err
	}
	manifest, err := LoadImageManifest(manifestPath)
	if err != nil {
		return PromotedImageEvidence{}, fmt.Errorf("load image evidence %q: %w", reference.ImageID, err)
	}
	profile, profileExists := profileByID[catalogImage.ProfileID]
	verifiedBundle, bundleExists := bundles[manifest.MirrorBundleID]
	if !profileExists || !bundleExists || !promotionManifestMatchesCatalog(manifest, catalogImage, profile, verifiedBundle, candidate.Repositories) {
		return PromotedImageEvidence{}, fmt.Errorf("image manifest %q does not match the candidate catalog", reference.ImageID)
	}
	smoke, stamp, manifestDigest, err := loadPromotionSmokeAndStamp(reference.ImageID, manifestPath, smokePath, stampPath, manifest)
	if err != nil {
		return PromotedImageEvidence{}, err
	}
	smokeDigest, err := digestFile(smokePath)
	if err != nil {
		return PromotedImageEvidence{}, err
	}
	stampDigest, err := digestFile(stampPath)
	if err != nil {
		return PromotedImageEvidence{}, err
	}
	desktopDigest, err := promotionDesktopDigest(reference, manifest, root, stamp.StampedAt)
	if err != nil {
		return PromotedImageEvidence{}, err
	}
	return PromotedImageEvidence{
		ImageID: reference.ImageID, ImageManifestDigest: "sha256:" + manifestDigest,
		SmokeResultDigest: "sha256:" + smokeDigest, OutputStampDigest: "sha256:" + stampDigest,
		DesktopResultDigest: desktopDigest, CompletedAt: smoke.CompletedAt,
	}, nil
}

func promotionManifestMatchesCatalog(manifest *ImageManifest, image catalog.ImageManifest, profile catalog.ProfileDefinition,
	bundle verifiedPromotionBundle, repositories []catalog.RepositoryDefinition) bool {
	return profile.ImageID == image.ID && manifest.ImageID == image.ID && manifest.ProfileID == image.ProfileID &&
		manifest.Generation == image.Generation && manifest.Template == image.Template && manifest.ImageDigest == image.Digest &&
		manifest.Provider == image.Provider && manifest.Arch == image.Arch && manifest.BuildMode == image.BuildMode &&
		manifest.RootfsSource == image.RootfsSource && manifest.DisplayModel == image.DisplayModel &&
		manifest.RootfsManifestDigest == image.RootfsManifestDigest && slices.Equal(manifest.PackageSets, image.PackageSetIDs) &&
		manifest.PackageSetCatalogDigest == image.PackageSetCatalogDigest && profile.MirrorBundleID == manifest.MirrorBundleID &&
		manifest.InputLockDigest == bundle.manifest.InputLockDigest && manifest.ProfilePath == profile.ProfilePath &&
		manifestProfileRepositoryMatches(manifest.ProfileRepository, profile.ProfileRepositoryID, repositories) &&
		manifestParentsMatchProfile(manifest.ProfileParents, profile.Parents, repositories) &&
		manifestRepositoriesMatchProfile(manifest.Repositories, profile.RepositoryIDs, repositories)
}

func loadPromotionSmokeAndStamp(imageID, manifestPath, smokePath, stampPath string, manifest *ImageManifest) (SmokeResult, OutputStampEvidence, string, error) {
	var smoke SmokeResult
	if err := decodeStrictFile(smokePath, &smoke); err != nil {
		return smoke, OutputStampEvidence{}, "", err
	}
	if smoke.SchemaVersion != 1 || smoke.Target != manifest.Target || smoke.CandidateManifest != filepath.Base(manifestPath) ||
		smoke.InstanceName == "" || smoke.VMID == "" || smoke.Node == "" || smoke.GuestIP == "" || smoke.CloudInitRuns < 2 ||
		!smoke.TerraformDestroyRequired || !smoke.TerraformDestroyed || !smoke.OutputProvenanceStamped ||
		smoke.CompletedAt.IsZero() || smoke.CompletedAt.Before(manifest.CreatedAt) {
		return smoke, OutputStampEvidence{}, "", fmt.Errorf("smoke evidence for %q is incomplete", imageID)
	}
	var stamp OutputStampEvidence
	if err := decodeStrictFile(stampPath, &stamp); err != nil {
		return smoke, stamp, "", err
	}
	manifestDigest, err := digestFile(manifestPath)
	if err != nil {
		return smoke, stamp, "", err
	}
	if stamp.SchemaVersion != 1 || stamp.StampedAt.IsZero() || stamp.StampedAt.Before(smoke.CompletedAt) ||
		stamp.Template != manifest.Template || stamp.ImageDigest != manifest.ImageDigest ||
		stamp.ManifestDigest != "sha256:"+manifestDigest || !stamp.Verified {
		return smoke, stamp, "", fmt.Errorf("output stamp for %q does not bind the image manifest", imageID)
	}
	return smoke, stamp, manifestDigest, nil
}

func promotionDesktopDigest(reference PromotionEvidenceRef, manifest *ImageManifest, root string, stampedAt time.Time) (string, error) {
	if manifest.Target != "desktop-verifier" {
		if reference.DesktopResult != "" || reference.DesktopScenarioID != "" {
			return "", fmt.Errorf("non-desktop image %q contains desktop promotion evidence", reference.ImageID)
		}
		return "", nil
	}
	if reference.DesktopResult == "" || reference.DesktopScenarioID == "" {
		return "", fmt.Errorf("desktop promotion evidence for %q is incomplete", reference.ImageID)
	}
	desktopPath, err := confinedEvidencePath(root, reference.DesktopResult)
	if err != nil {
		return "", err
	}
	if err := validateDesktopPromotionEvidence(desktopPath, manifest, reference.DesktopScenarioID, stampedAt); err != nil {
		return "", fmt.Errorf("desktop evidence for %q: %w", reference.ImageID, err)
	}
	digest, err := digestFile(desktopPath)
	if err != nil {
		return "", err
	}
	return "sha256:" + digest, nil
}

func validateDesktopPromotionEvidence(path string, manifest *ImageManifest, scenarioID string, stampedAt time.Time) error {
	var result desktop.Result
	if err := decodeStrictFile(path, &result); err != nil {
		return err
	}
	if result.SchemaVersion != 1 || result.State != "passed" || result.ImageID != manifest.ImageID || result.ProfileID != manifest.ProfileID ||
		result.ScenarioID != scenarioID || result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) || result.StartedAt.Before(stampedAt) || len(result.Steps) == 0 {
		return fmt.Errorf("desktop result identity, state, or timing is invalid")
	}
	required := map[string]bool{"restore": false, "start": false, "screenshot": false, "collect_accessibility": false, "stop": false}
	for _, step := range result.Steps {
		if step.State != "passed" || step.StartedAt.IsZero() || step.Duration < 0 {
			return fmt.Errorf("desktop step %q did not pass", step.ID)
		}
		if _, ok := required[step.Action]; ok {
			required[step.Action] = true
		}
		if (step.Action == "screenshot" || step.Action == "collect_accessibility") && len(step.Artifacts) == 0 {
			return fmt.Errorf("desktop evidence step %q has no artifact", step.ID)
		}
	}
	for action, present := range required {
		if !present {
			return fmt.Errorf("desktop result lacks required %q evidence step", action)
		}
	}
	return nil
}

type verifiedPromotionBundle struct {
	manifest *BundleManifest
	digest   string
}

func verifyPromotionBundles(plan PromotionPlan, evidenceRoot, legacyManifest, legacySignature, legacyPublicKey, legacyLock, legacyOfflineRoot string,
	now time.Time) (map[string]verifiedPromotionBundle, []PromotedBundleEvidence, []string, error) {
	verified := make(map[string]verifiedPromotionBundle)
	roots := make([]string, 0, max(1, len(plan.Bundles)))
	add := func(expectedID, manifestPath, signaturePath, publicKeyPath, lockPath, offlineRoot string) error {
		if manifestPath == "" || signaturePath == "" || publicKeyPath == "" || lockPath == "" || offlineRoot == "" {
			return fmt.Errorf("promotion bundle reference is incomplete")
		}
		manifest, err := VerifyBundle(manifestPath, signaturePath, publicKeyPath, lockPath, offlineRoot, now)
		if err != nil {
			return fmt.Errorf("verify promotion bundle: %w", err)
		}
		if expectedID != "" && manifest.BundleID != expectedID {
			return fmt.Errorf("promotion bundle ID %q does not match manifest %q", expectedID, manifest.BundleID)
		}
		if _, duplicate := verified[manifest.BundleID]; duplicate {
			return fmt.Errorf("duplicate promotion bundle %q", manifest.BundleID)
		}
		if manifest.FreshUntil.Sub(now) < time.Duration(plan.MinimumFreshHours)*time.Hour {
			return fmt.Errorf("bundle %q freshness remaining is below the promotion minimum", manifest.BundleID)
		}
		digest, err := canonicalDigest(manifest)
		if err != nil {
			return err
		}
		verified[manifest.BundleID] = verifiedPromotionBundle{manifest: manifest, digest: digest}
		roots = append(roots, offlineRoot)
		return nil
	}
	if len(plan.Bundles) == 0 {
		if err := add("", legacyManifest, legacySignature, legacyPublicKey, legacyLock, legacyOfflineRoot); err != nil {
			return nil, nil, nil, err
		}
		for _, bundle := range verified {
			if bundle.digest != plan.BundleManifestDigest {
				return nil, nil, nil, fmt.Errorf("bundle manifest digest does not match promotion plan")
			}
		}
	} else {
		if legacyPublicKey == "" {
			return nil, nil, nil, fmt.Errorf("multi-bundle promotion requires a controller-trusted sync public key")
		}
		for _, reference := range plan.Bundles {
			if !validOperationsID(reference.BundleID) {
				return nil, nil, nil, fmt.Errorf("promotion bundle has invalid ID %q", reference.BundleID)
			}
			manifestPath, err := confinedEvidencePath(evidenceRoot, reference.Manifest)
			if err != nil {
				return nil, nil, nil, err
			}
			signaturePath, err := confinedEvidencePath(evidenceRoot, reference.Signature)
			if err != nil {
				return nil, nil, nil, err
			}
			lockPath, err := confinedEvidencePath(evidenceRoot, reference.InputLock)
			if err != nil {
				return nil, nil, nil, err
			}
			offlineRoot, err := confinedEvidenceDirectory(evidenceRoot, reference.OfflineRoot)
			if err != nil {
				return nil, nil, nil, err
			}
			if err := add(reference.BundleID, manifestPath, signaturePath, legacyPublicKey, lockPath, offlineRoot); err != nil {
				return nil, nil, nil, err
			}
		}
	}
	promoted := make([]PromotedBundleEvidence, 0, len(verified))
	for _, bundle := range verified {
		promoted = append(promoted, PromotedBundleEvidence{BundleID: bundle.manifest.BundleID, BundleManifestDigest: bundle.digest,
			InputLockDigest: bundle.manifest.InputLockDigest, FreshUntil: bundle.manifest.FreshUntil, AdvisoryWatermark: bundle.manifest.AdvisoryWatermark})
	}
	sort.Slice(promoted, func(i, j int) bool { return promoted[i].BundleID < promoted[j].BundleID })
	return verified, promoted, roots, nil
}

func manifestProfileRepositoryMatches(manifestName, profileRepositoryID string, repositories []catalog.RepositoryDefinition) bool {
	for _, repository := range repositories {
		if repository.ID == profileRepositoryID {
			return repository.Name == manifestName
		}
	}
	return false
}

func manifestParentsMatchProfile(manifest []CatalystProfileParent, profile []catalog.ProfileParentDefinition, repositories []catalog.RepositoryDefinition) bool {
	if len(manifest) != len(profile) {
		return false
	}
	for index := range manifest {
		if manifest[index].ProfilePath != profile[index].ProfilePath {
			return false
		}
		matched := false
		for _, repository := range repositories {
			if repository.ID == profile[index].RepositoryID && repository.Name == manifest[index].Repository {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func manifestRepositoriesMatchProfile(manifest map[string]string, profileRepositoryIDs []string, repositories []catalog.RepositoryDefinition) bool {
	if len(manifest) != len(profileRepositoryIDs) {
		return false
	}
	repositoryByID := make(map[string]catalog.RepositoryDefinition, len(repositories))
	for _, repository := range repositories {
		repositoryByID[repository.ID] = repository
	}
	seenNames := make(map[string]struct{}, len(profileRepositoryIDs))
	for _, repositoryID := range profileRepositoryIDs {
		repository, ok := repositoryByID[repositoryID]
		if !ok || manifest[repository.Name] != repository.Revision {
			return false
		}
		if _, duplicate := seenNames[repository.Name]; duplicate {
			return false
		}
		seenNames[repository.Name] = struct{}{}
	}
	return true
}

func RollbackRelease(currentAliasPath, targetReceiptPath, targetReceiptSignaturePath, targetCatalogPath,
	releasePrivateKeyPath, releasePublicKeyPath, reason string, now time.Time) (*SignedReleaseAlias, error) {
	if reason == "" || len(reason) > 512 {
		return nil, fmt.Errorf("rollback requires a bounded reason")
	}
	var currentEnvelope SignedReleaseAlias
	if err := decodeStrictFile(currentAliasPath, &currentEnvelope); err != nil {
		return nil, err
	}
	if currentEnvelope.SchemaVersion != 1 || currentEnvelope.Alias.SchemaVersion != 1 {
		return nil, fmt.Errorf("current alias envelope is invalid")
	}
	current := currentEnvelope.Alias
	if !validReleaseAlias(&current) {
		return nil, fmt.Errorf("current release alias is invalid")
	}
	if err := VerifyOperationsDetached("release-alias", &current, &currentEnvelope.Signature, releasePublicKeyPath); err != nil {
		return nil, err
	}
	var targetReceipt PromotionReceipt
	if err := decodeStrictFile(targetReceiptPath, &targetReceipt); err != nil {
		return nil, err
	}
	if err := VerifyOperationsPayload("promotion-receipt", &targetReceipt, targetReceiptSignaturePath, releasePublicKeyPath); err != nil {
		return nil, err
	}
	if !validPromotionReceipt(&targetReceipt) {
		return nil, fmt.Errorf("rollback target receipt is invalid")
	}
	targetCatalog, err := catalog.Load(targetCatalogPath)
	if err != nil {
		return nil, err
	}
	targetCatalogDigest, err := canonicalDigest(targetCatalog)
	if err != nil || targetCatalogDigest != targetReceipt.CatalogDigest || targetReceipt.ReleaseID == current.ReleaseID || targetReceipt.Alias != current.Alias {
		return nil, fmt.Errorf("rollback target does not match its signed receipt or current alias")
	}
	receiptBundles := targetReceipt.Bundles
	if len(receiptBundles) == 0 {
		receiptBundles = []PromotedBundleEvidence{{BundleID: targetReceipt.BundleID, BundleManifestDigest: targetReceipt.BundleManifestDigest,
			FreshUntil: targetReceipt.BundleFreshUntil, AdvisoryWatermark: targetReceipt.AdvisoryWatermark}}
	}
	if len(receiptBundles) != len(targetCatalog.MirrorBundles) {
		return nil, fmt.Errorf("rollback target catalog bundle set does not match the signed receipt")
	}
	for _, receiptBundle := range receiptBundles {
		if !now.Before(receiptBundle.FreshUntil) {
			return nil, fmt.Errorf("rollback target bundle %q is stale", receiptBundle.BundleID)
		}
		bundleMatches := false
		for _, bundle := range targetCatalog.MirrorBundles {
			if bundle.ID == receiptBundle.BundleID && bundle.Digest == receiptBundle.BundleManifestDigest && bundle.FreshUntil.Equal(receiptBundle.FreshUntil) &&
				bundle.AdvisoryWatermark == receiptBundle.AdvisoryWatermark && bundle.Channel == "stable" {
				bundleMatches = true
			}
		}
		if !bundleMatches {
			return nil, fmt.Errorf("rollback target catalog bundle %q does not match the signed receipt", receiptBundle.BundleID)
		}
	}
	targetReceiptDigest, err := canonicalDigest(&targetReceipt)
	if err != nil {
		return nil, err
	}
	alias := &ReleaseAlias{SchemaVersion: 1, Alias: current.Alias, Revision: current.Revision + 1, ReleaseID: targetReceipt.ReleaseID,
		PreviousReleaseID: current.ReleaseID, CatalogDigest: targetReceipt.CatalogDigest, ReceiptDigest: targetReceiptDigest, UpdatedAt: now.UTC(), Reason: "rollback: " + reason}
	signature, err := SignOperationsPayload("release-alias", alias, releasePrivateKeyPath)
	if err != nil {
		return nil, err
	}
	_, publicKeyID, err := loadOperationsPublicKey(releasePublicKeyPath)
	if err != nil || signature.KeyID != publicKeyID {
		return nil, fmt.Errorf("release signing private/public keys do not match")
	}
	return &SignedReleaseAlias{SchemaVersion: 1, Alias: *alias, Signature: *signature}, nil
}

func verifyCurrentAlias(plan PromotionPlan, aliasPath, publicKeyPath string) (string, int, error) {
	if aliasPath == "" {
		if plan.ExpectedPreviousReleaseID != "" {
			return "", 0, fmt.Errorf("promotion expected a previous release but no alias was supplied")
		}
		return "", 0, nil
	}
	var envelope SignedReleaseAlias
	if err := decodeStrictFile(aliasPath, &envelope); err != nil {
		return "", 0, err
	}
	if envelope.SchemaVersion != 1 || envelope.Alias.SchemaVersion != 1 {
		return "", 0, fmt.Errorf("current alias envelope is invalid")
	}
	current := envelope.Alias
	if !validReleaseAlias(&current) {
		return "", 0, fmt.Errorf("current alias is invalid")
	}
	if err := VerifyOperationsDetached("release-alias", &current, &envelope.Signature, publicKeyPath); err != nil {
		return "", 0, err
	}
	if current.SchemaVersion != 1 || current.Alias != plan.Alias || current.ReleaseID != plan.ExpectedPreviousReleaseID || current.Revision < 1 {
		return "", 0, fmt.Errorf("current alias does not match promotion compare-and-swap expectation")
	}
	return current.ReleaseID, current.Revision, nil
}

func validReleaseAlias(alias *ReleaseAlias) bool {
	return alias != nil && alias.SchemaVersion == 1 && validOperationsID(alias.Alias) && validOperationsID(alias.ReleaseID) && alias.Revision >= 1 &&
		prefixedSHA256Pattern.MatchString(alias.CatalogDigest) && prefixedSHA256Pattern.MatchString(alias.ReceiptDigest) && !alias.UpdatedAt.IsZero() && alias.Reason != ""
}

func validPromotionReceipt(receipt *PromotionReceipt) bool {
	if receipt == nil || receipt.SchemaVersion != 1 || !validOperationsID(receipt.ReleaseID) || !validOperationsID(receipt.Alias) || receipt.PromotedAt.IsZero() ||
		!prefixedSHA256Pattern.MatchString(receipt.CatalogDigest) || !prefixedSHA256Pattern.MatchString(receipt.CandidateCatalogDigest) || !validOperationsID(receipt.BundleID) ||
		!prefixedSHA256Pattern.MatchString(receipt.BundleManifestDigest) || !receipt.BundleFreshUntil.After(receipt.PromotedAt) || receipt.AdvisoryWatermark == "" || len(receipt.Images) == 0 {
		return false
	}
	seen := map[string]struct{}{}
	primaryFound := len(receipt.Bundles) == 0
	for _, bundle := range receipt.Bundles {
		if !validOperationsID(bundle.BundleID) || !prefixedSHA256Pattern.MatchString(bundle.BundleManifestDigest) || !prefixedSHA256Pattern.MatchString(bundle.InputLockDigest) ||
			!bundle.FreshUntil.After(receipt.PromotedAt) || bundle.AdvisoryWatermark == "" {
			return false
		}
		if _, duplicate := seen[bundle.BundleID]; duplicate {
			return false
		}
		seen[bundle.BundleID] = struct{}{}
		if bundle.BundleID == receipt.BundleID && bundle.BundleManifestDigest == receipt.BundleManifestDigest && bundle.FreshUntil.Equal(receipt.BundleFreshUntil) && bundle.AdvisoryWatermark == receipt.AdvisoryWatermark {
			primaryFound = true
		}
	}
	return primaryFound
}

func confinedEvidencePath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe promotion evidence path %q", relative)
	}
	path := filepath.Join(root, relative)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("promotion evidence path escapes evidence root")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("promotion evidence path is not a regular file")
	}
	return resolved, nil
}

func confinedEvidenceDirectory(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe promotion evidence directory %q", relative)
	}
	path := filepath.Join(root, relative)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("promotion evidence directory escapes evidence root")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("promotion evidence directory is not a directory")
	}
	return resolved, nil
}

func copyCatalog(source *catalog.Catalog) (*catalog.Catalog, error) {
	payload, err := canonicalJSON(source)
	if err != nil {
		return nil, err
	}
	var copied catalog.Catalog
	if err := jsonUnmarshalStrictBytes(payload, &copied); err != nil {
		return nil, err
	}
	return &copied, nil
}

func jsonUnmarshalStrictBytes(payload []byte, destination any) error {
	return json.Unmarshal(payload, destination)
}
