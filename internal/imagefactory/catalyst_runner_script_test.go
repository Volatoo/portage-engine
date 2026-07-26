package imagefactory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalystRunnerImportsSnapshotBundlesWithoutFetch(t *testing.T) {
	t.Parallel()
	scriptPath := filepath.Join("..", "..", "image-factory", "catalyst", "run-offline.sh")
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(script)
	for _, required := range []string{
		`bundle unbundle "${repository_bundle}"`,
		`>"${gentoo_repo}/shallow"`,
		`bundle unbundle "${profile_repository_bundle}"`,
		`>"${profile_repository_source}/.git/shallow"`,
		`-type d -exec chmod 0755 {} +`,
		`-type f -exec chmod 0644 {} +`,
		`chmod 0755 -- "${distdir}"`,
		`find "${distdir}" -mindepth 1 -maxdepth 1 -type f -exec chmod 0644 -- {} +`,
		`hydrate_script=${CATALYST_HYDRATE_SCRIPT:-${script_dir}/../packer/scripts/hydrate-distfiles.py}`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("Catalyst runner lost shallow-bundle import guard %q", required)
		}
	}
	if strings.Contains(contents, `fetch --no-tags "${repository_bundle}"`) ||
		strings.Contains(contents, `fetch --no-tags "${profile_repository_bundle}"`) {
		t.Fatal("Catalyst runner must not fetch a deliberately shallow snapshot bundle")
	}
	if strings.Contains(contents, `${repo_root}/image-factory/packer/scripts/hydrate-distfiles.py`) {
		t.Fatal("Catalyst runner must resolve the hydrator in both source and offline bundle layouts")
	}
}

func TestCatalystAssemblerUsesPortableLoopDetachSyntax(t *testing.T) {
	t.Parallel()
	scriptPath := filepath.Join("..", "..", "image-factory", "catalyst", "assemble-qcow2.sh")
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(script)
	if strings.Contains(contents, `losetup -d -- "${loop_device}"`) {
		t.Fatal("losetup treats -- as a device on supported PVE hosts")
	}
	if got := strings.Count(contents, `losetup -d "${loop_device}"`); got != 2 {
		t.Fatalf("expected explicit and cleanup loop detach calls, got %d", got)
	}
	for _, required := range []string{
		`for candidate in /boot/vmlinuz-* /boot/kernel-*; do`,
		`test "$kernel_count" -eq 1`,
		`chmod 0644 -- "${mount_root}/etc/machine-id"`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("assembler lost Gentoo kernel filename guard %q", required)
		}
	}
}

func TestPackerProvisionerPreservesGitOwnershipProtection(t *testing.T) {
	t.Parallel()
	scriptPath := filepath.Join("..", "..", "image-factory", "packer", "scripts", "provision.sh")
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(script)
	if !strings.Contains(contents, `chown -R root:root -- "$repo"`) {
		t.Fatal("Packer provisioner must normalize inherited Catalyst repository ownership")
	}
	if strings.Contains(contents, "git config --global --add safe.directory") {
		t.Fatal("Packer provisioner must not bypass Git's dubious-ownership protection")
	}
}

func TestCatalystSeedImporterDisablesCloudInitUpgrade(t *testing.T) {
	t.Parallel()
	scriptPath := filepath.Join("..", "..", "image-factory", "catalyst", "import-pve-seed.sh")
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`--ciupgrade 0`, `--ciuser root`, `--ipconfig0 ip=dhcp`} {
		if !strings.Contains(string(script), required) {
			t.Fatalf("Catalyst PVE seed importer lost cloud-init contract %q", required)
		}
	}
}
