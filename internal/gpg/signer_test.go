package gpg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestNewSigner tests creating a new GPG signer.
func TestNewSigner(t *testing.T) {
	t.Parallel()

	signer := NewSigner("test-key-id", "/path/to/key", true)

	if signer == nil {
		t.Fatal("NewSigner returned nil")
	}

	if signer.keyID != "test-key-id" {
		t.Errorf("Expected keyID=test-key-id, got %s", signer.keyID)
	}

	if signer.keyPath != "/path/to/key" {
		t.Errorf("Expected keyPath=/path/to/key, got %s", signer.keyPath)
	}

	if !signer.enabled {
		t.Error("Expected enabled=true, got false")
	}
}

// TestNewSignerWithOptions tests creating a signer with options.
func TestNewSignerWithOptions(t *testing.T) {
	t.Parallel()

	signer := NewSigner("test-key", "", true,
		WithGnupgHome("/custom/gpg"),
		WithAutoCreate("Test Name", "test@example.com"),
	)

	if signer == nil {
		t.Fatal("NewSigner returned nil")
	}

	if signer.gnupgHome != "/custom/gpg" {
		t.Errorf("Expected gnupgHome=/custom/gpg, got %s", signer.gnupgHome)
	}

	if !signer.autoCreate {
		t.Error("Expected autoCreate=true")
	}

	if signer.keyName != "Test Name" {
		t.Errorf("Expected keyName=Test Name, got %s", signer.keyName)
	}

	if signer.keyEmail != "test@example.com" {
		t.Errorf("Expected keyEmail=test@example.com, got %s", signer.keyEmail)
	}
}

// TestKeyID tests getting and setting key ID.
func TestKeyID(t *testing.T) {
	t.Parallel()

	signer := NewSigner("initial-key", "", true)

	if signer.KeyID() != "initial-key" {
		t.Errorf("Expected KeyID=initial-key, got %s", signer.KeyID())
	}

	signer.SetKeyID("new-key")

	if signer.KeyID() != "new-key" {
		t.Errorf("Expected KeyID=new-key, got %s", signer.KeyID())
	}
}

// TestIsEnabled tests checking if signing is enabled.
func TestIsEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		keyID    string
		keyPath  string
		enabled  bool
		expected bool
	}{
		{
			name:     "Enabled",
			keyID:    "test-key",
			keyPath:  "/path/to/key",
			enabled:  true,
			expected: true,
		},
		{
			name:     "Disabled",
			keyID:    "test-key",
			keyPath:  "/path/to/key",
			enabled:  false,
			expected: false,
		},
		{
			name:     "Not enabled",
			keyID:    "",
			keyPath:  "",
			enabled:  false,
			expected: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			signer := NewSigner(tt.keyID, tt.keyPath, tt.enabled)
			if signer.IsEnabled() != tt.expected {
				t.Errorf("Expected IsEnabled=%v, got %v", tt.expected, signer.IsEnabled())
			}
		})
	}
}

// TestInitializeDisabled tests that Initialize does nothing when disabled.
func TestInitializeDisabled(t *testing.T) {
	t.Parallel()

	signer := NewSigner("", "", false)
	if err := signer.Initialize(); err != nil {
		t.Errorf("Initialize should succeed when disabled: %v", err)
	}
}

