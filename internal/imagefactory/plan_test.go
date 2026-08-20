package imagefactory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPreparePlanBindsInputsAndRejectsEndpointDrift(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := CommonConfig{SchemaVersion: 1, ProxmoxURL: "https://pve.internal:8006/api2/json", ProxmoxNode: "pve01",
		ProxmoxPool: "factory", ProxmoxStorage: "local-lvm", ProxmoxBridge: "vmbr0", SSHUsername: "root", SSHPrivateKeyFile: keyPath}
	commonPath := writeTestJSON(t, dir, "common.json", common)
	plan := testBuildPlan()
	plan.RepositoryURIs["gentoo"] = "https://git.internal/gentoo.git"
	plan.GentooMirror = "https://dist.internal"
	plan.Binhost = "https://binpkg.internal"
	planPath := writeTestJSON(t, dir, "plan.json", plan)

	repositoryPath := filepath.Join(dir, "gentoo.bundle")
	if err := os.WriteFile(repositoryPath, []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	gentooKeyPath := filepath.Join(dir, "gentoo-developer.asc")
	if err := os.WriteFile(gentooKeyPath, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(dir, "seed.qcow2")
	if err := os.WriteFile(seedPath, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	closure := ClosureManifest{SchemaVersion: 1, Target: plan.Target, RepositoryCommit: plan.Repositories["gentoo"], Objects: []ClosureObject{{
		Filename: "hello.tar.xz", URI: "https://dist.internal/hello.tar.xz", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 1,
	}}}
	closurePath := writeTestJSON(t, dir, "closure.json", closure)
	packageSetPath := writeTestJSON(t, dir, "package-sets.json", PackageSetCatalog{SchemaVersion: 1, CatalogID: "pe/test-sets-v1", Sets: []PackageSetDefinition{{ID: "pe/runtime-v1", Packages: []string{"app-emulation/cloud-init"}}}})
	planObject := objectForFile(t, plan.PlanObjectID, "build-plan", planPath, []string{plan.Target})
	planObject.Path = filepath.Base(planPath)
	sourceObject := objectForFile(t, plan.SourceProvenanceObjectID, "seed", seedPath, []string{plan.Target})
	repositoryObject := objectForFile(t, "repo/gentoo", "repository-snapshot", repositoryPath, nil)
	manifestObject := objectForFile(t, "closure/base", "distfile-manifest", closurePath, []string{plan.Target})
	packageSetObject := objectForFile(t, "package-sets/test-v1", "package-set-catalog", packageSetPath, nil)
	lock := &InputLock{Version: 1, BundleID: plan.MirrorBundleID, StrictOffline: true,
		AllowedHosts: []string{"pve.internal", "git.internal", "dist.internal", "binpkg.internal"},
		Objects:      []InputObject{planObject, sourceObject, repositoryObject, objectForFile(t, plan.GentooRepositoryKeyObjectID, "release-key", gentooKeyPath, nil), manifestObject, packageSetObject}}
	addTestPackerExecutionSurface(t, dir, plan.Target, lock)
	lockPath := writeTestJSON(t, dir, "lock.json", lock)
	vars, evidence, err := PreparePlan(commonPath, planPath, lockPath, dir, filepath.Join(dir, "packer.json"))
	if err != nil {
		t.Fatal(err)
	}
	if vars.SourceVMID != plan.SourceVMID || len(vars.RepositoryBundlePaths) != 1 || vars.RepositoryBundlePaths[0] != repositoryPath || evidence.DistfileManifestID != manifestObject.ID {
		t.Fatalf("unexpected prepared plan: vars=%+v evidence=%+v", vars, evidence)
	}
	if !slices.Equal(vars.PackageSets, plan.PackageSets) || evidence.PackageSetCatalogID != packageSetObject.ID || !slices.Contains(vars.Packages, "app-emulation/cloud-init") || !slices.Contains(vars.Packages, "app-misc/hello") {
		t.Fatalf("package sets were not resolved and bound: vars=%+v evidence=%+v", vars, evidence)
	}

	plan.GentooMirror = "https://dist.internal/files?token=secret"
	badPlanPath := writeTestJSON(t, dir, "bad-plan.json", plan)
	badPlanObject := objectForFile(t, plan.PlanObjectID, "build-plan", badPlanPath, []string{plan.Target})
	lock.Objects[0] = badPlanObject
	badLockPath := writeTestJSON(t, dir, "bad-lock.json", lock)
	if _, _, err := PreparePlan(commonPath, badPlanPath, badLockPath, dir, filepath.Join(dir, "packer.json")); err == nil {
		t.Fatal("accepted an endpoint containing a query secret")
	}
}

func TestLoadCommonConfigRejectsUnsafePVEEndpoint(t *testing.T) {
	dir := t.TempDir()
	config := CommonConfig{SchemaVersion: 1, ProxmoxURL: "https://pve.internal:8006/api2/json?token=secret", ProxmoxNode: "pve01",
		ProxmoxPool: "factory", ProxmoxStorage: "local-lvm", ProxmoxBridge: "vmbr0", SSHUsername: "root", SSHPrivateKeyFile: filepath.Join(dir, "id_ed25519")}
	path := writeTestJSON(t, dir, "common.json", config)
	if _, err := LoadCommonConfig(path); err == nil {
		t.Fatal("accepted a PVE endpoint containing a query secret")
	}
}

func TestTrackedBuildPlansLoad(t *testing.T) {
	for _, name := range []string{"base-systemd.build.json", "base-systemd.catalyst.build.json", "desktop-verifier.build.json"} {
		if _, err := LoadBuildPlan(filepath.Join("..", "..", "image-factory", "plans", name)); err != nil {
			t.Errorf("tracked BuildPlan %s is invalid: %v", name, err)
		}
	}
}

func TestPackerExecutionSurfaceRequiresGuestAgent(t *testing.T) {
	dir := t.TempDir()
	lock := &InputLock{Version: 1, BundleID: "mirror/test", StrictOffline: true, AllowedHosts: []string{"mirror.internal"}}
	addTestPackerExecutionSurface(t, dir, "base-systemd", lock)
	if err := requirePackerExecutionSurface(lock, "base-systemd"); err != nil {
		t.Fatal(err)
	}
	for index := range lock.Objects {
		if lock.Objects[index].Path == "factory/desktop/guest-agent.py" {
			lock.Objects = append(lock.Objects[:index], lock.Objects[index+1:]...)
			break
		}
	}
	if err := requirePackerExecutionSurface(lock, "base-systemd"); err == nil || !strings.Contains(err.Error(), "guest-agent.py") {
		t.Fatalf("Packer execution surface accepted without guest helper: %v", err)
	}
}

func TestTerraformCLIConfigRequiresStrictFilesystemMirrorTemplate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "terraform", "terraform.rc")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := &InputLock{Version: 1, BundleID: "mirror/test", StrictOffline: true}
	writeConfig := func(contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		object := objectForFile(t, "terraform/cli-config", "terraform-lock", path, []string{"base-systemd"})
		object.Path = "terraform/terraform.rc"
		lock.Objects = []InputObject{object}
	}

	writeConfig(strictTerraformCLIConfig + "\n")
	if err := validateTerraformCLIConfig(dir, lock, "base-systemd"); err != nil {
		t.Fatalf("approved config was rejected: %v", err)
	}
	writeConfig(strings.Replace(strictTerraformCLIConfig, "disable_checkpoint = true", "direct {}", 1))
	if err := validateTerraformCLIConfig(dir, lock, "base-systemd"); err == nil {
		t.Fatal("Terraform direct provider fallback was accepted")
	}
	writeConfig(strings.Replace(strictTerraformCLIConfig, "__PORTAGE_ENGINE_OFFLINE_TERRAFORM_PROVIDERS__", "/srv/portage-engine-offline/terraform/providers", 1))
	if err := validateTerraformCLIConfig(dir, lock, "base-systemd"); err == nil {
		t.Fatal("hard-coded Terraform provider mirror path was accepted")
	}
}

func TestDesktopSourceManifestBindsBaseABIAndRepositories(t *testing.T) {
	dir := t.TempDir()
	digest := "sha256:" + strings.Repeat("a", 64)
	plan := testBuildPlan()
	plan.Target = "desktop-verifier"
	plan.Desktop = true
	plan.DisplayModel = "qxl"
	plan.RootfsSource = "packer-base-image"
	plan.SourceTemplate = "pe-base-g2"
	plan.ProfileRepository = "pe-profiles"
	plan.ProfilePath = "portage-engine/amd64/23.0/no-multilib/systemd/desktop-verifier"
	plan.ProfileParents = []CatalystProfileParent{{Repository: "pe-profiles", ProfilePath: "portage-engine/amd64/23.0/no-multilib/systemd/base"}}
	plan.Repositories["pe-profiles"] = strings.Repeat("2", 40)
	plan.SourceRepositories = maps.Clone(plan.Repositories)
	plan.SourceDisplayModel = "std"
	plan.Repositories["pe-profiles"] = strings.Repeat("3", 40)
	manifest := ImageManifest{
		SchemaVersion: 1, CreatedAt: time.Unix(100, 0).UTC(), Target: "base-systemd", ImageID: "pe/base-g2", Generation: "g2",
		Provider: plan.Provider, Arch: plan.Arch, BuildMode: plan.BuildMode, Template: plan.SourceTemplate,
		ProfileID: "pe/base-v1", ProfilePath: plan.ProfileParents[0].ProfilePath, ProfileRepository: "pe-profiles",
		ProfileParents: []CatalystProfileParent{{Repository: "gentoo", ProfilePath: "default/linux/amd64/23.0/no-multilib/systemd"}},
		PackageSets:    []string{"pe/runtime-v1"}, PackageSetCatalogDigest: digest, MirrorBundleID: "mirror/base-g2",
		Repositories: maps.Clone(plan.SourceRepositories), RootfsSource: "approved-pbs-snapshot", Channel: "candidate",
		InputLockDigest: digest, CommonConfigDigest: digest, BuildPlanDigest: digest, PackerManifestDigest: digest,
		SourceProvenanceDigest: digest, RootfsManifestDigest: digest, PackerArtifactID: "103", ImageDigest: digest,
	}
	path := writeTestJSON(t, dir, "base.image-manifest.json", manifest)
	if err := validateSourceImageManifest(path, &plan); err != nil {
		t.Fatal(err)
	}
	manifest.Repositories["gentoo"] = strings.Repeat("3", 40)
	path = writeTestJSON(t, dir, "drifted.image-manifest.json", manifest)
	if err := validateSourceImageManifest(path, &plan); err == nil {
		t.Fatal("desktop source repository drift was accepted")
	}
}

func TestDesktopSuccessorSourceManifestBindsExactProfile(t *testing.T) {
	dir := t.TempDir()
	digest := "sha256:" + strings.Repeat("a", 64)
	plan := testBuildPlan()
	plan.Target = "desktop-verifier"
	plan.Desktop = true
	plan.DisplayModel = "qxl"
	plan.RootfsSource = "packer-desktop-image"
	plan.SourceTemplate = "pe-desktop-g1"
	plan.ProfileRepository = "pe-profiles"
	plan.ProfilePath = "portage-engine/amd64/23.0/no-multilib/systemd/desktop-verifier"
	plan.ProfileParents = []CatalystProfileParent{{Repository: "pe-profiles", ProfilePath: "portage-engine/amd64/23.0/no-multilib/systemd/base"}}
	plan.Repositories["pe-profiles"] = strings.Repeat("2", 40)
	plan.SourceRepositories = maps.Clone(plan.Repositories)
	plan.SourceDisplayModel = "std"
	manifest := ImageManifest{
		SchemaVersion: 1, CreatedAt: time.Unix(100, 0).UTC(), Target: "desktop-verifier", ImageID: "pe/desktop-g1", Generation: "g1",
		Provider: plan.Provider, Arch: plan.Arch, BuildMode: plan.BuildMode, Template: plan.SourceTemplate,
		ProfileID: plan.ProfileID, ProfilePath: plan.ProfilePath, ProfileRepository: plan.ProfileRepository, ProfileParents: append([]CatalystProfileParent(nil), plan.ProfileParents...),
		PackageSets: []string{"pe/desktop-v1"}, PackageSetCatalogDigest: digest, MirrorBundleID: "mirror/desktop-g1",
		Repositories: maps.Clone(plan.SourceRepositories), RootfsSource: "packer-base-image", Channel: "candidate",
		InputLockDigest: digest, CommonConfigDigest: digest, BuildPlanDigest: digest, PackerManifestDigest: digest,
		SourceProvenanceDigest: digest, RootfsManifestDigest: digest, PackerArtifactID: "139", ImageDigest: digest,
	}
	path := writeTestJSON(t, dir, "desktop.image-manifest.json", manifest)
	if err := validateSourceImageManifest(path, &plan); err != nil {
		t.Fatal(err)
	}
	manifest.ProfilePath += "-drifted"
	path = writeTestJSON(t, dir, "drifted.image-manifest.json", manifest)
	if err := validateSourceImageManifest(path, &plan); err == nil {
		t.Fatal("desktop successor profile drift was accepted")
	}
}

func TestImageDerivedPlansRequireOnlySourceRepositoryKeys(t *testing.T) {
	plan := testBuildPlan()
	plan.RootfsSource = "packer-base-image"
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "source_repositories") {
		t.Fatalf("image-derived plan without source repositories was accepted: %v", err)
	}

	plan.SourceRepositories = maps.Clone(plan.Repositories)
	plan.SourceDisplayModel = "std"
	if err := plan.Validate(); err != nil {
		t.Fatalf("fully pinned image-derived plan was rejected: %v", err)
	}

	plan.SourceRepositories["unreviewed"] = strings.Repeat("b", 40)
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "source_repositories") {
		t.Fatalf("extra source repository was accepted: %v", err)
	}

	plan.SourceRepositories = maps.Clone(plan.Repositories)
	plan.RootfsSource = "approved-pbs-snapshot"
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "unexpected source image contract") {
		t.Fatalf("non-image plan with source repositories was accepted: %v", err)
	}
}

