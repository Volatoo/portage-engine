package builder

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCreateConfigBundle tests the creation of configuration bundles.
func TestCreateConfigBundle(t *testing.T) {
	config := &PortageConfig{
		PackageUse: map[string][]string{
			"dev-lang/python:3.11": {"ssl", "threads"},
		},
		MakeConf: map[string]string{
			"MAKEOPTS": "-j8",
		},
	}

	packages := &BuildPackageSpec{
		Packages: []PackageSpec{
			{
				Atom:     "dev-lang/python:3.11",
				Version:  "3.11.8",
				UseFlags: []string{"ssl", "threads"},
			},
		},
	}

	metadata := BundleMetadata{
		UserID:     "test-user",
		TargetArch: "amd64",
		Profile:    "default/linux/amd64/23.0",
		CreatedAt:  time.Now().Format(time.RFC3339),
	}

	transfer := NewConfigTransfer("")
	bundle, err := transfer.CreateConfigBundle(config, packages, metadata)
	if err != nil {
		t.Fatalf("Failed to create config bundle: %v", err)
	}

	if bundle == nil {
		t.Fatal("Bundle is nil")
	}

	if len(bundle.Packages.Packages) != 1 {
		t.Errorf("Expected 1 package, got %d", len(bundle.Packages.Packages))
	}

	if bundle.Config.MakeConf["MAKEOPTS"] != "-j8" {
		t.Errorf("Expected MAKEOPTS=-j8, got %s", bundle.Config.MakeConf["MAKEOPTS"])
	}
}

// TestExportImportBundle tests exporting and importing configuration bundles.
func TestExportImportBundle(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "portage-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	config := &PortageConfig{
		PackageUse: map[string][]string{
			"dev-lang/python:3.11": {"ssl", "threads"},
		},
		PackageKeywords: map[string][]string{
			"dev-lang/python:3.11": {"~amd64"},
		},
		MakeConf: map[string]string{
			"MAKEOPTS": "-j8",
		},
		GlobalUse: []string{"systemd", "-consolekit"},
	}

	packages := &BuildPackageSpec{
		Packages: []PackageSpec{
			{
				Atom:     "dev-lang/python:3.11",
				Version:  "3.11.8",
				UseFlags: []string{"ssl", "threads"},
			},
		},
	}

	metadata := BundleMetadata{
		UserID:     "test-user",
		TargetArch: "amd64",
		Profile:    "default/linux/amd64/23.0",
		CreatedAt:  time.Now().Format(time.RFC3339),
	}

	transfer := NewConfigTransfer(tmpDir)

	// Create bundle
	bundle, err := transfer.CreateConfigBundle(config, packages, metadata)
	if err != nil {
		t.Fatalf("Failed to create config bundle: %v", err)
	}

	// Export bundle
	bundlePath := filepath.Join(tmpDir, "test-bundle.tar.gz")
	if err := transfer.ExportBundle(bundle, bundlePath); err != nil {
		t.Fatalf("Failed to export bundle: %v", err)
	}

	// Verify bundle file exists
	if _, err := os.Stat(bundlePath); os.IsNotExist(err) {
		t.Fatalf("Bundle file does not exist: %s", bundlePath)
	}

	// Import bundle
	importedBundle, err := transfer.ImportBundle(bundlePath)
	if err != nil {
		t.Fatalf("Failed to import bundle: %v", err)
	}

	// Verify imported data
	if importedBundle.Metadata.UserID != metadata.UserID {
		t.Errorf("Expected UserID=%s, got %s", metadata.UserID, importedBundle.Metadata.UserID)
	}

	if len(importedBundle.Packages.Packages) != 1 {
		t.Errorf("Expected 1 package, got %d", len(importedBundle.Packages.Packages))
	}

	if importedBundle.Config.MakeConf["MAKEOPTS"] != "-j8" {
		t.Errorf("Expected MAKEOPTS=-j8, got %s", importedBundle.Config.MakeConf["MAKEOPTS"])
	}
}

