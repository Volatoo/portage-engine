package imagefactory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testBuildPlan() BuildPlan {
	return BuildPlan{
		SchemaVersion: 1, Target: "base-systemd", PlanObjectID: "build-plan/base-g1",
		ImageID: "pe/amd64/base-g1", Generation: "g1", Provider: "pve", Arch: "amd64", BuildMode: "native-gentoo",
		SourceTemplate: "gentoo-seed", SourceVMID: 9000, SourceProvenanceObjectID: "seed/gentoo",
		Template: "pe-base-g1", TemplateSummary: "test candidate", ProfileID: "pe/amd64/base",
		ProfilePath: "default/linux/amd64/23.0/systemd", ProfileRepository: "gentoo", MirrorBundleID: "mirror/test",
		GentooRepositoryKeyObjectID: "release-key/gentoo-developer",
		Repositories:                map[string]string{"gentoo": "0123456789abcdef0123456789abcdef01234567"},
		RepositoryObjectIDs:         map[string]string{"gentoo": "repo/gentoo"}, RepositoryURIs: map[string]string{"gentoo": "https://git.internal/gentoo.git"},
		RootfsSource: "approved-qcow2", Channel: "candidate",
		GentooMirror: "https://dist.internal", Binhost: "https://binpkg.internal", PackageSets: []string{"pe/runtime-v1"}, Packages: []string{"app-misc/hello"},
		DisplayModel: "std", Cores: 2, Memory: 4096,
	}
}

func writeTestJSON(t *testing.T, dir, name string, value any) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func objectForFile(t *testing.T, id, kind, path string, requiredFor []string) InputObject {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return InputObject{ID: id, Kind: kind, Path: filepath.Base(path), SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data)), RequiredFor: requiredFor}
}