func TestBaseSuccessorSourceManifestBindsExactProfile(t *testing.T) {
	dir := t.TempDir()
	digest := "sha256:" + strings.Repeat("a", 64)
	plan := testBuildPlan()
	plan.RootfsSource = "packer-base-image"
	plan.SourceTemplate = "pe-base-g2"
	plan.ProfileRepository = "pe-profiles"
	plan.ProfilePath = "portage-engine/amd64/23.0/no-multilib/systemd/base"
	plan.ProfileParents = []CatalystProfileParent{{Repository: "gentoo", ProfilePath: "default/linux/amd64/23.0/no-multilib/systemd"}}
	plan.ProfileRepositoryKeyObjectID = "release-key/pe-profiles"
	plan.Repositories["pe-profiles"] = strings.Repeat("2", 40)
	plan.SourceRepositories = maps.Clone(plan.Repositories)
	plan.SourceDisplayModel = "std"
	plan.Repositories["pe-profiles"] = strings.Repeat("3", 40)
	plan.RepositoryObjectIDs["pe-profiles"] = "repo/pe-profiles"
	plan.RepositoryURIs["pe-profiles"] = "https://git.internal/pe-profiles.git"
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	if kind := expectedSourceObjectKind(&plan); kind != "image-manifest" {
		t.Fatalf("unexpected base successor source kind %q", kind)
	}
	manifest := ImageManifest{
		SchemaVersion: 1, CreatedAt: time.Unix(100, 0).UTC(), Target: "base-systemd", ImageID: "pe/base-g2", Generation: "g2",
		Provider: plan.Provider, Arch: plan.Arch, BuildMode: plan.BuildMode, Template: plan.SourceTemplate,
		ProfileID: "pe/base-v1", ProfilePath: plan.ProfilePath, ProfileRepository: plan.ProfileRepository, ProfileParents: slices.Clone(plan.ProfileParents),
		PackageSets: []string{"pe/runtime-v1"}, PackageSetCatalogDigest: digest, MirrorBundleID: "mirror/base-g2",
		Repositories: maps.Clone(plan.SourceRepositories), RootfsSource: "catalyst-stage4-qcow2", Channel: "candidate",
		InputLockDigest: digest, CommonConfigDigest: digest, BuildPlanDigest: digest, PackerManifestDigest: digest,
		SourceProvenanceDigest: digest, RootfsManifestDigest: digest, PackerArtifactID: "135", ImageDigest: digest,
	}
	path := writeTestJSON(t, dir, "base.image-manifest.json", manifest)
	if err := validateSourceImageManifest(path, &plan); err != nil {
		t.Fatal(err)
	}
	manifest.ProfilePath = "portage-engine/amd64/23.0/systemd/base"
	path = writeTestJSON(t, dir, "drifted.image-manifest.json", manifest)
	if err := validateSourceImageManifest(path, &plan); err == nil {
		t.Fatal("base successor profile drift was accepted")
	}
}

