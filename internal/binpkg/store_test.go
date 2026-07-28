package binpkg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromoteStagedPublishesFilesAndIndexTogether(t *testing.T) {
	parent := t.TempDir()
	store := NewStore(filepath.Join(parent, "binpkgs"))
	staging := filepath.Join(parent, "quarantine", "job-1")
	rel := filepath.ToSlash(filepath.Join("app-misc", "jq", "jq-1.8.1-1.gpkg.tar"))
	source := filepath.Join(staging, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("verified package"), 0o600); err != nil {
		t.Fatal(err)
	}

	paths, err := store.PromoteStaged(staging, []string{rel}, "amd64")
	if err != nil {
		t.Fatalf("PromoteStaged: %v", err)
	}
	if len(paths) != 1 || paths[0] != filepath.Join(store.BasePath(), filepath.FromSlash(rel)) {
		t.Fatalf("unexpected promoted paths: %#v", paths)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("staged replay source was removed before durable commit: %v", err)
	}
	if _, err := store.PromoteStaged(staging, []string{rel}, "amd64"); err != nil {
		t.Fatalf("exact promotion replay: %v", err)
	}
	index, err := os.ReadFile(filepath.Join(store.BasePath(), "Packages"))
	if err != nil {
		t.Fatalf("read Packages: %v", err)
	}
	if !strings.Contains(string(index), "app-misc/jq-1.8.1") {
		t.Fatalf("promoted package missing from Packages: %s", index)
	}
	if _, ok := store.Query(&QueryRequest{Name: "app-misc/jq", Arch: "amd64"}); !ok {
		t.Fatal("promoted package missing from in-memory store")
	}
}

func TestPromoteStagedRejectsTraversalWithoutPublishing(t *testing.T) {
	parent := t.TempDir()
	store := NewStore(filepath.Join(parent, "binpkgs"))
	staging := filepath.Join(parent, "quarantine", "job-1")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := store.PromoteStaged(staging, []string{"../escape.gpkg.tar"}, "amd64"); err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.gpkg.tar")); !os.IsNotExist(err) {
		t.Fatalf("traversal target was created: %v", err)
	}
}

