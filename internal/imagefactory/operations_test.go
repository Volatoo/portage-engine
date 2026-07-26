package imagefactory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/desktop"
)

const operationsTestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func writeOperationsKeys(t *testing.T, dir, prefix string) (string, string) {
	t.Helper()
	privateKey, publicKey, err := NewOperationsKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	privatePath := writeTestJSON(t, dir, prefix+"-private.json", privateKey)
	publicPath := writeTestJSON(t, dir, prefix+"-public.json", publicKey)
	if err := os.Chmod(privatePath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(publicPath, 0o644); err != nil {
		t.Fatal(err)
	}
	return privatePath, publicPath
}

func TestDesktopPromotionEvidenceIsARequiredDeterministicGate(t *testing.T) {
	stampedAt := time.Date(2026, 7, 24, 1, 0, 0, 0, time.UTC)
	manifest := &ImageManifest{ImageID: "pe/amd64/desktop-g2", ProfileID: "pe/amd64/desktop-v1"}
	result := desktop.Result{
		SchemaVersion: 1, ScenarioID: "desktop/image-baseline-v1", ProfileID: manifest.ProfileID, ImageID: manifest.ImageID,
		State: "passed", StartedAt: stampedAt.Add(time.Minute), CompletedAt: stampedAt.Add(2 * time.Minute),
		Steps: []desktop.StepResult{
			{ID: "restore", Action: "restore", State: "passed", StartedAt: stampedAt.Add(time.Minute)},
			{ID: "start", Action: "start", State: "passed", StartedAt: stampedAt.Add(time.Minute)},
			{ID: "a11y", Action: "collect_accessibility", State: "passed", StartedAt: stampedAt.Add(time.Minute), Artifacts: []string{"tree.json"}},
			{ID: "screenshot", Action: "screenshot", State: "passed", StartedAt: stampedAt.Add(time.Minute), Artifacts: []string{"screen.png"}},
			{ID: "stop", Action: "stop", State: "passed", StartedAt: stampedAt.Add(time.Minute)},
		},
	}
	path := writeTestJSON(t, t.TempDir(), "desktop-result.json", result)
	if err := validateDesktopPromotionEvidence(path, manifest, result.ScenarioID, stampedAt); err != nil {
		t.Fatal(err)
	}
	result.Steps[3].State = "failed"
	path = writeTestJSON(t, t.TempDir(), "failed-desktop-result.json", result)
	if err := validateDesktopPromotionEvidence(path, manifest, result.ScenarioID, stampedAt); err == nil {
		t.Fatal("promotion accepted a failed screenshot step")
	}
	result.Steps[3].State = "passed"
	result.ImageID = "pe/amd64/other"
	path = writeTestJSON(t, t.TempDir(), "drifted-desktop-result.json", result)
	if err := validateDesktopPromotionEvidence(path, manifest, result.ScenarioID, stampedAt); err == nil {
		t.Fatal("promotion accepted desktop evidence for another image")
	}
}

func prepareSignedBundle(t *testing.T, now time.Time) (string, string, string, string, string, *BundleManifest) {
	t.Helper()
	dir := t.TempDir()
	objectPath := filepath.Join(dir, "payload.bin")
	if err := os.WriteFile(objectPath, []byte("locked payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	object := objectForFile(t, "payload/test", "script", objectPath, nil)
	lock := InputLock{Version: 1, BundleID: "mirror/test-release", StrictOffline: true, AllowedHosts: []string{"mirror.internal"},
		AdvisoryCutoff: now.Add(-time.Hour).UTC().Format(time.RFC3339), Objects: []InputObject{object}}
	lockPath := writeTestJSON(t, dir, "inputs.lock.json", lock)
	privatePath, publicPath := writeOperationsKeys(t, t.TempDir(), "bundle")
	manifest, signature, err := SealBundle(lockPath, dir, privatePath, now, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := writeTestJSON(t, dir, "bundle-manifest.json", manifest)
	signaturePath := writeTestJSON(t, dir, "bundle-manifest.sig.json", signature)
	return dir, lockPath, manifestPath, signaturePath, publicPath, manifest
}

func TestSealAndVerifyBundleRejectsExpiryAndDrift(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	dir, lockPath, manifestPath, signaturePath, publicPath, manifest := prepareSignedBundle(t, now)
	if _, err := VerifyBundle(manifestPath, signaturePath, publicPath, lockPath, dir, now.Add(-time.Minute)); err == nil || !strings.Contains(err.Error(), "future") {
		t.Fatalf("future-dated bundle was accepted: %v", err)
	}
	if _, err := VerifyBundle(manifestPath, signaturePath, publicPath, lockPath, dir, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(manifestPath, signaturePath, publicPath, lockPath, dir, manifest.FreshUntil); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired bundle was accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload.bin"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(manifestPath, signaturePath, publicPath, lockPath, dir, now.Add(time.Hour)); err == nil {
		t.Fatal("tampered bundle object was accepted")
	}
}

func TestPromotionBundleSetVerifiesIndependentInputLocks(t *testing.T) {
	now := time.Date(2026, 7, 23, 4, 0, 0, 0, time.UTC)
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	syncPrivate, syncPublic := writeOperationsKeys(t, t.TempDir(), "sync")
	refs := make([]PromotionBundleRef, 0, 2)
	for _, id := range []string{"mirror/base-g2", "mirror/desktop-g1"} {
		name := strings.ReplaceAll(id, "/", "-")
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		payload := filepath.Join(dir, "payload.bin")
		if err := os.WriteFile(payload, []byte(id), 0o600); err != nil {
			t.Fatal(err)
		}
		object := objectForFile(t, "payload/"+name, "script", payload, nil)
		lock := InputLock{Version: 1, BundleID: id, StrictOffline: true, AllowedHosts: []string{"mirror.internal"},
			AdvisoryCutoff: now.Add(-time.Hour).Format(time.RFC3339), Objects: []InputObject{object}}
		lockPath := writeTestJSON(t, dir, "inputs.lock.json", lock)
		manifest, signature, err := SealBundle(lockPath, dir, syncPrivate, now.Add(-time.Minute), 72*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		writeTestJSON(t, dir, "bundle-manifest.json", manifest)
		writeTestJSON(t, dir, "bundle-manifest.sig.json", signature)
		refs = append(refs, PromotionBundleRef{BundleID: id, Manifest: name + "/bundle-manifest.json", Signature: name + "/bundle-manifest.sig.json",
			InputLock: name + "/inputs.lock.json", OfflineRoot: name})
	}
	plan := PromotionPlan{MinimumFreshHours: 24, Bundles: refs}
	verified, evidence, roots, err := verifyPromotionBundles(plan, root, "", "", syncPublic, "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(verified) != 2 || len(evidence) != 2 || len(roots) != 2 || evidence[0].BundleID != "mirror/base-g2" || evidence[1].BundleID != "mirror/desktop-g1" {
		t.Fatalf("unexpected verified bundle set: %+v", evidence)
	}
}

func TestPromotionAndRollbackBindAllEvidence(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	dir, lockPath, bundleManifestPath, bundleSignaturePath, bundlePublicPath, bundle := prepareSignedBundle(t, now.Add(-2*time.Hour))
	bundleDigest, err := CanonicalDigest(bundle)
	if err != nil {
		t.Fatal(err)
	}
	releasePrivatePath, releasePublicPath := writeOperationsKeys(t, t.TempDir(), "release")
	profileID := "pe/amd64/base-v1"
	imageID := "image/base-g1"
	image := ImageManifest{SchemaVersion: 1, CreatedAt: now.Add(-90 * time.Minute), Target: "base-systemd", ImageID: imageID, Generation: "g1",
		Provider: "pve", Arch: "amd64", BuildMode: "native-gentoo", SourceTemplate: "seed", SourceVMID: 9000,
		SourceProvenanceObjectID: "seed/test", SourceProvenanceDigest: operationsTestDigest, Template: "pe-base-g1", ProfileID: profileID,
		ProfilePath: "portage-engine/amd64/23.0/systemd/base", ProfileRepository: "pe-profiles",
		ProfileParents: []CatalystProfileParent{{Repository: "gentoo", ProfilePath: catalystOfficialProfile}},
		PackageSets:    []string{"pe/runtime-v1"}, PackageSetCatalogDigest: operationsTestDigest, MirrorBundleID: bundle.BundleID,
		Repositories: map[string]string{"gentoo": strings.Repeat("1", 40), "pe-profiles": strings.Repeat("2", 40)}, RootfsSource: "catalyst",
		Channel: "candidate", InputLockDigest: bundle.InputLockDigest, CommonConfigDigest: operationsTestDigest, BuildPlanDigest: operationsTestDigest,
		PackerManifestDigest: operationsTestDigest, PackerArtifactID: "9001", ImageDigest: operationsTestDigest, RootfsManifestDigest: operationsTestDigest}
	imagePath := writeTestJSON(t, dir, "base.image-manifest.json", image)
	imageFileDigest, err := digestFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	smoke := SmokeResult{SchemaVersion: 1, Target: image.Target, CandidateManifest: filepath.Base(imagePath), InstanceName: "smoke-base", VMID: "9100", Node: "pve01", GuestIP: "192.0.2.10",
		CloudInitRuns: 2, TerraformDestroyRequired: true, TerraformDestroyed: true, OutputProvenanceStamped: true, CompletedAt: now.Add(-time.Hour)}
	smokePath := writeTestJSON(t, dir, "smoke-result.json", smoke)
	stamp := OutputStampEvidence{SchemaVersion: 1, StampedAt: now.Add(-50 * time.Minute), Template: image.Template, VMID: 9001, Node: "pve01",
		ManifestDigest: "sha256:" + imageFileDigest, ImageDigest: image.ImageDigest, Verified: true}
	stampPath := writeTestJSON(t, dir, "output-stamp.json", stamp)
	candidate := &catalog.Catalog{Version: 1,
		Profiles: []catalog.ProfileDefinition{{ID: profileID, Arch: "amd64", ProfilePath: image.ProfilePath, BinhostPath: "releases/amd64/binpackages/23.0/x86-64_test", ProfileRepositoryID: "pe-profiles/rev-" + image.Repositories["pe-profiles"],
			Parents: []catalog.ProfileParentDefinition{{RepositoryID: "gentoo/rev-" + image.Repositories["gentoo"], ProfilePath: catalystOfficialProfile}}, RepositoryIDs: []string{"gentoo/rev-" + image.Repositories["gentoo"], "pe-profiles/rev-" + image.Repositories["pe-profiles"]},
			ImageID: imageID, MirrorBundleID: bundle.BundleID, Default: true, Channel: "candidate"}},
		Repositories: []catalog.RepositoryDefinition{
			{ID: "gentoo/rev-" + image.Repositories["gentoo"], Name: "gentoo", Location: "/var/db/repos/gentoo", SyncType: "git", SyncURI: "https://git.internal/gentoo.git", Revision: image.Repositories["gentoo"], Channel: "candidate"},
			{ID: "pe-profiles/rev-" + image.Repositories["pe-profiles"], Name: "pe-profiles", Location: "/var/db/repos/pe-profiles", SyncType: "git", SyncURI: "https://git.internal/pe-profiles.git", Revision: image.Repositories["pe-profiles"], Channel: "candidate"}},
		Images: []catalog.ImageManifest{{ID: imageID, ProfileID: profileID, Generation: image.Generation, Provider: image.Provider, Arch: image.Arch,
			BuildMode: image.BuildMode, Template: image.Template, Digest: image.ImageDigest, RootfsSource: image.RootfsSource, RootfsManifestDigest: image.RootfsManifestDigest,
			PackageSetIDs: append([]string(nil), image.PackageSets...), PackageSetCatalogDigest: image.PackageSetCatalogDigest, Channel: "candidate"}},
		MirrorBundles: []catalog.MirrorBundle{{ID: bundle.BundleID, Digest: bundleDigest, CreatedAt: bundle.CreatedAt, FreshUntil: bundle.FreshUntil, AdvisoryWatermark: bundle.AdvisoryWatermark, Channel: "candidate"}}}
	if err := candidate.Validate(); err != nil {
		t.Fatal(err)
	}
	candidatePath := writeTestJSON(t, dir, "candidate-catalog.json", candidate)
	candidateDigest, err := CanonicalDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	promotionPlan := PromotionPlan{SchemaVersion: 1, ReleaseID: "release-2026-07-22-g1", Alias: "stable", CandidateCatalogDigest: candidateDigest,
		BundleManifestDigest: bundleDigest, MinimumFreshHours: 24, Evidence: []PromotionEvidenceRef{{ImageID: imageID, ImageManifest: filepath.Base(imagePath), SmokeResult: filepath.Base(smokePath), OutputStamp: filepath.Base(stampPath)}}}
	planPath := writeTestJSON(t, dir, "promotion-plan.json", promotionPlan)
	result, err := PromoteRelease(candidatePath, bundleManifestPath, bundleSignaturePath, bundlePublicPath, lockPath, dir, dir, planPath,
		releasePrivatePath, releasePublicPath, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Catalog.Images[0].Channel != "stable" || result.Alias.Revision != 1 || result.Receipt.BundleManifestDigest != bundleDigest {
		t.Fatalf("unexpected promotion result: %+v", result)
	}
	if err := VerifyOperationsDetached("release-alias", &result.AliasEnvelope.Alias, &result.AliasEnvelope.Signature, releasePublicPath); err != nil {
		t.Fatal(err)
	}
	promotionPlan.BundleManifestDigest = ""
	promotionPlan.Bundles = []PromotionBundleRef{{BundleID: bundle.BundleID, Manifest: filepath.Base(bundleManifestPath), Signature: filepath.Base(bundleSignaturePath),
		InputLock: filepath.Base(lockPath), OfflineRoot: "."}}
	planPath = writeTestJSON(t, dir, "promotion-plan-multi.json", promotionPlan)
	result, err = PromoteRelease(candidatePath, "", "", bundlePublicPath, "", "", dir, planPath,
		releasePrivatePath, releasePublicPath, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Receipt.Bundles) != 1 || result.Receipt.Bundles[0].InputLockDigest != bundle.InputLockDigest {
		t.Fatalf("multi-bundle receipt did not bind the input lock: %+v", result.Receipt.Bundles)
	}
	smoke.TerraformDestroyed = false
	_ = writeTestJSON(t, dir, filepath.Base(smokePath), smoke)
	if _, err := PromoteRelease(candidatePath, "", "", bundlePublicPath, "", "", dir, planPath,
		releasePrivatePath, releasePublicPath, "", now); err == nil {
		t.Fatal("promotion accepted smoke evidence without successful destroy")
	}
	stableCatalogPath := writeTestJSON(t, dir, "stable-catalog.json", result.Catalog)
	receiptPath := writeTestJSON(t, dir, "promotion-receipt.json", result.Receipt)
	receiptSignaturePath := writeTestJSON(t, dir, "promotion-receipt.sig.json", result.ReceiptSignature)
	currentAlias := &ReleaseAlias{SchemaVersion: 1, Alias: "stable", Revision: 2, ReleaseID: "release-2026-07-29-g2", PreviousReleaseID: result.Receipt.ReleaseID,
		CatalogDigest: operationsTestDigest, ReceiptDigest: operationsTestDigest, UpdatedAt: now.Add(time.Hour), Reason: "promotion"}
	currentSignature, err := SignOperationsPayload("release-alias", currentAlias, releasePrivatePath)
	if err != nil {
		t.Fatal(err)
	}
	currentEnvelopePath := writeTestJSON(t, dir, "current-alias.json", SignedReleaseAlias{SchemaVersion: 1, Alias: *currentAlias, Signature: *currentSignature})
	rolledBack, err := RollbackRelease(currentEnvelopePath, receiptPath, receiptSignaturePath, stableCatalogPath, releasePrivatePath, releasePublicPath, "regression", now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Alias.ReleaseID != result.Receipt.ReleaseID || rolledBack.Alias.PreviousReleaseID != currentAlias.ReleaseID || rolledBack.Alias.Revision != 3 {
		t.Fatalf("unexpected rollback alias: %+v", rolledBack.Alias)
	}
}

func TestCleanupAndRebuildPlanningProtectLeasesAndFreshness(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	state := &OperationsState{SchemaVersion: 1, CapturedAt: now.Add(-time.Minute), ValidUntil: now.Add(30 * time.Minute), RetainNewest: 1, MinRetiredAgeHours: 24,
		Aliases: []ReleaseAlias{{SchemaVersion: 1, Alias: "stable", ReleaseID: "release-current"}},
		Generations: []GenerationRecord{
			{ReleaseID: "release-current", CatalogPath: "releases/current/catalog.json", CatalogDigest: operationsTestDigest, CreatedAt: now.Add(-time.Hour)},
			{ReleaseID: "release-leased", CatalogPath: "releases/leased/catalog.json", CatalogDigest: operationsTestDigest, CreatedAt: now.Add(-72 * time.Hour), RetiredAt: now.Add(-48 * time.Hour)},
			{ReleaseID: "release-old", CatalogPath: "releases/old/catalog.json", CatalogDigest: operationsTestDigest, CreatedAt: now.Add(-96 * time.Hour), RetiredAt: now.Add(-72 * time.Hour)}},
		Leases: []GenerationLease{{LeaseID: "lease-1", ReleaseID: "release-leased", ExpiresAt: now.Add(time.Hour), State: "active"}}}
	cleanup, err := PlanGenerationCleanup(state, now)
	if err != nil {
		t.Fatal(err)
	}
	deleteIDs := []string{}
	for _, decision := range cleanup.Decisions {
		if decision.Action == "delete" {
			deleteIDs = append(deleteIDs, decision.ReleaseID)
		}
	}
	if !slices.Equal(deleteIDs, []string{"release-old"}) {
		t.Fatalf("unexpected cleanup decisions: %+v", cleanup.Decisions)
	}
	statePrivate, statePublic := writeOperationsKeys(t, t.TempDir(), "state")
	stateSignature, err := SignOperationsPayload("operations-state", state, statePrivate)
	if err != nil {
		t.Fatal(err)
	}
	stateEnvelopeDir := t.TempDir()
	stateEnvelope := SignedOperationsState{SchemaVersion: 1, State: *state, Signature: *stateSignature}
	stateEnvelopePath := writeTestJSON(t, stateEnvelopeDir, "signed-state.json", stateEnvelope)
	if _, err := LoadSignedOperationsState(stateEnvelopePath, statePublic); err != nil {
		t.Fatal(err)
	}
	stateEnvelope.State.RetainNewest++
	writeTestJSON(t, stateEnvelopeDir, "signed-state.json", stateEnvelope)
	if _, err := LoadSignedOperationsState(stateEnvelopePath, statePublic); err == nil {
		t.Fatal("cleanup accepted a modified signed state snapshot")
	}
	expiredState := *state
	expiredState.ValidUntil = now
	if _, err := PlanGenerationCleanup(&expiredState, now); err == nil {
		t.Fatal("cleanup accepted an expired state snapshot")
	}
	bundle := &BundleManifest{SchemaVersion: 1, BundleID: "mirror/current", FreshUntil: now.Add(48 * time.Hour)}
	policy := &RebuildPolicy{SchemaVersion: 1, IntervalHours: 24, MinimumFreshHours: 12, Profiles: []string{"pe/amd64/base-v1", "pe/amd64/desktop-v1"},
		LastSuccessfulBuild: map[string]time.Time{"pe/amd64/base-v1": now.Add(-25 * time.Hour), "pe/amd64/desktop-v1": now.Add(-time.Hour)}}
	rebuild, err := PlanRebuild(policy, bundle, now)
	if err != nil {
		t.Fatal(err)
	}
	if !rebuild.Decisions[0].Due || rebuild.Decisions[1].Due {
		t.Fatalf("unexpected rebuild decisions: %+v", rebuild.Decisions)
	}
}