func TestArtifactEndpointAllowsDigestLockedHTTPButControlPlaneDoesNot(t *testing.T) {
	host, err := validateEndpoint("http://10.31.0.2/gentoo/distfile.tar.xz", []string{"10.31.0.2"})
	if err != nil || host != "10.31.0.2" {
		t.Fatalf("allowlisted HTTP artifact endpoint was rejected: host=%q err=%v", host, err)
	}
	for _, endpoint := range []string{
		"http://10.31.0.9/object?token=secret",
		"http://user:secret@10.31.0.9/object",
		"ftp://10.31.0.9/object",
		"http://10.31.0.8/object",
	} {
		if _, err := validateEndpoint(endpoint, []string{"10.31.0.9"}); err == nil {
			t.Fatalf("unsafe artifact endpoint was accepted: %s", endpoint)
		}
	}

	dir := t.TempDir()
	config := CommonConfig{SchemaVersion: 1, ProxmoxURL: "http://10.31.0.200:8006/api2/json", ProxmoxNode: "pve01",
		ProxmoxPool: "factory", ProxmoxStorage: "local-lvm", ProxmoxBridge: "vmbr0", SSHUsername: "root", SSHPrivateKeyFile: filepath.Join(dir, "id_ed25519")}
	path := writeTestJSON(t, dir, "http-pve.json", config)
	if _, err := LoadCommonConfig(path); err == nil {
		t.Fatal("plaintext PVE control-plane endpoint was accepted")
	}
}

