package imagefactory

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testCatalystPlan() CatalystPlan {
	return CatalystPlan{SchemaVersion: 2, Target: catalystTarget, PlanObjectID: "catalyst-plan/base-systemd-g1", RootfsID: "rootfs/catalyst-base-systemd-g1",
		Generation: "g1", Arch: "amd64", Subarch: "amd64", RelType: "default", VersionStamp: "pe-g1", SnapshotID: "pe-g1-111111111111",
		ProfileID: "pe/amd64/glibc/systemd/base-v1", ProfilePath: catalystOfficialProfile, ProfileRepository: "gentoo", MirrorBundleID: "mirror/catalyst-test",
		Repositories: map[string]string{"gentoo": strings.Repeat("1", 40)}, RepositoryObjectID: "repository/gentoo-g1", GentooRepositoryKeyObjectID: "release-key/gentoo-repository", Stage3ObjectID: "stage3/amd64-systemd-g1",
		Stage3DigestsObjectID: "stage3-digests/amd64-systemd-g1", ReleaseKeyObjectID: "release-key/gentoo", CatalystRuntimeObjectID: "catalyst-runtime/test",
		DistfileManifestObjectID: "distfiles/catalyst-base-systemd", PackageSetCatalogObjectID: "package-sets/image-v1", PackageSets: []string{"pe/catalyst-boot-v1"},
		Packages: []string{"app-misc/hello"}, RuntimeGentooRepositoryURI: "https://git.internal/gentoo.git", RuntimeGentooMirror: "https://dist.internal",
		SeedFilename: "stage3-amd64-systemd-test.tar.xz", OutputFilename: "stage4-amd64-pe-g1.tar.xz", QCOW2Filename: "stage4-amd64-pe-g1.qcow2", DiskSizeGiB: 16, Jobs: 4}
}

func prepareCatalystFixture(t *testing.T) (CatalystPlan, string, string, string, *CatalystPrepared) {
	t.Helper()
	return prepareCatalystFixtureForPlan(t, testCatalystPlan())
}

