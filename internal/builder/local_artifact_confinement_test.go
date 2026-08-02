package builder

import (
	"os"
	"path/filepath"
	"testing"
)

// A native builder runs emerge as root against ebuild code that may come from
// an operator-supplied overlay, and that code can write anything it likes into
// PKGDIR. The three tests below drive that attacker: a name that looks like a
// produced package but is really a link onto the builder host must not be
// collected, must not be copied, and must never be resolved into a path the
// control plane then streams and publishes.

func TestBuildOutputSymlinkIsNotCollectedAsAnArtifact(t *testing.T) {
	parent := t.TempDir()
	pkgDir := filepath.Join(parent, "binpkgs")
	if err := os.MkdirAll(filepath.Join(pkgDir, "app-misc"), 0o750); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "worker.key")
	if err := os.WriteFile(secret, []byte("worker private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join("app-misc", "jq-1.8.1-1.gpkg.tar")
	if err := os.WriteFile(filepath.Join(pkgDir, real), []byte("real package"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join("app-misc", "exfil-1.gpkg.tar")
	if err := os.Symlink(secret, filepath.Join(pkgDir, planted)); err != nil {
		t.Fatal(err)
	}
	// A relative link out of PKGDIR is the same attack without an absolute path.
	relPlanted := filepath.Join("app-misc", "exfil-2.gpkg.tar")
	if err := os.Symlink(filepath.Join("..", "..", "worker.key"), filepath.Join(pkgDir, relPlanted)); err != nil {
		t.Fatal(err)
	}

	lb := &LocalBuilder{artifactDir: filepath.Join(parent, "artifacts")}
	rels, err := lb.waitForArtifacts(pkgDir)
	if err != nil {
		t.Fatalf("waitForArtifacts: %v", err)
	}
	for _, rel := range rels {
		if rel == planted || rel == relPlanted {
			t.Fatalf("a symlink out of PKGDIR was recorded as a produced artifact: %q", rel)
		}
	}
	if len(rels) != 1 || rels[0] != real {
		t.Fatalf("expected only the real package, got %q", rels)
	}
}

func TestArtifactCopyRefusesToFollowLinksOutOfEitherTree(t *testing.T) {
	parent := t.TempDir()
	pkgDir := filepath.Join(parent, "binpkgs")
	artifactDir := filepath.Join(parent, "artifacts")
	for _, dir := range []string{
		filepath.Join(pkgDir, "app-misc"),
		filepath.Join(artifactDir, "app-misc"),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(parent, "worker.key")
	if err := os.WriteFile(secret, []byte("worker private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	lb := &LocalBuilder{artifactDir: artifactDir}

	// The source entry is swapped for a link after collection listed it: the
	// copy itself must not dereference it into the artifact dir.
	const exfil = "app-misc/exfil-1.gpkg.tar"
	if err := os.Symlink(secret, filepath.Join(pkgDir, filepath.FromSlash(exfil))); err != nil {
		t.Fatal(err)
	}
	if err := lb.copyArtifacts(pkgDir, []string{exfil}); err == nil {
		t.Fatal("copying a PKGDIR symlink into the artifact dir was allowed")
	}
	if body, err := os.ReadFile(filepath.Join(artifactDir, filepath.FromSlash(exfil))); err == nil {
		t.Fatalf("host file was laundered into the artifact dir: %q", body)
	}

	// The destination entry is a link out of the artifact dir: a copy must not
	// write through it as the (root) build service.
	const overwrite = "app-misc/jq-1.8.1-1.gpkg.tar"
	victim := filepath.Join(parent, "authorized_keys")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, filepath.FromSlash(overwrite)), []byte("attacker bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(artifactDir, filepath.FromSlash(overwrite))); err != nil {
		t.Fatal(err)
	}
	if err := lb.copyArtifacts(pkgDir, []string{overwrite}); err == nil {
		t.Fatal("copy wrote through a symlink out of the artifact dir")
	}
	body, err := os.ReadFile(victim)
	if err != nil || string(body) != "original" {
		t.Fatalf("file outside the artifact dir was overwritten: %q %v", body, err)
	}
}

func TestArtifactPathByRelRefusesLinksOutOfTheArtifactDir(t *testing.T) {
	parent := t.TempDir()
	artifactDir := filepath.Join(parent, "artifacts")
	if err := os.MkdirAll(filepath.Join(artifactDir, "app-misc"), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "worker.key")
	if err := os.WriteFile(secret, []byte("worker private key"), 0o600); err != nil {
		t.Fatal(err)
	}

	const named = "app-misc/exfil-1.gpkg.tar"
	if err := os.Symlink(secret, filepath.Join(artifactDir, filepath.FromSlash(named))); err != nil {
		t.Fatal(err)
	}
	// The escape can also sit in a directory component rather than the leaf.
	const traversed = "sys-libs/worker.key"
	if err := os.Symlink(outside, filepath.Join(artifactDir, "sys-libs")); err != nil {
		t.Fatal(err)
	}
	const served = "app-misc/jq-1.8.1-1.gpkg.tar"
	if err := os.WriteFile(filepath.Join(artifactDir, filepath.FromSlash(served)), []byte("real package"), 0o600); err != nil {
		t.Fatal(err)
	}

	lb := &LocalBuilder{
		artifactDir: artifactDir,
		jobs: map[string]*BuildJob{
			"job-1": {
				ID:        "job-1",
				Status:    "success",
				Artifacts: []string{named, traversed, served},
			},
		},
	}
	for _, rel := range []string{named, traversed} {
		path, err := lb.GetArtifactPathByRel("job-1", rel)
		if err == nil {
			t.Fatalf("artifact %q resolved out of the artifact dir to %q", rel, path)
		}
	}
	// The legitimate package must still resolve: this gate rejects escapes, not
	// the packages every real build ships.
	path, err := lb.GetArtifactPathByRel("job-1", served)
	if err != nil || path != filepath.Join(artifactDir, filepath.FromSlash(served)) {
		t.Fatalf("a real produced artifact stopped resolving: %q %v", path, err)
	}
}