func TestPreparePlanBindsExternalProfileRepository(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := CommonConfig{SchemaVersion: 1, ProxmoxURL: "https://pve.internal:8006/api2/json", ProxmoxNode: "pve01",
		ProxmoxPool: "factory", ProxmoxStorage: "local-lvm", ProxmoxBridge: "vmbr0", SSHUsername: "root", SSHPrivateKeyFile: keyPath}
	commonPath := writeTestJSON(t, dir, "common.json", common)
	plan := testBuildPlan()
	plan.ProfileRepository = "pe-profiles"
	plan.ProfilePath = "portage-engine/amd64/23.0/systemd/base"
	plan.ProfileParents = []CatalystProfileParent{{Repository: "gentoo", ProfilePath: catalystOfficialProfile}}
	plan.ProfileRepositoryKeyObjectID = "release-key/pe-profiles"
	plan.Repositories["pe-profiles"] = strings.Repeat("2", 40)
	plan.RepositoryObjectIDs["pe-profiles"] = "repo/pe-profiles"
	plan.RepositoryURIs["pe-profiles"] = "https://git.internal/pe-profiles.git"
	planPath := writeTestJSON(t, dir, "plan.json", plan)
	write := func(name, value string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	gentooBundle := write("gentoo.bundle", "gentoo")
	profileBundle := write("pe-profiles.bundle", "profiles")
	profileKey := write("pe-profiles-release.asc", "key")
	gentooKey := write("gentoo-developer.asc", "gentoo-key")
	seedPath := write("seed.qcow2", "seed")
	closurePath := writeTestJSON(t, dir, "closure.json", ClosureManifest{SchemaVersion: 1, Target: plan.Target, RepositoryCommit: plan.Repositories["gentoo"], Objects: []ClosureObject{{
		Filename: "hello.tar.xz", URI: "https://dist.internal/hello.tar.xz", SHA256: strings.Repeat("a", 64), Size: 1}}})
	packageSetPath := writeTestJSON(t, dir, "package-sets.json", PackageSetCatalog{SchemaVersion: 1, CatalogID: "pe/test-sets-v1", Sets: []PackageSetDefinition{{ID: "pe/runtime-v1", Packages: []string{"app-emulation/cloud-init"}}}})
	lock := InputLock{Version: 1, BundleID: plan.MirrorBundleID, StrictOffline: true, AllowedHosts: []string{"pve.internal", "git.internal", "dist.internal", "binpkg.internal"}, Objects: []InputObject{
		objectForFile(t, plan.PlanObjectID, "build-plan", planPath, []string{plan.Target}),
		objectForFile(t, plan.SourceProvenanceObjectID, "seed", seedPath, []string{plan.Target}),
		objectForFile(t, "repo/gentoo", "repository-snapshot", gentooBundle, nil),
		objectForFile(t, "repo/pe-profiles", "repository-snapshot", profileBundle, nil),
		objectForFile(t, plan.GentooRepositoryKeyObjectID, "release-key", gentooKey, nil),
		objectForFile(t, "release-key/pe-profiles", "release-key", profileKey, nil),
		objectForFile(t, "closure/base", "distfile-manifest", closurePath, []string{plan.Target}),
		objectForFile(t, "package-sets/test-v1", "package-set-catalog", packageSetPath, nil),
	}}
	addTestPackerExecutionSurface(t, dir, plan.Target, &lock)
	lockPath := writeTestJSON(t, dir, "lock.json", lock)
	vars, evidence, err := PreparePlan(commonPath, planPath, lockPath, dir, filepath.Join(dir, "packer.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(vars.RepositoryNames, []string{"gentoo", "pe-profiles"}) || vars.ProfileRepository != "pe-profiles" || vars.ProfileRepositoryKeyName != filepath.Base(profileKey) || vars.GentooRepositoryKeyName != filepath.Base(gentooKey) || len(vars.LockedRepositoryInputPaths) != 4 || !slices.Equal(vars.ProfileParents, []string{"gentoo:" + catalystOfficialProfile}) || len(evidence.RepositoryBundleIDs) != 2 || len(evidence.RepositoryKeyIDs) != 2 {
		t.Fatalf("external profile repository was not bound: vars=%+v evidence=%+v", vars, evidence)
	}
}

func TestCatalystQCOW2BuildPlanRequiresQCOW2ManifestSource(t *testing.T) {
	plan := testBuildPlan()
	plan.RootfsSource = "catalyst-stage4-qcow2"
	plan.SourceProvenanceObjectID = "qcow2-manifest/catalyst-base-g1"
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	if kind := expectedSourceObjectKind(&plan); kind != "qcow2-manifest" {
		t.Fatalf("unexpected Catalyst handoff source kind %q", kind)
	}
}

func TestCatalystQCOW2SourceManifestBindsProfile(t *testing.T) {
	dir := t.TempDir()
	manifest := QCOW2Manifest{
		SchemaVersion: 1, CreatedAt: time.Unix(100, 0).UTC(), Target: catalystProfileTarget,
		RootfsID: "rootfs/test", Generation: "g1", Arch: "amd64", ProfileID: "pe/amd64/no-multilib/systemd/base-v1",
		RootfsManifestDigest: "sha256:" + strings.Repeat("a", 64), AssemblerDigest: "sha256:" + strings.Repeat("b", 64),
		QCOW2Filename: "gentoo.qcow2", QCOW2Digest: "sha256:" + strings.Repeat("c", 64), QCOW2Size: 1024, VirtualSizeGiB: 32,
	}
	path := writeTestJSON(t, dir, "source.qcow2-manifest.json", manifest)
	plan := testBuildPlan()
	plan.ProfileID = manifest.ProfileID
	if err := validateCatalystSourceManifest(path, &plan); err != nil {
		t.Fatal(err)
	}
	manifest.ProfileID = "pe/amd64/wrong-profile"
	path = writeTestJSON(t, dir, "wrong-profile.qcow2-manifest.json", manifest)
	if err := validateCatalystSourceManifest(path, &plan); err == nil || !strings.Contains(err.Error(), "architecture/profile") {
		t.Fatalf("Catalyst source profile drift was accepted: %v", err)
	}
}

func TestBuildPlanRequiresGentooRepositoryVerificationKey(t *testing.T) {
	plan := testBuildPlan()
	plan.GentooRepositoryKeyObjectID = ""
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "Gentoo repository verification key") {
		t.Fatalf("unsigned Gentoo repository plan was accepted: %v", err)
	}
}

func TestPreparePlanRejectsDuplicateDistfileFilename(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := CommonConfig{SchemaVersion: 1, ProxmoxURL: "https://pve.internal:8006/api2/json", ProxmoxNode: "pve01",
		ProxmoxPool: "factory", ProxmoxStorage: "local-lvm", ProxmoxBridge: "vmbr0", SSHUsername: "root", SSHPrivateKeyFile: keyPath}
	commonPath := writeTestJSON(t, dir, "common.json", common)
	plan := testBuildPlan()
	plan.RepositoryURIs["gentoo"] = "https://git.internal/gentoo.git"
	plan.GentooMirror = "https://dist.internal"
	planPath := writeTestJSON(t, dir, "plan.json", plan)
	repositoryPath := filepath.Join(dir, "gentoo.bundle")
	seedPath := filepath.Join(dir, "seed.qcow2")
	if err := os.WriteFile(repositoryPath, []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedPath, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	gentooKeyPath := filepath.Join(dir, "gentoo-developer.asc")
	if err := os.WriteFile(gentooKeyPath, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	object := ClosureObject{Filename: "same.tar.xz", URI: "https://dist.internal/same.tar.xz", SHA256: strings.Repeat("a", 64), Size: 1}
	closurePath := writeTestJSON(t, dir, "closure.json", ClosureManifest{SchemaVersion: 1, Target: plan.Target, RepositoryCommit: plan.Repositories["gentoo"], Objects: []ClosureObject{object, object}})
	packageSetPath := writeTestJSON(t, dir, "package-sets.json", PackageSetCatalog{SchemaVersion: 1, CatalogID: "pe/test-sets-v1", Sets: []PackageSetDefinition{{ID: "pe/runtime-v1", Packages: []string{"app-emulation/cloud-init"}}}})
	planObject := objectForFile(t, plan.PlanObjectID, "build-plan", planPath, []string{plan.Target})
	planObject.Path = filepath.Base(planPath)
	lock := &InputLock{Version: 1, BundleID: plan.MirrorBundleID, StrictOffline: true,
		AllowedHosts: []string{"pve.internal", "git.internal", "dist.internal"},
		Objects: []InputObject{planObject, objectForFile(t, plan.SourceProvenanceObjectID, "seed", seedPath, []string{plan.Target}),
			objectForFile(t, "repo/gentoo", "repository-snapshot", repositoryPath, nil), objectForFile(t, plan.GentooRepositoryKeyObjectID, "release-key", gentooKeyPath, nil), objectForFile(t, "closure/base", "distfile-manifest", closurePath, []string{plan.Target}),
			objectForFile(t, "package-sets/test-v1", "package-set-catalog", packageSetPath, nil)}}
	addTestPackerExecutionSurface(t, dir, plan.Target, lock)
	lockPath := writeTestJSON(t, dir, "lock.json", lock)
	if _, _, err := PreparePlan(commonPath, planPath, lockPath, dir, filepath.Join(dir, "packer.json")); err == nil {
		t.Fatal("accepted duplicate distfile filenames")
	}
}

func TestStampPVEOutputBindsManifestDigest(t *testing.T) {
	putSeen := false
	stampedDescription := ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/cluster/resources"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":9200,"node":"pve01","name":"pe-base-g1","template":1,"type":"qemu"}]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api2/json/nodes/pve01/qemu/9200/config":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			stampedDescription = r.Form.Get("description")
			putSeen = strings.Contains(stampedDescription, "portage-engine-provenance=sha256:") &&
				strings.Contains(stampedDescription, pveManifestField+"=") &&
				r.Form.Get("ciupgrade") == "0" && r.Form.Get("ciuser") == "root" && r.Form.Get("ipconfig0") == "ip=dhcp"
			_, _ = w.Write([]byte(`{"data":null}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve01/qemu/9200/config":
			_, _ = fmt.Fprintf(w, `{"data":{"name":"pe-base-g1","template":1,"description":%q,"ciupgrade":0,"ciuser":"root","ipconfig0":"ip=dhcp","vga":"memory=64,type=std"}}`, stampedDescription)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manifest := stampedTestImageManifest("pe/amd64/base-g1", "pe-base-g1", "std")
	manifestPath := writeTestJSON(t, t.TempDir(), "manifest.json", manifest)
	common := &CommonConfig{ProxmoxURL: server.URL + "/api2/json", ProxmoxNode: "pve01", ProxmoxInsecure: true}
	evidence, err := StampPVEOutput(context.Background(), common, manifest, manifestPath, "user@pve!factory", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if !putSeen || !evidence.Verified || evidence.VMID != 9200 {
		t.Fatalf("output was not stamped: %+v", evidence)
	}
	recovered, err := RecoverPVEOutputManifest(context.Background(), common, manifest.Template, evidence.ManifestDigest, "user@pve!factory", "secret")
	if err != nil {
		t.Fatal(err)
	}
	wantRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(recovered.RawManifest, wantRaw) || recovered.Manifest.Template != manifest.Template || recovered.VMID != 9200 {
		t.Fatalf("unexpected recovered manifest: %+v", recovered)
	}
}

func TestStampPVEOutputRejectsReadBackDrift(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/cluster/resources"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":9200,"node":"pve01","name":"pe-base-g1","template":1,"type":"qemu"}]}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"data":null}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"data":{"description":"drifted","ciupgrade":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manifest := stampedTestImageManifest("pe/amd64/base-g1", "pe-base-g1", "std")
	manifestPath := writeTestJSON(t, t.TempDir(), "manifest.json", manifest)
	common := &CommonConfig{ProxmoxURL: server.URL + "/api2/json", ProxmoxNode: "pve01", ProxmoxInsecure: true}
	if _, err := StampPVEOutput(context.Background(), common, manifest, manifestPath, "user@pve!factory", "secret"); err == nil || !strings.Contains(err.Error(), "read-back mismatch") {
		t.Fatalf("accepted drifted PVE output stamp: %v", err)
	}
}

func TestPVEVGAModelAcceptsAPIRepresentations(t *testing.T) {
	for raw, want := range map[string]string{
		"":                   "std",
		"std,memory=64":      "std",
		"memory=64,type=std": "std",
		"type=qxl,memory=64": "qxl",
	} {
		if got := pveVGAModel(raw); got != want {
			t.Errorf("pveVGAModel(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestStampPVEOutputRejectsVGADrift(t *testing.T) {
	stampedDescription := ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/cluster/resources"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":9200,"node":"pve01","name":"pe-desktop-g1","template":1,"type":"qemu"}]}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/config"):
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			stampedDescription = r.Form.Get("description")
			_, _ = w.Write([]byte(`{"data":null}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = fmt.Fprintf(w, `{"data":{"description":%q,"ciupgrade":0,"ciuser":"root","ipconfig0":"ip=dhcp","vga":"std,memory=64"}}`, stampedDescription)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manifest := stampedTestImageManifest("pe/amd64/desktop-g1", "pe-desktop-g1", "qxl")
	manifestPath := writeTestJSON(t, t.TempDir(), "manifest.json", manifest)
	common := &CommonConfig{ProxmoxURL: server.URL + "/api2/json", ProxmoxNode: "pve01", ProxmoxInsecure: true}
	if _, err := StampPVEOutput(context.Background(), common, manifest, manifestPath, "user@pve!factory", "secret"); err == nil || !strings.Contains(err.Error(), "read-back mismatch") {
		t.Fatalf("accepted drifted PVE VGA: %v", err)
	}
}

func stampedTestImageManifest(imageID, template, displayModel string) *ImageManifest {
	digest := "sha256:" + strings.Repeat("a", 64)
	return &ImageManifest{
		SchemaVersion: 1, CreatedAt: time.Unix(100, 0).UTC(), Target: "base-systemd", ImageID: imageID, Generation: "g1",
		Provider: "pve", Arch: "amd64", BuildMode: "native-gentoo", SourceTemplate: "seed", SourceVMID: 9000,
		SourceProvenanceObjectID: "seed/test", SourceProvenanceDigest: digest, Template: template, ProfileID: "pe/amd64/base-v1",
		ProfilePath: "default/linux/amd64/23.0/systemd", ProfileRepository: "gentoo", PackageSets: []string{"pe/runtime-v1"},
		PackageSetCatalogDigest: digest, MirrorBundleID: "mirror/test", Repositories: map[string]string{"gentoo": strings.Repeat("1", 40)},
		RootfsSource: "approved-qcow2", DisplayModel: displayModel, Channel: "candidate", InputLockDigest: digest,
		CommonConfigDigest: digest, BuildPlanDigest: digest, PackerManifestDigest: digest, PackerArtifactID: "9200",
		ImageDigest: digest, RootfsManifestDigest: digest,
	}
}

func TestRecoverablePVEManifestRejectsTampering(t *testing.T) {
	manifest := stampedTestImageManifest("pe/amd64/base-g1", "pe-base-g1", "std")
	manifestPath := writeTestJSON(t, t.TempDir(), "manifest.json", manifest)
	description, digest, err := stampedPVEManifestDescription(manifest, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(description, pveManifestField+"=", pveManifestField+"=A", 1)
	if _, _, err := decodePVEManifestDescription(tampered, digest); err == nil {
		t.Fatal("accepted a tampered recoverable PVE manifest")
	}
}

func TestStampedPVEManifestRejectsDescriptionOverflow(t *testing.T) {
	manifest := stampedTestImageManifest(strings.Repeat("a", maxPVEVMDescriptionBytes), "pe-base-g1", "std")
	manifestPath := writeTestJSON(t, t.TempDir(), "manifest.json", manifest)
	if _, _, err := stampedPVEManifestDescription(manifest, manifestPath); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("accepted a recoverable PVE stamp above the description limit: %v", err)
	}
}

func TestCheckPVESourceRequiresRecoverableImageManifest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"template":1,"name":"pe-base-g1","description":"approved | portage-engine-provenance=` + digest + `","ciupgrade":0,"ciuser":"root","ipconfig0":"ip=dhcp","vga":"std"}}`))
	}))
	defer server.Close()
	plan := testBuildPlan()
	plan.RootfsSource = "packer-base-image"
	plan.SourceTemplate = "pe-base-g1"
	plan.SourceDisplayModel = "std"
	common := &CommonConfig{ProxmoxURL: server.URL, ProxmoxNode: "pve01", ProxmoxInsecure: true}
	evidence := &PlanEvidence{SourceProvenanceDigest: digest}
	if err := CheckPVESource(context.Background(), common, &plan, evidence, "user@pve!factory", "secret"); err == nil || !strings.Contains(err.Error(), "recoverable approved image manifest") {
		t.Fatalf("accepted an image-derived PVE source without a recoverable manifest: %v", err)
	}
}

func TestCheckPVESourceRequiresTemplateProvenance(t *testing.T) {
	digestBytes := sha256.Sum256([]byte("seed"))
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=user@pve!factory=secret" {
			t.Errorf("unexpected authorization header")
		}
		_, _ = w.Write([]byte(`{"data":{"template":1,"name":"gentoo-seed","description":"approved | portage-engine-provenance=` + digest + `","ciupgrade":0,"ciuser":"root","ipconfig0":"ip=dhcp"}}`))
	}))
	defer server.Close()
	common := &CommonConfig{ProxmoxURL: server.URL + "/api2/json", ProxmoxNode: "pve01", ProxmoxInsecure: true}
	plan := testBuildPlan()
	evidence := &PlanEvidence{SourceProvenanceDigest: digest}
	if err := CheckPVESource(context.Background(), common, &plan, evidence, "user@pve!factory", "secret"); err != nil {
		t.Fatal(err)
	}
	evidence.SourceProvenanceDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := CheckPVESource(context.Background(), common, &plan, evidence, "user@pve!factory", "secret"); err == nil {
		t.Fatal("accepted a source template with the wrong provenance marker")
	}
}

func TestCheckPVESourceBindsDisplayModel(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"template":1,"name":"gentoo-seed","description":"approved | portage-engine-provenance=` + digest + `","ciupgrade":0,"ciuser":"root","ipconfig0":"ip=dhcp","vga":"memory=64,type=std"}}`))
	}))
	defer server.Close()
	common := &CommonConfig{ProxmoxURL: server.URL + "/api2/json", ProxmoxNode: "pve01", ProxmoxInsecure: true}
	plan := testBuildPlan()
	plan.SourceDisplayModel = "qxl"
	evidence := &PlanEvidence{SourceProvenanceDigest: digest}
	if err := CheckPVESource(context.Background(), common, &plan, evidence, "user@pve!factory", "secret"); err == nil || !strings.Contains(err.Error(), "display model") {
		t.Fatalf("accepted source template display drift: %v", err)
	}
}