// TestApplyConfigToSystem tests applying configuration to a system.
func TestApplyConfigToSystem(t *testing.T) {
	// Create a temporary directory to simulate a target system
	tmpDir, err := os.MkdirTemp("", "portage-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	config := &PortageConfig{
		PackageUse: map[string][]string{
			"dev-lang/python:3.11": {"ssl", "threads"},
		},
		PackageKeywords: map[string][]string{
			"dev-lang/python:3.11": {"~amd64"},
		},
		PackageMask: []string{
			">=dev-lang/python-3.12",
		},
		MakeConf: map[string]string{
			"MAKEOPTS": "-j8",
		},
	}

	packages := &BuildPackageSpec{
		Packages: []PackageSpec{
			{
				Atom:    "dev-lang/python:3.11",
				Version: "3.11.8",
			},
		},
	}

	metadata := BundleMetadata{
		UserID:     "test-user",
		TargetArch: "amd64",
	}

	transfer := NewConfigTransfer("")
	bundle, err := transfer.CreateConfigBundle(config, packages, metadata)
	if err != nil {
		t.Fatalf("Failed to create config bundle: %v", err)
	}

	// Apply config to temp directory
	if err := transfer.ApplyConfigToSystem(bundle, tmpDir); err != nil {
		t.Fatalf("Failed to apply config to system: %v", err)
	}

	// Verify files were created
	portageDir := filepath.Join(tmpDir, "etc", "portage")

	// Check package.use
	packageUsePath := filepath.Join(portageDir, "package.use", "00-user")
	if _, err := os.Stat(packageUsePath); os.IsNotExist(err) {
		t.Errorf("package.use file not created: %s", packageUsePath)
	} else {
		content, err := os.ReadFile(packageUsePath)
		if err != nil {
			t.Errorf("Failed to read package.use: %v", err)
		} else {
			contentStr := string(content)
			if !contains(contentStr, "dev-lang/python:3.11") {
				t.Errorf("package.use does not contain expected content")
			}
		}
	}

	// Check package.accept_keywords
	keywordsPath := filepath.Join(portageDir, "package.accept_keywords", "00-user")
	if _, err := os.Stat(keywordsPath); os.IsNotExist(err) {
		t.Errorf("package.accept_keywords file not created: %s", keywordsPath)
	}

	// Check package.mask
	maskPath := filepath.Join(portageDir, "package.mask", "00-user")
	if _, err := os.Stat(maskPath); os.IsNotExist(err) {
		t.Errorf("package.mask file not created: %s", maskPath)
	} else {
		content, err := os.ReadFile(maskPath)
		if err != nil {
			t.Errorf("Failed to read package.mask: %v", err)
		} else {
			contentStr := string(content)
			if !contains(contentStr, ">=dev-lang/python-3.12") {
				t.Errorf("package.mask does not contain expected content")
			}
		}
	}

	// Check make.conf — overrides are appended to the real make.conf file
	// (Portage does not source make.conf.d/).
	makeConfPath := filepath.Join(portageDir, "make.conf")
	if _, err := os.Stat(makeConfPath); os.IsNotExist(err) {
		t.Errorf("make.conf file not created: %s", makeConfPath)
	} else {
		content, err := os.ReadFile(makeConfPath)
		if err != nil {
			t.Errorf("Failed to read make.conf: %v", err)
		} else {
			contentStr := string(content)
			if !contains(contentStr, "MAKEOPTS") {
				t.Errorf("make.conf does not contain expected content")
			}
		}
	}
	for _, target := range []string{
		portageDir,
		filepath.Join(portageDir, "package.use"),
		packageUsePath,
		keywordsPath,
		maskPath,
		makeConfPath,
	} {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat %s: %v", target, err)
		}
		if info.IsDir() && info.Mode().Perm()&0o005 != 0o005 {
			t.Errorf("Portage helper cannot traverse/read directory %s: mode %o", target, info.Mode().Perm())
		}
		if !info.IsDir() && info.Mode().Perm()&0o004 != 0o004 {
			t.Errorf("Portage helper cannot read config %s: mode %o", target, info.Mode().Perm())
		}
	}
}

