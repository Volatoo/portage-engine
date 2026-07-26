package imagefactory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/catalog"
)

func TestAssembleCandidateCatalogBindsSignedBundle(t *testing.T) {
	now := time.Date(2026, 7, 23, 4, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	plan := testBuildPlan()
	common := CommonConfig{SchemaVersion: 1, ProxmoxURL: "https://pve.internal:8006/api2/json", ProxmoxNode: "pve01",
		ProxmoxPool: "factory", ProxmoxStorage: "local-lvm", ProxmoxBridge: "vmbr0", SSHUsername: "root", SSHPrivateKeyFile: "/test/key"}
	commonPath := writeTestJSON(t, dir, "common.json", common)
	planPath := writeTestJSON(t, dir, "plan.json", plan)
	repositoryPath := filepath.Join(dir, "gentoo.bundle")
	if err := os.WriteFile(repositoryPath, []byte("repository"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(dir, "seed.qcow2")
	if err := os.WriteFile(seedPath, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	gentooKeyPath := filepath.Join(dir, "gentoo.asc")
	if err := os.WriteFile(gentooKeyPath, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	closurePath := writeTestJSON(t, dir, "closure.json", ClosureManifest{SchemaVersion: 1, Target: plan.Target,
		RepositoryCommit: plan.Repositories["gentoo"], Objects: []ClosureObject{{Filename: "hello.tar.xz", URI: "https://dist.internal/hello.tar.xz",
			SHA256: strings.Repeat("a", 64), Size: 1}}})
	packageSetPath := writeTestJSON(t, dir, "package-sets.json", PackageSetCatalog{SchemaVersion: 1, CatalogID: "pe/test-sets-v1",
		Sets: []PackageSetDefinition{{ID: "pe/runtime-v1", Packages: []string{"app-emulation/cloud-init"}}}})
	planObject := objectForFile(t, plan.PlanObjectID, "build-plan", planPath, []string{plan.Target})
	sourceObject := objectForFile(t, plan.SourceProvenanceObjectID, "seed", seedPath, []string{plan.Target})
	repositoryObject := objectForFile(t, plan.RepositoryObjectIDs["gentoo"], "repository-snapshot", repositoryPath, []string{plan.Target})
	packageSetObject := objectForFile(t, "package-sets/test-v1", "package-set-catalog", packageSetPath, []string{plan.Target})
	lock := InputLock{Version: 1, BundleID: plan.MirrorBundleID, StrictOffline: true,
		AllowedHosts: []string{"pve.internal", "git.internal", "dist.internal", "binpkg.internal"}, AdvisoryCutoff: now.Add(-time.Hour).Format(time.RFC3339),
		Objects: []InputObject{planObject, sourceObject, repositoryObject, objectForFile(t, plan.GentooRepositoryKeyObjectID, "release-key", gentooKeyPath, []string{plan.Target}),
			objectForFile(t, "closure/base", "distfile-manifest", closurePath, []string{plan.Target}), packageSetObject}}
	lockPath := writeTestJSON(t, dir, "inputs.lock.json", lock)
	planDigest, _ := digestFile(planPath)
	lockDigest, _ := digestFile(lockPath)
	commonDigest, _ := digestFile(commonPath)
	customData := map[string]string{
		"image_generation": plan.Generation, "mirror_bundle_id": plan.MirrorBundleID, "profile_id": plan.ProfileID,
		"profile_path": plan.ProfilePath, "profile_repository": plan.ProfileRepository, "profile_parents": "",
		"repository_names": "gentoo", "repository_revisions": plan.Repositories["gentoo"], "template_name": plan.Template,
		"package_sets": strings.Join(plan.PackageSets, ","), "package_set_catalog_digest": "sha256:" + packageSetObject.SHA256,
		"source_template": plan.SourceTemplate, "source_vmid": strconv.Itoa(plan.SourceVMID), "source_provenance_object_id": plan.SourceProvenanceObjectID,
		"source_provenance_digest": "sha256:" + sourceObject.SHA256, "desktop": "false", "display_model": plan.DisplayModel, "build_plan_digest": "sha256:" + planDigest,
		"input_lock_digest": "sha256:" + lockDigest, "common_config_digest": "sha256:" + commonDigest,
	}
	packerPath := writeTestJSON(t, dir, "packer.json", map[string]any{"builds": []any{map[string]any{"artifact_id": "9001", "custom_data": customData}}})
	manifest, err := GenerateManifest(commonPath, planPath, lockPath, packerPath, now)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := writeTestJSON(t, dir, "image-manifest.json", manifest)
	privateKey, publicKey := writeOperationsKeys(t, t.TempDir(), "sync")
	publicData, err := os.ReadFile(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sync-public.json"), publicData, 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, signature, err := SealBundle(lockPath, dir, privateKey, now.Add(time.Minute), 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, dir, "bundle-manifest.json", bundle)
	writeTestJSON(t, dir, "bundle-manifest.sig.json", signature)
	spec := CandidateCatalogAssembly{SchemaVersion: 1, CatalogVersion: 1, DefaultProfileID: plan.ProfileID, DefaultResourceClass: "medium",
		ResourceClasses: []catalog.ResourceClass{{ID: "medium", MachineSpec: map[string]string{"cores": "4", "memory": "8192"}}},
		Artifacts: []CandidateCatalogArtifact{{ImageManifest: filepath.Base(manifestPath), BinhostPath: "releases/amd64/binpackages/23.0/x86-64_test", BuildPlan: filepath.Base(planPath), CommonConfig: filepath.Base(commonPath),
			BundleManifest: "bundle-manifest.json", BundleSignature: "bundle-manifest.sig.json", BundlePublicKey: "sync-public.json",
			InputLock: filepath.Base(lockPath), OfflineRoot: "."}}}
	specPath := writeTestJSON(t, dir, "candidate-assembly.json", spec)
	assembled, err := AssembleCandidateCatalog(specPath, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	wantRepositoryDigest := sha256.Sum256([]byte("repository"))
	if len(assembled.Images) != 1 || len(assembled.MirrorBundles) != 1 || assembled.Repositories[0].Digest != "sha256:"+hex.EncodeToString(wantRepositoryDigest[:]) || !assembled.Profiles[0].Default {
		t.Fatalf("unexpected candidate catalog: %+v", assembled)
	}
	manifest.InputLockDigest = operationsTestDigest
	writeTestJSON(t, dir, "image-manifest.json", manifest)
	if _, err := AssembleCandidateCatalog(specPath, now.Add(2*time.Minute)); err == nil {
		t.Fatal("candidate catalog accepted an image bound to a different input lock")
	}
}