// TestInitializeNoKey tests Initialize fails without key when not auto-create.
func TestInitializeNoKey(t *testing.T) {
	t.Parallel()

	signer := NewSigner("", "", true)
	err := signer.Initialize()
	if err == nil {
		t.Error("Initialize should fail without key when auto-create disabled")
	}
	if !strings.Contains(err.Error(), "no key ID configured") {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestInitializeAutoCreateReusesPersistentKey(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg is not installed")
	}

	home, err := os.MkdirTemp("/tmp", "pe-gpg-restart-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	const email = "restart-test@portage-engine.invalid"
	first := NewSigner("", "", true,
		WithGnupgHome(home),
		WithAutoCreate("Portage Engine Restart Test", email),
	)
	if err := first.Initialize(); err != nil {
		t.Fatalf("first Initialize: %v", err)
	}
	firstKeyID := first.KeyID()
	if firstKeyID == "" {
		t.Fatal("first Initialize did not select a key")
	}

	second := NewSigner("", "", true,
		WithGnupgHome(home),
		WithAutoCreate("Portage Engine Restart Test", email),
	)
	if err := second.Initialize(); err != nil {
		t.Fatalf("second Initialize: %v", err)
	}
	if second.KeyID() != firstKeyID {
		t.Fatalf("restart selected key %q, want %q", second.KeyID(), firstKeyID)
	}

	keyIDs, err := second.findSecretKeyIDs(email)
	if err != nil {
		t.Fatalf("list restart-test keys: %v", err)
	}
	if len(keyIDs) != 1 {
		t.Fatalf("restart created %d matching secret keys, want 1: %v", len(keyIDs), keyIDs)
	}
	marker, err := os.ReadFile(filepath.Join(home, activeKeyFile))
	if err != nil {
		t.Fatalf("read active-key marker: %v", err)
	}
	if strings.TrimSpace(string(marker)) != firstKeyID {
		t.Fatalf("active-key marker = %q, want %q", strings.TrimSpace(string(marker)), firstKeyID)
	}
}

func TestInitializeAutoCreateFailsClosedOnMissingMarkedKey(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg is not installed")
	}

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, activeKeyFile), []byte("DEADBEEFDEADBEEF\n"), 0600); err != nil {
		t.Fatal(err)
	}
	signer := NewSigner("", "", true,
		WithGnupgHome(home),
		WithAutoCreate("Portage Engine Missing Key Test", "missing-key@portage-engine.invalid"),
	)
	err := signer.Initialize()
	if err == nil || !strings.Contains(err.Error(), "references a missing key") {
		t.Fatalf("Initialize error = %v, want missing-key failure", err)
	}
}

// TestBuildBaseArgs tests building base GPG arguments.
func TestBuildBaseArgs(t *testing.T) {
	t.Parallel()

	// Without keyPath
	signer := NewSigner("test-key", "", true)
	args := signer.buildBaseArgs()
	if len(args) != 0 {
		t.Errorf("Expected empty args without keyPath, got %v", args)
	}

	// With non-existent keyPath (should not add args)
	signer2 := NewSigner("test-key", "/nonexistent/path", true)
	args2 := signer2.buildBaseArgs()
	if len(args2) != 0 {
		t.Errorf("Expected empty args with non-existent keyPath, got %v", args2)
	}
}

// TestSignPackage tests package signing functionality.
func TestSignPackage(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a test package file
	pkgPath := filepath.Join(tmpDir, "test-package.tbz2")
	if err := os.WriteFile(pkgPath, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create test package: %v", err)
	}

	signer := NewSigner("", "", false)

	// Should not create signature when disabled
	err := signer.SignPackage(pkgPath)
	if err != nil {
		t.Errorf("SignPackage should succeed when disabled: %v", err)
	}

	// Verify no signature file was created
	sigFile := pkgPath + ".sig"
	if _, err := os.Stat(sigFile); err == nil {
		t.Error("Signature file should not be created when signing is disabled")
	}
}

// TestSignPackageDisabled tests that signing is skipped when disabled.
func TestSignPackageDisabled(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	pkgPath := filepath.Join(tmpDir, "test-package.tbz2")
	if err := os.WriteFile(pkgPath, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create test package: %v", err)
	}

	signer := NewSigner("test-key", "/path/to/key", false)

	// Should not error when disabled
	err := signer.SignPackage(pkgPath)
	if err != nil {
		t.Errorf("SignPackage should not error when disabled: %v", err)
	}
}

// TestSignPackageNoKeyID tests signing fails without key ID.
func TestSignPackageNoKeyID(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	pkgPath := filepath.Join(tmpDir, "test-package.tbz2")
	if err := os.WriteFile(pkgPath, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create test package: %v", err)
	}

	signer := NewSigner("", "", true) // enabled but no key ID

	err := signer.SignPackage(pkgPath)
	if err == nil {
		t.Error("SignPackage should fail without key ID")
	}
	if !strings.Contains(err.Error(), "key ID not configured") {
		t.Errorf("Unexpected error: %v", err)
	}
}