func TestCheckPVESourceRejectsImplicitCloudInitUpgrade(t *testing.T) {
	digestBytes := sha256.Sum256([]byte("seed"))
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"template":1,"name":"gentoo-seed","description":"approved | portage-engine-provenance=` + digest + `","ciupgrade":1,"ciuser":"root","ipconfig0":"ip=dhcp"}}`))
	}))
	defer server.Close()
	common := &CommonConfig{ProxmoxURL: server.URL + "/api2/json", ProxmoxNode: "pve01", ProxmoxInsecure: true}
	plan := testBuildPlan()
	evidence := &PlanEvidence{SourceProvenanceDigest: digest}
	if err := CheckPVESource(context.Background(), common, &plan, evidence, "user@pve!factory", "secret"); err == nil || !strings.Contains(err.Error(), "ciupgrade=0") {
		t.Fatalf("accepted source template with implicit package upgrades: %v", err)
	}
}

func TestCheckPVESourceRejectsMissingDHCPContract(t *testing.T) {
	digestBytes := sha256.Sum256([]byte("seed"))
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"template":1,"name":"gentoo-seed","description":"approved | portage-engine-provenance=` + digest + `","ciupgrade":0,"ciuser":"root"}}`))
	}))
	defer server.Close()
	common := &CommonConfig{ProxmoxURL: server.URL + "/api2/json", ProxmoxNode: "pve01", ProxmoxInsecure: true}
	plan := testBuildPlan()
	evidence := &PlanEvidence{SourceProvenanceDigest: digest}
	if err := CheckPVESource(context.Background(), common, &plan, evidence, "user@pve!factory", "secret"); err == nil || !strings.Contains(err.Error(), "ipconfig0=ip=dhcp") {
		t.Fatalf("accepted source template without a DHCP cloud-init contract: %v", err)
	}
}

func TestCheckPVESourceRejectsInsufficientNodeMemoryHeadroom(t *testing.T) {
	digestBytes := sha256.Sum256([]byte("seed"))
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/qemu/9000/config"):
			_, _ = w.Write([]byte(`{"data":{"template":1,"name":"gentoo-seed","description":"approved | portage-engine-provenance=` + digest + `","ciupgrade":0,"ciuser":"root","ipconfig0":"ip=dhcp"}}`))
		case strings.HasSuffix(r.URL.Path, "/nodes/pve01/status"):
			_, _ = w.Write([]byte(`{"data":{"memory":{"free":8589934592,"total":34359738368}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	common := &CommonConfig{ProxmoxURL: server.URL + "/api2/json", ProxmoxNode: "pve01", ProxmoxInsecure: true, ProxmoxHostMemoryHeadroomMB: 4096}
	plan := testBuildPlan()
	plan.Memory = 8192
	evidence := &PlanEvidence{SourceProvenanceDigest: digest}
	if err := CheckPVESource(context.Background(), common, &plan, evidence, "user@pve!factory", "secret"); err == nil || !strings.Contains(err.Error(), "host headroom") {
		t.Fatalf("accepted an image build that would overcommit the PVE host: %v", err)
	}
}