func prepareCatalystFixtureForPlan(t *testing.T, plan CatalystPlan) (CatalystPlan, string, string, string, *CatalystPrepared) {
	t.Helper()
	dir := t.TempDir()
	planPath := writeTestJSON(t, dir, "catalyst-plan.json", plan)
	write := func(name, value string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	repoPath := write("gentoo.bundle", "bundle")
	repoKeyPath := write("gentoo-repository.asc", "repository key")
	stage3Path := write(plan.SeedFilename, "stage3")
	digestsPath := write("stage3.DIGESTS.asc", "signed digests")
	keyPath := write("gentoo-release.asc", "key")
	runtimePath := write("catalyst-runtime.tar.xz", "runtime")
	closurePath := writeTestJSON(t, dir, "distfiles.json", ClosureManifest{SchemaVersion: 1, Target: plan.Target, RepositoryCommit: plan.Repositories["gentoo"], Objects: []ClosureObject{{
		Filename: "hello.tar.xz", URI: "https://dist.internal/hello.tar.xz", SHA256: strings.Repeat("a", 64), Size: 7}}})
	catalogPath := writeTestJSON(t, dir, "package-sets.json", PackageSetCatalog{SchemaVersion: 1, CatalogID: "pe/image-package-sets-v1", Sets: []PackageSetDefinition{{
		ID: "pe/catalyst-boot-v1", Packages: []string{"app-emulation/cloud-init", "app-emulation/qemu-guest-agent", "sys-boot/grub", "sys-kernel/gentoo-kernel-bin"}}}})
	objects := []InputObject{
		objectForFile(t, plan.PlanObjectID, "catalyst-plan", planPath, []string{plan.Target}),
		objectForFile(t, plan.RepositoryObjectID, "repository-snapshot", repoPath, []string{plan.Target}),
		objectForFile(t, plan.GentooRepositoryKeyObjectID, "release-key", repoKeyPath, []string{plan.Target}),
		objectForFile(t, plan.Stage3ObjectID, "stage3", stage3Path, []string{plan.Target}),
		objectForFile(t, plan.Stage3DigestsObjectID, "stage3-digests", digestsPath, []string{plan.Target}),
		objectForFile(t, plan.ReleaseKeyObjectID, "release-key", keyPath, []string{plan.Target}),
		objectForFile(t, plan.CatalystRuntimeObjectID, "catalyst-runtime", runtimePath, []string{plan.Target}),
		objectForFile(t, plan.DistfileManifestObjectID, "distfile-manifest", closurePath, []string{plan.Target}),
		objectForFile(t, plan.PackageSetCatalogObjectID, "package-set-catalog", catalogPath, []string{plan.Target}),
	}
	if plan.Target == catalystProfileTarget {
		profileBundlePath := write("pe-profiles.bundle", "profile bundle")
		profileKeyPath := write("pe-profiles-release.asc", "profile key")
		objects = append(objects,
			objectForFile(t, plan.ProfileRepositoryObjectID, "repository-snapshot", profileBundlePath, []string{plan.Target}),
			objectForFile(t, plan.ProfileRepositoryKeyObjectID, "release-key", profileKeyPath, []string{plan.Target}))
	}
	for index := range objects {
		objects[index].Path = filepath.Base(objects[index].Path)
		if objects[index].Kind == "catalyst-runtime" {
			objects[index].Platform = runtime.GOOS + "-" + runtime.GOARCH
		}
	}
	lockPath := writeTestJSON(t, dir, "lock.json", InputLock{Version: 1, BundleID: plan.MirrorBundleID, StrictOffline: true,
		AllowedHosts: []string{"git.internal", "dist.internal"}, Objects: objects})
	workRoot := filepath.Join(dir, "work")
	prepared, err := PrepareCatalystPlan(planPath, lockPath, dir, workRoot)
	if err != nil {
		t.Fatal(err)
	}
	return plan, planPath, lockPath, dir, prepared
}

func testCatalystExternalProfilePlan() CatalystPlan {
	plan := testCatalystPlan()
	plan.Target = catalystProfileTarget
	plan.PlanObjectID = "catalyst-plan/profile-systemd-g1"
	plan.RootfsID = "rootfs/catalyst-profile-systemd-g1"
	plan.ProfileRepository = "pe-profiles"
	plan.ProfilePath = "portage-engine/amd64/23.0/systemd/base"
	plan.ProfileParents = []CatalystProfileParent{{Repository: "gentoo", ProfilePath: catalystOfficialProfile}}
	plan.ProfileRepositoryObjectID = "repository/pe-profiles-g1"
	plan.ProfileRepositoryKeyObjectID = "release-key/pe-profiles"
	plan.RuntimeProfileRepositoryURI = "https://git.internal/pe-profiles.git"
	plan.Repositories["pe-profiles"] = strings.Repeat("2", 40)
	return plan
}

func TestPrepareCatalystPlanBindsInputsAndGeneratesReviewableSpec(t *testing.T) {
	previousUmask := syscall.Umask(0o077)
	defer syscall.Umask(previousUmask)
	plan, _, _, _, prepared := prepareCatalystFixture(t)
	if !slices.Contains(prepared.Packages, "sys-kernel/gentoo-kernel-bin") || !slices.Contains(prepared.Packages, "app-misc/hello") {
		t.Fatalf("package sets were not expanded: %+v", prepared.Packages)
	}
	spec, err := os.ReadFile(prepared.SpecPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(spec)
	for _, expected := range []string{"target: stage4", "profile: " + plan.ProfilePath, "snapshot_treeish: " + plan.SnapshotID, "keep_repos: gentoo", "stage4/root_overlay:", "sys-boot/grub"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("generated spec lacks %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "stage4/use:") {
		t.Fatal("generated stage4 spec must not override the selected profile's USE defaults")
	}
	config, err := os.ReadFile(prepared.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`digests = ["sha256", "sha512"]`, `options = ["bindist", "sticky-config", "versioned_cache"]`, "jobs = 4"} {
		if !strings.Contains(string(config), expected) {
			t.Fatalf("generated Catalyst TOML lacks %q:\n%s", expected, config)
		}
	}
	buildMakeConf, err := os.ReadFile(filepath.Join(prepared.PortageConfigPath, "make.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buildMakeConf), `GRUB_PLATFORMS="efi-64"`) {
		t.Fatal("Catalyst build config must select EFI GRUB without replacing profile USE defaults")
	}
	if prepared.GeneratedOverlayDigest == "" || len(prepared.Inputs) != 9 {
		t.Fatalf("prepared evidence is incomplete: %+v", prepared)
	}
	for path, expected := range map[string]os.FileMode{
		filepath.Join(prepared.RootOverlayPath, "etc"):                         0o755,
		filepath.Join(prepared.RootOverlayPath, "etc", "portage"):              0o755,
		filepath.Join(prepared.RootOverlayPath, "etc", "portage", "make.conf"): 0o644,
		filepath.Join(prepared.PortageConfigPath, "make.conf"):                 0o644,
		prepared.FSScriptPath: 0o700,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat generated path %s: %v", path, err)
		}
		if info.Mode().Perm() != expected {
			t.Fatalf("generated path %s mode = %v, want %v", path, info.Mode().Perm(), expected)
		}
	}
	for _, path := range []string{
		filepath.Join(prepared.PortageConfigPath, "package.use", "portage-engine-image"),
		filepath.Join(prepared.RootOverlayPath, "etc", "portage", "package.use", "portage-engine-image"),
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "sys-kernel/installkernel dracut\n" {
			t.Fatalf("generated kernel package policy is invalid: %s", content)
		}
	}
	fsscript, err := os.ReadFile(prepared.FSScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fsscript), "chmod 0644 /etc/machine-id") {
		t.Fatal("generated Catalyst fsscript must leave machine-id readable for systemd-networkd DHCP DUID generation")
	}
	for _, required := range []string{
		"emerge --verbose --update --deep --newuse --with-bdeps=y @world",
		"emerge --verbose --update --deep --newuse --with-bdeps=y app-emulation/cloud-init",
	} {
		if !strings.Contains(string(fsscript), required) {
			t.Fatalf("generated Catalyst fsscript does not reconcile the selected profile: missing %q", required)
		}
	}
}

func TestPrepareCatalystPlanBindsExternalProfileRepository(t *testing.T) {
	plan, _, _, _, prepared := prepareCatalystFixtureForPlan(t, testCatalystExternalProfilePlan())
	if prepared.ProfileRepository != plan.ProfileRepository || prepared.ProfileRepositoryCommit != plan.Repositories[plan.ProfileRepository] || !slices.Equal(prepared.ProfileParents, plan.ProfileParents) || len(prepared.Inputs) != 11 {
		t.Fatalf("external profile evidence is incomplete: %+v", prepared)
	}
	spec, err := os.ReadFile(prepared.SpecPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"profile: pe-profiles:" + plan.ProfilePath,
		"repos: " + prepared.ProfileRepositorySourcePath,
		"keep_repos: gentoo pe-profiles",
	} {
		if !strings.Contains(string(spec), expected) {
			t.Fatalf("external profile spec lacks %q:\n%s", expected, spec)
		}
	}
	runtimeRepo, err := os.ReadFile(filepath.Join(prepared.RootOverlayPath, "etc", "portage", "repos.conf", "pe-profiles.conf"))
	if err != nil || !strings.Contains(string(runtimeRepo), plan.RuntimeProfileRepositoryURI) {
		t.Fatalf("runtime profile repository config is missing or invalid: %v\n%s", err, runtimeRepo)
	}
	plan.ProfilePath = "../escape"
	if err := plan.Validate(); err == nil {
		t.Fatal("Catalyst external profile accepted path traversal")
	}
}

func TestCatalystPlanRejectsExternalProfileAndPublicEndpoint(t *testing.T) {
	plan := testCatalystPlan()
	plan.GentooRepositoryKeyObjectID = plan.ReleaseKeyObjectID
	if err := plan.Validate(); err == nil {
		t.Fatal("Catalyst plan accepted one object as both stage and repository trust roots")
	}
	plan = testCatalystPlan()
	plan.ProfilePath = "default/linux/amd64/23.0/desktop/plasma"
	if err := plan.Validate(); err == nil {
		t.Fatal("IMG-2 accepted an unproven external/non-systemd profile")
	}
	plan = testCatalystPlan()
	plan.SnapshotID = "stable"
	if err := plan.Validate(); err == nil {
		t.Fatal("Catalyst plan accepted the special stable treeish that triggers network fetch")
	}
	_, planPath, lockPath, dir, _ := prepareCatalystFixture(t)
	var loaded CatalystPlan
	if err := decodeStrictFile(planPath, &loaded); err != nil {
		t.Fatal(err)
	}
	loaded.RuntimeGentooMirror = "https://distfiles.gentoo.org"
	badPlanPath := writeTestJSON(t, dir, "bad-catalyst-plan.json", loaded)
	lock, err := LoadInputLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock.Objects[0] = objectForFile(t, loaded.PlanObjectID, "catalyst-plan", badPlanPath, []string{loaded.Target})
	lock.Objects[0].Path = filepath.Base(badPlanPath)
	badLockPath := writeTestJSON(t, dir, "bad-lock.json", lock)
	if _, err := PrepareCatalystPlan(badPlanPath, badLockPath, dir, filepath.Join(dir, "bad-work")); err == nil {
		t.Fatal("Catalyst plan accepted a public endpoint outside the lock allowlist")
	}
}

func TestCatalystAndQCOW2ManifestsRecheckEvidence(t *testing.T) {
	plan, planPath, lockPath, dir, prepared := prepareCatalystFixture(t)
	preparedPath := writeTestJSON(t, dir, "prepared.json", prepared)
	gatePath := writeTestJSON(t, dir, "gate.json", NewCatalystGateEvidence(prepared, time.Unix(100, 0)))
	if err := os.MkdirAll(filepath.Dir(prepared.ExpectedRootfsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prepared.ExpectedRootfsPath, []byte("rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := GenerateCatalystRootfsManifest(planPath, lockPath, preparedPath, gatePath, prepared.ExpectedRootfsPath, time.Unix(200, 0))
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := writeTestJSON(t, dir, "rootfs-manifest.json", manifest)
	qcow2Path := filepath.Join(dir, plan.QCOW2Filename)
	if err := os.WriteFile(qcow2Path, []byte("qcow2"), 0o600); err != nil {
		t.Fatal(err)
	}
	assemblerPath := filepath.Join(dir, "assembler.sh")
	if err := os.WriteFile(assemblerPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	qcow2Manifest, err := GenerateQCOW2Manifest(manifestPath, qcow2Path, assemblerPath, time.Unix(300, 0))
	if err != nil {
		t.Fatal(err)
	}
	qcow2ManifestPath := writeTestJSON(t, dir, "qcow2-manifest.json", qcow2Manifest)
	if _, err := VerifyQCOW2Artifact(qcow2ManifestPath, qcow2Path); err != nil {
		t.Fatal(err)
	}
	overlayEtc := filepath.Join(prepared.RootOverlayPath, "etc")
	if err := os.Chmod(overlayEtc, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateCatalystRootfsManifest(planPath, lockPath, preparedPath, gatePath, prepared.ExpectedRootfsPath, time.Now()); err == nil {
		t.Fatal("rootfs manifest accepted an unsafe generated overlay directory mode")
	}
	if err := os.Chmod(overlayEtc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prepared.SpecPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateCatalystRootfsManifest(planPath, lockPath, preparedPath, gatePath, prepared.ExpectedRootfsPath, time.Now()); err == nil {
		t.Fatal("rootfs manifest accepted a changed generated spec")
	}
}

func TestCatalystExternalProfileManifestRetainsBinding(t *testing.T) {
	plan, planPath, lockPath, dir, prepared := prepareCatalystFixtureForPlan(t, testCatalystExternalProfilePlan())
	preparedPath := writeTestJSON(t, dir, "prepared.json", prepared)
	gatePath := writeTestJSON(t, dir, "gate.json", NewCatalystGateEvidence(prepared, time.Unix(100, 0)))
	if err := os.MkdirAll(filepath.Dir(prepared.ExpectedRootfsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prepared.ExpectedRootfsPath, []byte("rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := GenerateCatalystRootfsManifest(planPath, lockPath, preparedPath, gatePath, prepared.ExpectedRootfsPath, time.Unix(200, 0))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ProfileRepository != plan.ProfileRepository || manifest.ProfileRepositoryCommit != plan.Repositories[plan.ProfileRepository] || !slices.Equal(manifest.ProfileParents, plan.ProfileParents) {
		t.Fatalf("external profile binding was lost: %+v", manifest)
	}
	manifestPath := writeTestJSON(t, dir, "external-rootfs-manifest.json", manifest)
	if _, err := LoadCatalystRootfsManifest(manifestPath); err != nil {
		t.Fatal(err)
	}
}