func TestPromoteStagedDoesNotOverwritePublishedLocation(t *testing.T) {
	parent := t.TempDir()
	publicRoot := filepath.Join(parent, "binpkgs")
	store := NewStore(publicRoot)
	rel := filepath.ToSlash(filepath.Join("app-misc", "jq", "jq-1.8.1-1.gpkg.tar"))
	published := filepath.Join(publicRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(published), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(published, []byte("first immutable build"), 0o640); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(parent, "quarantine", "job-2")
	source := filepath.Join(staging, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("different rebuild"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PromoteStaged(staging, []string{rel}, "amd64"); err == nil {
		t.Fatal("expected immutable destination conflict")
	}
	got, err := os.ReadFile(published)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first immutable build" {
		t.Fatalf("published bytes were overwritten: %q", got)
	}
}

// TestNewStore tests creating a new binary package store.
func TestNewStore(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "binpkg-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	store := NewStore(tmpDir)
	if store == nil {
		t.Fatal("NewStore returned nil")
	}

	if store.basePath != tmpDir {
		t.Errorf("Expected basePath=%s, got %s", tmpDir, store.basePath)
	}
}

// TestQuery tests querying for packages.
func TestQuery(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "binpkg-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	store := NewStore(tmpDir)

	// Create a test package structure
	pkgDir := filepath.Join(tmpDir, "amd64", "dev-lang")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatalf("Failed to create package dir: %v", err)
	}

	// Create a test package file
	pkgFile := filepath.Join(pkgDir, "python-3.11.0.tbz2")
	if err := os.WriteFile(pkgFile, []byte("test package"), 0644); err != nil {
		t.Fatalf("Failed to create package file: %v", err)
	}

	// Add package to store explicitly since loadPackages doesn't scan
	pkg := &Package{
		Name:    "python",
		Version: "3.11.0",
		Arch:    "amd64",
		Path:    pkgFile,
	}
	if err := store.Add(pkg); err != nil {
		t.Fatalf("Failed to add package: %v", err)
	}

	req := &QueryRequest{
		Name:     "python",
		Version:  "3.11.0",
		Arch:     "amd64",
		UseFlags: []string{},
	}

	foundPkg, found := store.Query(req)

	if !found {
		t.Error("Package should be found")
	}

	if foundPkg == nil {
		t.Fatal("Package is nil")
	}

	if foundPkg.Name != "python" {
		t.Errorf("Expected name=python, got %s", foundPkg.Name)
	}

	if foundPkg.Version != "3.11.0" {
		t.Errorf("Expected version=3.11.0, got %s", foundPkg.Version)
	}
}

// TestQueryNotFound tests querying for non-existent package.
func TestQueryNotFound(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "binpkg-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	store := NewStore(tmpDir)

	req := &QueryRequest{
		Name:     "dev-lang/nonexistent",
		Version:  "1.0.0",
		Arch:     "amd64",
		UseFlags: []string{},
	}

	_, found := store.Query(req)

	if found {
		t.Error("Non-existent package should not be found")
	}
}

// TestQueryRequest tests QueryRequest structure.
func TestQueryRequest(t *testing.T) {
	req := &QueryRequest{
		Name:     "dev-lang/python",
		Version:  "3.11",
		Arch:     "amd64",
		UseFlags: []string{"ssl", "threads"},
	}

	if req.Name != "dev-lang/python" {
		t.Errorf("Expected Name=dev-lang/python, got %s", req.Name)
	}

	if len(req.UseFlags) != 2 {
		t.Errorf("Expected 2 USE flags, got %d", len(req.UseFlags))
	}
}

// TestPackage tests Package structure.
func TestPackage(t *testing.T) {
	pkg := &Package{
		Name:     "python",
		Version:  "3.11.0",
		Arch:     "amd64",
		UseFlags: []string{"ssl", "threads"},
		Path:     "/path/to/package.tbz2",
	}

	if pkg.Name != "python" {
		t.Errorf("Expected Name=python, got %s", pkg.Name)
	}

	if pkg.Version != "3.11.0" {
		t.Errorf("Expected Version=3.11.0, got %s", pkg.Version)
	}
}

// TestQueryVersionless verifies that a query without a version returns the
// newest available version, and that arch-less queries match any arch.
func TestQueryVersionless(t *testing.T) {
	store := NewStore(t.TempDir())

	for _, v := range []string{"3.11.0", "3.9.2", "3.10.14-r1"} {
		if err := store.Add(&Package{Name: "dev-lang/python", Version: v, Arch: "amd64"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	pkg, found := store.Query(&QueryRequest{Name: "dev-lang/python", Arch: "amd64"})
	if !found {
		t.Fatal("version-less query should find the package")
	}
	if pkg.Version != "3.11.0" {
		t.Errorf("expected newest version 3.11.0, got %s", pkg.Version)
	}

	if _, found := store.Query(&QueryRequest{Name: "dev-lang/python"}); !found {
		t.Error("arch-less query should find the package")
	}

	if _, found := store.Query(&QueryRequest{Name: "dev-lang/perl", Arch: "amd64"}); found {
		t.Error("query for an unknown package should not match")
	}
}

// TestRegenerateIndexPopulatesStore verifies that RegenerateIndex feeds the
// in-memory query view from the on-disk scan (previously the JSON query API
// always answered found=false because nothing populated the store).
func TestRegenerateIndexPopulatesStore(t *testing.T) {
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "app-misc")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A minimal xpak-less file still gets indexed by path-derived CPV.
	if err := os.WriteFile(filepath.Join(pkgDir, "jq-1.7.tbz2"), []byte("pkg"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewStore(dir)
	n, err := store.RegenerateIndex("amd64")
	if err != nil {
		t.Fatalf("RegenerateIndex: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 indexed package, got %d", n)
	}

	pkg, found := store.Query(&QueryRequest{Name: "app-misc/jq", Arch: "amd64"})
	if !found {
		t.Fatal("query after RegenerateIndex should find the scanned package")
	}
	if pkg.Version != "1.7" {
		t.Errorf("expected version 1.7, got %s", pkg.Version)
	}
	if pkg.Path == "" || pkg.Checksum == "" {
		t.Errorf("expected path and checksum to be populated, got %+v", pkg)
	}
}

// TestSplitCPV covers the name/version boundary rules.
func TestSplitCPV(t *testing.T) {
	cases := []struct {
		cpv, name, version string
		ok                 bool
	}{
		{"dev-lang/python-3.11.0", "dev-lang/python", "3.11.0", true},
		{"app-misc/foo-1.0-r1", "app-misc/foo", "1.0-r1", true},
		{"app-misc/foo-2-1.0", "app-misc/foo-2", "1.0", true},
		{"noversion", "", "", false},
	}
	for _, c := range cases {
		name, version, ok := splitCPV(c.cpv)
		if ok != c.ok || name != c.name || version != c.version {
			t.Errorf("splitCPV(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.cpv, name, version, ok, c.name, c.version, c.ok)
		}
	}
}
