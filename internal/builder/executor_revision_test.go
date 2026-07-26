package builder

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRevisionMatches(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		actual   string
		want     bool
	}{
		{name: "full commit", expected: "0123456789abcdef0123456789abcdef01234567", actual: "0123456789abcdef0123456789abcdef01234567\n", want: true},
		{name: "abbreviated commit", expected: "0123456", actual: "0123456789abcdef0123456789abcdef01234567", want: true},
		{name: "case insensitive hex", expected: "ABCDEF0", actual: "abcdef0123456789", want: true},
		{name: "different commit", expected: "7654321", actual: "0123456789abcdef", want: false},
		{name: "too short", expected: "012345", actual: "0123456789abcdef", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := revisionMatches(tt.expected, tt.actual); got != tt.want {
				t.Fatalf("revisionMatches(%q, %q) = %v, want %v", tt.expected, tt.actual, got, tt.want)
			}
		})
	}
}

func TestValidateRepositoryStateRejectsDirtyTree(t *testing.T) {
	expected := "0123456789abcdef0123456789abcdef01234567"
	if err := validateRepositoryState(expected, expected+"\n", ""); err != nil {
		t.Fatalf("clean repository rejected: %v", err)
	}
	if err := validateRepositoryState(expected, expected+"\n", " M profiles/package.mask\n"); err == nil {
		t.Fatal("dirty tracked file accepted")
	}
	if err := validateRepositoryState(expected, expected+"\n", "?? profiles/evil\n"); err == nil {
		t.Fatal("untracked file accepted")
	}
}

func TestCollectArtifactsPublishesJobDependencyClosureOnly(t *testing.T) {
	root := t.TempDir()
	artifactDir := filepath.Join(root, "artifacts")
	pkgDir := filepath.Join(root, "job-a", "binpkgs")
	writePackage := func(base, rel string) {
		t.Helper()
		path := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{
		"app-misc/jq/jq-1.8.2-1.gpkg.tar",
		"dev-libs/oniguruma/oniguruma-6.9.10-1.gpkg.tar",
	}
	for _, rel := range want {
		writePackage(pkgDir, rel)
	}
	writePackage(artifactDir, "app-misc/jq/jq-1.7.1-9.gpkg.tar") // stale warm-pool output

	job := &BuildJob{ID: "job-a", Request: &LocalBuildRequest{PackageName: "app-misc/jq"}}
	be := NewBuildExecutor(filepath.Join(root, "work"), artifactDir)
	if err := be.collectArtifacts(pkgDir, job); err != nil {
		t.Fatal(err)
	}
	if got := job.artifactsSnapshot(); !slices.Equal(got, want) {
		t.Fatalf("job artifacts = %v, want complete job-local closure %v", got, want)
	}
	if got := job.ArtifactURL; got != filepath.Join(artifactDir, want[0]) {
		t.Fatalf("primary artifact = %q, want jq artifact", got)
	}
	for _, rel := range want {
		if _, err := os.Stat(filepath.Join(artifactDir, rel)); err != nil {
			t.Fatalf("published dependency closure missing %s: %v", rel, err)
		}
	}
}
