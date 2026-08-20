package builder

import (
	"strings"
	"testing"

	"github.com/slchris/portage-engine/pkg/config"
)

func cfgWith(format string) *config.BuilderConfig {
	return &config.BuilderConfig{
		BinpkgFormat: format,
	}
}

// envMap turns a KEY=VALUE slice into a map for easy assertions.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i >= 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return m
}

func TestBuildEnvironment_GpkgUnsigned(t *testing.T) {
	be := NewBuildExecutorWithOptions("/work", "/art", BuildOptions{Format: "gpkg"})
	bundle := &ConfigBundle{Config: &PortageConfig{}}
	env := envMap(be.buildEnvironment(PackageSpec{Atom: "dev-lang/python"}, bundle, "/work/packages", ""))

	if env["BINPKG_FORMAT"] != "gpkg" {
		t.Errorf("BINPKG_FORMAT = %q, want gpkg", env["BINPKG_FORMAT"])
	}
	if !strings.Contains(env["FEATURES"], "buildpkg") {
		t.Errorf("FEATURES missing buildpkg: %q", env["FEATURES"])
	}
	if strings.Contains(env["FEATURES"], "binpkg-signing") {
		t.Errorf("FEATURES should not enable signing when no key: %q", env["FEATURES"])
	}
	if _, ok := env["BINPKG_GPG_SIGNING_KEY"]; ok {
		t.Error("BINPKG_GPG_SIGNING_KEY set without a signing key")
	}
	if env["PKGDIR"] != "/work/packages" {
		t.Errorf("PKGDIR = %q, want /work/packages", env["PKGDIR"])
	}
}

func TestBuildEnvironment_UserCannotEnableSigning(t *testing.T) {
	be := NewBuildExecutorWithOptions("/work", "/art", BuildOptions{Format: "gpkg"})
	bundle := &ConfigBundle{Config: &PortageConfig{Environment: map[string]string{"FEATURES": "userfeature"}}}
	env := envMap(be.buildEnvironment(PackageSpec{Atom: "dev-lang/python"}, bundle, "/work/packages", ""))

	if strings.Contains(env["FEATURES"], "binpkg-signing") {
		t.Errorf("builder enabled forbidden binpkg-signing: %q", env["FEATURES"])
	}
	// User-supplied FEATURES must be preserved, not clobbered.
	if !strings.Contains(env["FEATURES"], "userfeature") {
		t.Errorf("FEATURES dropped user feature: %q", env["FEATURES"])
	}
	if !strings.Contains(env["FEATURES"], "buildpkg") {
		t.Errorf("FEATURES missing buildpkg: %q", env["FEATURES"])
	}
	if _, ok := env["BINPKG_GPG_SIGNING_KEY"]; ok {
		t.Error("builder exported a signing key")
	}
}

func TestBuildEnvironmentIncludesCatalogRequiredFeatures(t *testing.T) {
	be := NewBuildExecutorWithOptions("/work", "/art", BuildOptions{Format: "gpkg"})
	bundle := &ConfigBundle{
		Config: &PortageConfig{},
		Metadata: BundleMetadata{RequiredFeatures: []string{
			"binpkg-multi-instance", "sandbox", "userpriv",
		}},
	}
	env := envMap(be.buildEnvironment(PackageSpec{Atom: "app-misc/jq"}, bundle, "/work/packages", ""))
	if env["FEATURES"] != "binpkg-multi-instance buildpkg sandbox userpriv" {
		t.Fatalf("FEATURES = %q, want catalog policy plus buildpkg", env["FEATURES"])
	}
}

func TestBuildEnvironment_XpakIsUnsigned(t *testing.T) {
	be := NewBuildExecutorWithOptions("/work", "/art", BuildOptions{Format: "xpak"})
	bundle := &ConfigBundle{Config: &PortageConfig{}}
	env := envMap(be.buildEnvironment(PackageSpec{Atom: "dev-lang/python"}, bundle, "/work/packages", ""))

	if env["BINPKG_FORMAT"] != "xpak" {
		t.Errorf("BINPKG_FORMAT = %q, want xpak", env["BINPKG_FORMAT"])
	}
	if strings.Contains(env["FEATURES"], "binpkg-signing") {
		t.Errorf("xpak must not enable binpkg-signing: %q", env["FEATURES"])
	}
}

func TestBuildEnvironmentBindsNativePortageConfigRoot(t *testing.T) {
	be := NewBuildExecutor("/work", "/artifacts")
	bundle := &ConfigBundle{Config: &PortageConfig{}}
	env := envMap(be.buildEnvironment(PackageSpec{Atom: "app-misc/hello"}, bundle, "/work/packages", "/work/job-1"))
	if env["PORTAGE_CONFIGROOT"] != "/work/job-1" {
		t.Fatalf("native build did not bind isolated Portage config root: %+v", env)
	}
}

func TestBuildOptionsFromConfig(t *testing.T) {
	native := buildOptionsFromConfig(&config.BuilderConfig{
		BinpkgFormat: "gpkg",
	})
	if native.Format != "gpkg" {
		t.Errorf("format = %q, want gpkg", native.Format)
	}
	if got := buildOptionsFromConfig(cfgWith("")).Format; got != "gpkg" {
		t.Errorf("default format = %q, want gpkg", got)
	}
}