func TestApplyConfigToSystemRejectsRepositoryPathTraversalBeforeWriting(t *testing.T) {
	targetRoot := t.TempDir()
	bundle := &ConfigBundle{
		Config: &PortageConfig{
			Repos: []RepoConfig{{
				Name:     "../../escape",
				Location: "/var/db/repos/escape",
				SyncType: "git",
				SyncURI:  "https://example.org/repo.git",
			}},
		},
		Packages: &BuildPackageSpec{Packages: []PackageSpec{{Atom: "app-misc/hello"}}},
	}

	transfer := NewConfigTransfer("")
	if err := transfer.ApplyConfigToSystem(bundle, targetRoot); err == nil {
		t.Fatal("expected repository path traversal to be rejected")
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "etc")); !os.IsNotExist(err) {
		t.Fatalf("validation must fail before creating files, stat error: %v", err)
	}
}

// TestPackageSpec tests package specification.
func TestPackageSpec(t *testing.T) {
	spec := PackageSpec{
		Atom:     "dev-lang/python:3.11",
		Version:  "3.11.8",
		UseFlags: []string{"ssl", "threads", "-test"},
		Keywords: []string{"~amd64"},
		Environment: map[string]string{
			"PYTHON_TARGETS": "python3_11",
		},
	}

	if spec.Atom != "dev-lang/python:3.11" {
		t.Errorf("Expected Atom=dev-lang/python:3.11, got %s", spec.Atom)
	}

	if len(spec.UseFlags) != 3 {
		t.Errorf("Expected 3 USE flags, got %d", len(spec.UseFlags))
	}

	if spec.Environment["PYTHON_TARGETS"] != "python3_11" {
		t.Errorf("Expected PYTHON_TARGETS=python3_11, got %s", spec.Environment["PYTHON_TARGETS"])
	}
}

// TestReadSystemPortageConfig tests reading from /etc/portage directory.
func TestReadSystemPortageConfig(t *testing.T) {
	tmpDir := setupTestPortageDir(t)
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			t.Logf("Failed to clean up temp dir: %v", err)
		}
	}()

	transfer := NewConfigTransfer("")
	config, err := transfer.ReadSystemPortageConfig(tmpDir)
	if err != nil {
		t.Fatalf("Failed to read system portage config: %v", err)
	}

	verifyPortageConfig(t, config)
}