// TestGetPublicKeyNoKeyID tests GetPublicKey fails without key ID.
func TestGetPublicKeyNoKeyID(t *testing.T) {
	t.Parallel()

	signer := NewSigner("", "", true)

	_, err := signer.GetPublicKey()
	if err == nil {
		t.Error("GetPublicKey should fail without key ID")
	}
	if !strings.Contains(err.Error(), "no key ID configured") {
		t.Errorf("Unexpected error: %v", err)
	}
}

// TestExportPublicKeyNoKeyID tests ExportPublicKey fails without key ID.
func TestExportPublicKeyNoKeyID(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	signer := NewSigner("", "", true)

	err := signer.ExportPublicKey(filepath.Join(tmpDir, "key.asc"))
	if err == nil {
		t.Error("ExportPublicKey should fail without key ID")
	}
}

// TestSignDirectoryDisabled tests SignDirectory when disabled.
func TestSignDirectoryDisabled(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	signer := NewSigner("test-key", "", false)

	err := signer.SignDirectory(tmpDir)
	if err != nil {
		t.Errorf("SignDirectory should succeed when disabled: %v", err)
	}
}

// TestGetSecretKeyNoKeyID tests GetSecretKey fails without key ID.
func TestGetSecretKeyNoKeyID(t *testing.T) {
	t.Parallel()

	signer := NewSigner("", "", true)

	_, err := signer.GetSecretKey()
	if err == nil {
		t.Error("GetSecretKey should fail without key ID")
	}
	if !strings.Contains(err.Error(), "no key ID configured") {
		t.Errorf("Unexpected error: %v", err)
	}
}

// TestExportSecretKeyNoKeyID tests ExportSecretKey fails without key ID.
func TestExportSecretKeyNoKeyID(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	signer := NewSigner("", "", true)

	err := signer.ExportSecretKey(filepath.Join(tmpDir, "secret.asc"))
	if err == nil {
		t.Error("ExportSecretKey should fail without key ID")
	}
}

// TestExportKeyPairNoKeyID tests ExportKeyPair fails without key ID.
func TestExportKeyPairNoKeyID(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	signer := NewSigner("", "", true)

	_, _, err := signer.ExportKeyPair(tmpDir)
	if err == nil {
		t.Error("ExportKeyPair should fail without key ID")
	}
}

// TestExportKeyPairCreateDirectory tests ExportKeyPair creates directory.
func TestExportKeyPairCreateDirectory(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	newDir := filepath.Join(tmpDir, "nested", "gpg-keys")
	signer := NewSigner("", "", true) // No key ID

	// Will fail on export because no key ID
	_, _, err := signer.ExportKeyPair(newDir)
	if err == nil {
		t.Error("ExportKeyPair should fail without key ID")
	}

	// Directory should still be created before the export attempt
	if _, err := os.Stat(newDir); os.IsNotExist(err) {
		t.Error("ExportKeyPair should create the directory before failing")
	}
}

// TestGetPublicKeyReturnsCache verifies that a pre-populated cache is returned
// without shelling out to gpg.
func TestGetPublicKeyReturnsCache(t *testing.T) {
	t.Parallel()

	signer := NewSigner("test-key", "", true)
	signer.pubKeyMu.Lock()
	signer.publicKey = "CACHED-KEY"
	signer.pubKeyMu.Unlock()

	key, err := signer.GetPublicKey()
	if err != nil {
		t.Fatalf("GetPublicKey returned error: %v", err)
	}
	if key != "CACHED-KEY" {
		t.Errorf("Expected cached key, got %q", key)
	}
}

// TestGetPublicKeyConcurrent exercises GetPublicKey concurrently to catch data
// races on the cached public key (run with -race). The cache is pre-populated
// so the test does not depend on gpg being installed.
func TestGetPublicKeyConcurrent(t *testing.T) {
	t.Parallel()

	signer := NewSigner("test-key", "", true)
	signer.pubKeyMu.Lock()
	signer.publicKey = "CACHED-KEY"
	signer.pubKeyMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := signer.GetPublicKey()
			if err != nil {
				t.Errorf("GetPublicKey returned error: %v", err)
				return
			}
			if key != "CACHED-KEY" {
				t.Errorf("Expected cached key, got %q", key)
			}
		}()
	}
	wg.Wait()
}