func addTestPackerExecutionSurface(t *testing.T, root, target string, lock *InputLock) {
	t.Helper()
	paths := map[string]bool{
		"factory/run-offline.sh":                      true,
		"factory/smoke-offline.sh":                    true,
		"factory/packer/template.pkr.hcl":             false,
		"factory/packer/scripts/provision.sh":         true,
		"factory/packer/scripts/sanitize-and-gate.sh": true,
		"factory/packer/scripts/hydrate-distfiles.py": true,
		"factory/catalyst/verify-profile.py":          true,
		"factory/desktop/guest-agent.py":              true,
	}
	index := 0
	for relative, executable := range paths {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if executable {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(relative+"\n"), mode); err != nil {
			t.Fatal(err)
		}
		object := objectForFile(t, fmt.Sprintf("script/test-%d", index), "script", path, []string{target})
		object.Path = relative
		object.Executable = executable
		lock.Objects = append(lock.Objects, object)
		index++
	}
	terraformConfigPath := filepath.Join(root, "terraform", "terraform.rc")
	if err := os.MkdirAll(filepath.Dir(terraformConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(terraformConfigPath, []byte(strictTerraformCLIConfig+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	terraformConfig := objectForFile(t, "terraform/cli-config", "terraform-lock", terraformConfigPath, []string{target})
	terraformConfig.Path = "terraform/terraform.rc"
	lock.Objects = append(lock.Objects, terraformConfig)
}

func TestGenerateManifest(t *testing.T) {
	dir := t.TempDir()
	plan := testBuildPlan()
	common := CommonConfig{SchemaVersion: 1, ProxmoxURL: "https://pve.internal:8006/api2/json", ProxmoxNode: "pve01",
		ProxmoxPool: "factory", ProxmoxStorage: "local-lvm", ProxmoxBridge: "vmbr0", SSHUsername: "root", SSHPrivateKeyFile: "/test/key"}
	commonPath := writeTestJSON(t, dir, "common.json", common)
	planPath := writeTestJSON(t, dir, "plan.json", plan)
	planObject := objectForFile(t, plan.PlanObjectID, "build-plan", planPath, []string{plan.Target})
	sourceData := []byte("seed")
	sourceDigest := sha256.Sum256(sourceData)
	sourceObject := InputObject{ID: plan.SourceProvenanceObjectID, Kind: "seed", Path: "seed.qcow2", SHA256: hex.EncodeToString(sourceDigest[:]), Size: int64(len(sourceData)), RequiredFor: []string{plan.Target}}
	packageSetPath := writeTestJSON(t, dir, "package-sets.json", PackageSetCatalog{SchemaVersion: 1, CatalogID: "pe/test-sets-v1", Sets: []PackageSetDefinition{{ID: "pe/runtime-v1", Packages: []string{"app-emulation/cloud-init"}}}})
	packageSetObject := objectForFile(t, "package-sets/test-v1", "package-set-catalog", packageSetPath, nil)
	lock := &InputLock{Version: 1, BundleID: plan.MirrorBundleID, StrictOffline: true, AllowedHosts: []string{"pve.internal"}, Objects: []InputObject{planObject, sourceObject}}
	lock.Objects = append(lock.Objects, packageSetObject)
	lockPath := writeTestJSON(t, dir, "lock.json", lock)
	planDigest, _ := digestFile(planPath)
	lockDigest, _ := digestFile(lockPath)
	commonDigest, _ := digestFile(commonPath)
	customData := map[string]string{
		"image_generation": plan.Generation, "mirror_bundle_id": plan.MirrorBundleID, "profile_id": plan.ProfileID,
		"profile_path": plan.ProfilePath, "profile_repository": plan.ProfileRepository, "profile_parents": "",
		"repository_names": "gentoo", "repository_revisions": plan.Repositories["gentoo"], "template_name": plan.Template,
		"package_sets": strings.Join(plan.PackageSets, ","), "package_set_catalog_digest": "sha256:" + packageSetObject.SHA256,
		"source_template": plan.SourceTemplate, "source_vmid": strconv.Itoa(plan.SourceVMID),
		"source_provenance_object_id": plan.SourceProvenanceObjectID, "source_provenance_digest": "sha256:" + sourceObject.SHA256,
		"desktop": "false", "display_model": plan.DisplayModel, "build_plan_digest": "sha256:" + planDigest, "input_lock_digest": "sha256:" + lockDigest,
		"common_config_digest": "sha256:" + commonDigest,
	}
	packerPath := writeTestJSON(t, dir, "packer.json", map[string]any{"builds": []any{map[string]any{"artifact_id": "9001", "custom_data": customData}}})
	now := time.Unix(100, 0).UTC()
	manifest, err := GenerateManifest(commonPath, planPath, lockPath, packerPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Template != plan.Template || manifest.PackerArtifactID != "9001" || manifest.ImageDigest == "" || manifest.BuildPlanDigest == "" || !manifest.CreatedAt.Equal(now) {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	fragment := manifest.CatalogFragment()
	if fragment.Image.Digest != manifest.ImageDigest || fragment.Profile.ImageID != manifest.ImageID {
		t.Fatalf("unexpected catalog fragment: %+v", fragment)
	}
	manifestPath := writeTestJSON(t, dir, "image-manifest.json", manifest)
	loaded, err := LoadImageManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.ValidateForPlan(&plan); err != nil {
		t.Fatal(err)
	}
	if err := loaded.ValidateEvidenceFiles(commonPath, planPath, lockPath); err != nil {
		t.Fatal(err)
	}
	loaded.Provider = "qemu"
	if err := loaded.ValidateForPlan(&plan); err == nil {
		t.Fatal("accepted a candidate manifest with mutated provider metadata")
	}

	customData["profile_path"] = "wrong/profile"
	packerPath = writeTestJSON(t, dir, "packer-wrong.json", map[string]any{"builds": []any{map[string]any{"artifact_id": "9001", "custom_data": customData}}})
	if _, err := GenerateManifest(commonPath, planPath, lockPath, packerPath, now); err == nil {
		t.Fatal("accepted Packer output that does not match the reviewed BuildPlan")
	}
}