func setupTestPortageDir(t *testing.T) string {
	tmpDir, err := os.MkdirTemp("", "portage-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	createMakeConf(t, tmpDir)
	createPackageUse(t, tmpDir)
	createPackageKeywords(t, tmpDir)
	createPackageMask(t, tmpDir)
	createPackageUnmask(t, tmpDir)
	createReposConf(t, tmpDir)

	return tmpDir
}

func createMakeConf(t *testing.T, tmpDir string) {
	content := `# Test make.conf
CFLAGS="-O2 -pipe"
MAKEOPTS="-j8"
USE="ssl threads -doc"
PORTAGE_TMPDIR="/var/tmp"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "make.conf"), []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create make.conf: %v", err)
	}
}

func createPackageUse(t *testing.T, tmpDir string) {
	dir := filepath.Join(tmpDir, "package.use")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("Failed to create package.use dir: %v", err)
	}
	content := `# Python settings
dev-lang/python ssl threads sqlite
dev-lang/python:3.11 -test -doc

# Vim settings
app-editors/vim python vim-pager
`
	if err := os.WriteFile(filepath.Join(dir, "custom"), []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create package.use file: %v", err)
	}
}

func createPackageKeywords(t *testing.T, tmpDir string) {
	dir := filepath.Join(tmpDir, "package.accept_keywords")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("Failed to create package.accept_keywords dir: %v", err)
	}
	content := `# Testing packages
dev-lang/rust ~amd64
sys-devel/gcc ~amd64
`
	if err := os.WriteFile(filepath.Join(dir, "testing"), []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create keywords file: %v", err)
	}
}

func createPackageMask(t *testing.T, tmpDir string) {
	content := `# Masked packages
>=dev-lang/python-3.12
`
	if err := os.WriteFile(filepath.Join(tmpDir, "package.mask"), []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create package.mask: %v", err)
	}
}

func createPackageUnmask(t *testing.T, tmpDir string) {
	content := `# Unmasked packages
=sys-kernel/gentoo-sources-6.6.0
`
	if err := os.WriteFile(filepath.Join(tmpDir, "package.unmask"), []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create package.unmask: %v", err)
	}
}

func createReposConf(t *testing.T, tmpDir string) {
	dir := filepath.Join(tmpDir, "repos.conf")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("Failed to create repos.conf dir: %v", err)
	}
	content := `[DEFAULT]
main-repo = gentoo

[gentoo]
location = /var/db/repos/gentoo
sync-type = git
sync-uri = https://github.com/gentoo-mirror/gentoo.git
priority = 100

[guru]
location = /var/db/repos/guru
sync-type = git
sync-uri = https://github.com/gentoo-mirror/guru.git
priority = 50
`
	if err := os.WriteFile(filepath.Join(dir, "gentoo.conf"), []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create repos conf: %v", err)
	}
}

func verifyPortageConfig(t *testing.T, config *PortageConfig) {
	verifyMakeConf(t, config)
	verifyPackageUse(t, config)
	verifyPackageKeywords(t, config)
	verifyPackageMask(t, config)

	verifyPackageUnmask(t, config)
	verifyRepos(t, config)
}

func verifyMakeConf(t *testing.T, config *PortageConfig) {
	if config.MakeConf["MAKEOPTS"] != "-j8" {
		t.Errorf("Expected MAKEOPTS=-j8, got %s", config.MakeConf["MAKEOPTS"])
	}
	if len(config.GlobalUse) != 3 {
		t.Errorf("Expected 3 global USE flags, got %d", len(config.GlobalUse))
	}
}

func verifyPackageUse(t *testing.T, config *PortageConfig) {
	pythonUse, ok := config.PackageUse["dev-lang/python"]
	if !ok {
		t.Error("dev-lang/python not found in package.use")
	} else if len(pythonUse) < 3 {
		t.Errorf("Expected at least 3 USE flags for python, got %d", len(pythonUse))
	}
}

func verifyPackageKeywords(t *testing.T, config *PortageConfig) {
	rustKeywords, ok := config.PackageKeywords["dev-lang/rust"]
	if !ok {
		t.Error("dev-lang/rust not found in package.accept_keywords")
	} else if len(rustKeywords) == 0 || rustKeywords[0] != "~amd64" {
		t.Errorf("Expected ~amd64 keyword for rust, got %v", rustKeywords)
	}
}

func verifyPackageMask(t *testing.T, config *PortageConfig) {
	if len(config.PackageMask) == 0 {
		t.Error("Expected package.mask entries")
	}
}

func verifyPackageUnmask(t *testing.T, config *PortageConfig) {
	if len(config.PackageUnmask) == 0 {
		t.Error("Expected package.unmask entries")
	}
}

func verifyRepos(t *testing.T, config *PortageConfig) {
	if len(config.Repos) < 2 {
		t.Errorf("Expected at least 2 repos, got %d", len(config.Repos))
	}
	for _, repo := range config.Repos {
		if repo.Name == "gentoo" {
			if repo.SyncType != "git" || repo.Priority != 100 {
				t.Error("gentoo repo configuration mismatch")
			}
			return
		}
	}
	t.Error("gentoo repo not found")
}

// TestReadSystemPortageConfigNonExistent tests reading from non-existent directory.
func TestReadSystemPortageConfigNonExistent(t *testing.T) {
	transfer := NewConfigTransfer("")
	_, err := transfer.ReadSystemPortageConfig("/nonexistent/portage")
	if err == nil {
		t.Error("Expected error for non-existent directory")
	}
}

// contains checks if a string contains a substring.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && findSubstring(s, substr))
}

// findSubstring finds a substring in a string.
func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
