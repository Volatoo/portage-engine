package builder

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The config-bundle path reaches the same artifact dir as the native path, by a
// different collector: worker() picks executeConfigBundleBuild whenever the
// request carries a bundle, and that ends in BuildExecutor.collectArtifacts.
// emerge has just run ebuild code as root by then, so these tests drive the
// same attacker the native collector already refuses - a name in PKGDIR that
// looks like a produced package but is a link onto the builder host, and a name
// in the artifact dir that turns the copy into a write somewhere else.

// jqPackage is the one real package every case below starts from: emerge wrote
// it, it is a regular file inside PKGDIR, and it must survive every rule these
// tests add.
const jqPackage = "app-misc/jq/jq-1.8.2-1.gpkg.tar"

func writeJQPackage(t *testing.T, base, body string) {
	t.Helper()
	path := filepath.Join(base, filepath.FromSlash(jqPackage))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestExecutorCollectionRefusesBuildOutputSymlinks(t *testing.T) {
	parent := t.TempDir()
	pkgDir := filepath.Join(parent, "job-a", "binpkgs")
	artifactDir := filepath.Join(parent, "artifacts")
	secret := filepath.Join(parent, "worker.key")
	if err := os.WriteFile(secret, []byte("worker private key"), 0o600); err != nil {
		t.Fatal(err)
	}

	writeJQPackage(t, pkgDir, "real package")
	const planted = "app-misc/jq/exfil-1.gpkg.tar"
	if err := os.Symlink(secret, filepath.Join(pkgDir, filepath.FromSlash(planted))); err != nil {
		t.Fatal(err)
	}
	// A relative link out of PKGDIR is the same attack without an absolute path.
	const relPlanted = "app-misc/jq/exfil-2.gpkg.tar"
	escape := filepath.Join("..", "..", "..", "..", "worker.key")
	if err := os.Symlink(escape, filepath.Join(pkgDir, filepath.FromSlash(relPlanted))); err != nil {
		t.Fatal(err)
	}

	job := &BuildJob{ID: "job-a", Request: &LocalBuildRequest{PackageName: "app-misc/jq"}}
	be := NewBuildExecutor(filepath.Join(parent, "work"), artifactDir)
	if err := be.collectArtifacts(pkgDir, job); err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}

	if got := job.artifactsSnapshot(); !slices.Equal(got, []string{filepath.FromSlash(jqPackage)}) {
		t.Fatalf("job artifacts = %v, want only the real package", got)
	}
	for _, rel := range []string{planted, relPlanted} {
		if body, err := os.ReadFile(filepath.Join(artifactDir, filepath.FromSlash(rel))); err == nil {
			t.Fatalf("host secret was published as build artifact %q: %q", rel, body)
		}
	}
}

func TestExecutorCollectionRefusesToWriteThroughArtifactDirLinks(t *testing.T) {
	parent := t.TempDir()
	pkgDir := filepath.Join(parent, "job-a", "binpkgs")
	artifactDir := filepath.Join(parent, "artifacts")
	writeJQPackage(t, pkgDir, "attacker bytes")

	victim := filepath.Join(parent, "authorized_keys")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(artifactDir, "app-misc", "jq"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(artifactDir, filepath.FromSlash(jqPackage))); err != nil {
		t.Fatal(err)
	}

	job := &BuildJob{ID: "job-a", Request: &LocalBuildRequest{PackageName: "app-misc/jq"}}
	be := NewBuildExecutor(filepath.Join(parent, "work"), artifactDir)
	if err := be.collectArtifacts(pkgDir, job); err == nil {
		t.Fatal("collection wrote through a symlink out of the artifact dir")
	}
	if body, err := os.ReadFile(victim); err != nil || string(body) != "original" {
		t.Fatalf("file outside the artifact dir was overwritten: %q %v", body, err)
	}
}

func TestExecutorCollectionRefusesArtifactDirCategoryLinks(t *testing.T) {
	parent := t.TempDir()
	pkgDir := filepath.Join(parent, "job-a", "binpkgs")
	artifactDir := filepath.Join(parent, "artifacts")
	writeJQPackage(t, pkgDir, "attacker bytes")

	// The escape sits in a directory component rather than the leaf: creating
	// the category directory must not follow it out of the artifact dir either.
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(artifactDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(artifactDir, "app-misc")); err != nil {
		t.Fatal(err)
	}

	job := &BuildJob{ID: "job-a", Request: &LocalBuildRequest{PackageName: "app-misc/jq"}}
	be := NewBuildExecutor(filepath.Join(parent, "work"), artifactDir)
	if err := be.collectArtifacts(pkgDir, job); err == nil {
		t.Fatal("collection created the artifact tree through a symlinked category")
	}
	if _, err := os.Stat(filepath.Join(outside, "jq", "jq-1.8.2-1.gpkg.tar")); err == nil {
		t.Fatal("build output was written outside the artifact dir")
	}
}

func TestExecutorCollectionKeepsPublishingInTreePackageLinks(t *testing.T) {
	// A PKGDIR entry that points at another file in the same PKGDIR is a real
	// layout, not an escape, and must keep being collected and published.
	parent := t.TempDir()
	pkgDir := filepath.Join(parent, "job-a", "binpkgs")
	artifactDir := filepath.Join(parent, "artifacts")
	writeJQPackage(t, pkgDir, "real package")
	const alias = "app-misc/jq/jq-1.8.2-2.gpkg.tar"
	if err := os.Symlink("jq-1.8.2-1.gpkg.tar", filepath.Join(pkgDir, filepath.FromSlash(alias))); err != nil {
		t.Fatal(err)
	}

	job := &BuildJob{ID: "job-a", Request: &LocalBuildRequest{PackageName: "app-misc/jq"}}
	be := NewBuildExecutor(filepath.Join(parent, "work"), artifactDir)
	if err := be.collectArtifacts(pkgDir, job); err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}
	want := []string{filepath.FromSlash(jqPackage), filepath.FromSlash(alias)}
	slices.Sort(want)
	if got := job.artifactsSnapshot(); !slices.Equal(got, want) {
		t.Fatalf("job artifacts = %v, want %v", got, want)
	}
	for _, rel := range want {
		body, err := os.ReadFile(filepath.Join(artifactDir, rel))
		if err != nil || string(body) != "real package" {
			t.Fatalf("artifact %q was not published: %q %v", rel, body, err)
		}
	}
}
