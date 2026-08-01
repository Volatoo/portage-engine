package imagefactory

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestPackageSetCatalogResolveIncludesAndDeduplicates(t *testing.T) {
	catalog := &PackageSetCatalog{SchemaVersion: 1, CatalogID: "pe/test-v1", Sets: []PackageSetDefinition{
		{ID: "pe/runtime-v1", Packages: []string{"app-emulation/cloud-init", "dev-vcs/git"}},
		{ID: "pe/build-test-v1", Includes: []string{"pe/runtime-v1"}, Packages: []string{"dev-build/cmake", "dev-vcs/git"}},
	}}
	resolved, err := catalog.Resolve([]string{"pe/build-test-v1"}, []string{"app-misc/hello"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app-emulation/cloud-init", "dev-vcs/git", "dev-build/cmake", "app-misc/hello"}
	if !slices.Equal(resolved, want) {
		t.Fatalf("Resolve() = %v, want %v", resolved, want)
	}
}

func TestExamplePackageSetCatalog(t *testing.T) {
	catalog, err := LoadPackageSetCatalog(filepath.Join("..", "..", "image-factory", "package-sets", "catalog.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.Resolve([]string{"pe/desktop-verifier-v1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the image gate's desktop command/import contract backed by explicit
	// reviewed atoms. Relying on incidental transitive dependencies caused the
	// first live desktop candidate to compile successfully and then fail closed.
	for _, atom := range []string{
		"app-emulation/cloud-init",
		"app-crypt/gnupg",
		"dev-build/cmake",
		"app-accessibility/at-spi2-core",
		"dev-python/pygobject",
		"media-fonts/dejavu",
		"media-gfx/scrot",
		"x11-apps/xrandr",
		"x11-apps/xset",
		"x11-base/xorg-server",
		"x11-misc/lightdm",
		"x11-misc/xdotool",
		"xfce-base/xfce4-meta",
	} {
		if !slices.Contains(resolved, atom) {
			t.Fatalf("desktop verifier set does not include %q: %v", atom, resolved)
		}
	}
}

func TestPackageSetCatalogRejectsCycle(t *testing.T) {
	catalog := &PackageSetCatalog{SchemaVersion: 1, CatalogID: "pe/test-v1", Sets: []PackageSetDefinition{
		{ID: "pe/a-v1", Includes: []string{"pe/b-v1"}},
		{ID: "pe/b-v1", Includes: []string{"pe/a-v1"}},
	}}
	if err := catalog.Validate(); err == nil {
		t.Fatal("accepted a package-set include cycle")
	}
}
