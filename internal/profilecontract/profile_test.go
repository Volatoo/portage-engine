package profilecontract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeProfileFixture(t *testing.T) (string, Selection) {
	t.Helper()
	repository := t.TempDir()
	profileDir := filepath.Join(repository, "profiles", "portage-engine", "amd64", "systemd", "base")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "profiles", "repo_name"), []byte("pe-profiles\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "parent"), []byte("# approved parent\ngentoo:default/linux/amd64/23.0/systemd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repository, Selection{RepositoryName: "pe-profiles", RepositoryRoot: repository, ProfilePath: "portage-engine/amd64/systemd/base",
		Parents: []Parent{{RepositoryName: "gentoo", ProfilePath: "default/linux/amd64/23.0/systemd"}}, VerifyExactParents: true}
}

func TestVerifyProfileSelection(t *testing.T) {
	repository, selection := makeProfileFixture(t)
	got, err := Verify(selection)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(repository, "profiles", filepath.FromSlash(selection.ProfilePath))
	want, err = filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Verify() = %q, want %q", got, want)
	}
}

func TestVerifyRejectsParentDriftAndEscape(t *testing.T) {
	repository, selection := makeProfileFixture(t)
	selection.Parents[0].ProfilePath = "default/linux/amd64/23.0"
	if _, err := Verify(selection); err == nil {
		t.Fatal("accepted a profile parent different from the catalog")
	}

	outside := t.TempDir()
	link := filepath.Join(repository, "profiles", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	selection.ProfilePath = "escape"
	selection.Parents = nil
	if _, err := Verify(selection); err == nil {
		t.Fatal("accepted a profile symlink escaping the repository")
	}
}

func TestRepositoryTemplateProfiles(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", "..", "profile-repository"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []Selection{
		{RepositoryName: "pe-profiles", RepositoryRoot: repository, ProfilePath: "portage-engine/amd64/23.0/systemd/base", VerifyExactParents: true,
			Parents: []Parent{{RepositoryName: "gentoo", ProfilePath: "default/linux/amd64/23.0/systemd"}}},
		{RepositoryName: "pe-profiles", RepositoryRoot: repository, ProfilePath: "portage-engine/amd64/23.0/systemd/desktop-verifier", VerifyExactParents: true,
			Parents: []Parent{{RepositoryName: "pe-profiles", ProfilePath: "portage-engine/amd64/23.0/systemd/base"}}},
		{RepositoryName: "pe-profiles", RepositoryRoot: repository, ProfilePath: "portage-engine/amd64/23.0/no-multilib/systemd/base", VerifyExactParents: true,
			Parents: []Parent{{RepositoryName: "gentoo", ProfilePath: "default/linux/amd64/23.0/no-multilib/systemd"}}},
		{RepositoryName: "pe-profiles", RepositoryRoot: repository, ProfilePath: "portage-engine/amd64/23.0/no-multilib/systemd/desktop-verifier", VerifyExactParents: true,
			Parents: []Parent{{RepositoryName: "pe-profiles", ProfilePath: "portage-engine/amd64/23.0/no-multilib/systemd/base"}}},
	}
	for _, selection := range tests {
		profileDir, err := Verify(selection)
		if err != nil {
			t.Fatalf("template profile %s: %v", selection.ProfilePath, err)
		}
		if strings.HasSuffix(selection.ProfilePath, "/desktop-verifier") {
			defaults, err := os.ReadFile(filepath.Join(profileDir, "make.defaults"))
			if err != nil {
				t.Fatalf("desktop policy %s: %v", selection.ProfilePath, err)
			}
			if string(defaults) != "USE=\"X harfbuzz gtk gtk3 policykit udisks\"\nINPUT_DEVICES=\"-* libinput\"\nVIDEO_CARDS=\"-* qxl\"\n" {
				t.Fatalf("desktop profile %s lost deterministic X policy", selection.ProfilePath)
			}
			packageUse, err := os.ReadFile(filepath.Join(profileDir, "package.use"))
			if err != nil {
				t.Fatalf("desktop package policy %s: %v", selection.ProfilePath, err)
			}
			if string(packageUse) != "x11-base/xorg-server xvfb\n" {
				t.Fatalf("desktop profile %s lost its Xvfb package policy", selection.ProfilePath)
			}
		}
	}
}
