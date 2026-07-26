package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slchris/portage-engine/pkg/config"
)

// TestVerifyInstallRejectsInjection ensures the verify path validates the
// package atom before it can reach an emerge argv, closing command/option
// injection via /api/v1/verify.
func TestVerifyInstallRejectsInjection(t *testing.T) {
	lb := &LocalBuilder{cfg: &config.BuilderConfig{}}
	bad := []string{
		"app-misc/jq; rm -rf /", // shell metacharacters
		"$(reboot)",
		"`id`",
		"--root=/etc",       // emerge option smuggling
		"-K app-misc/jq",    // leading dash
		"app-misc/jq && ls", // command chaining
		"",
	}
	for _, atom := range bad {
		if _, err := lb.VerifyInstall(VerifyInstallRequest{
			PackageName: atom,
			BinhostURL:  "http://binhost",
			Generation:  "unsigned",
			Artifacts: []VerificationArtifact{{
				RelativePath: "app-misc/jq/jq-1.gpkg.tar",
				SHA256:       strings.Repeat("a", 64),
				Size:         1,
			}},
		}); err == nil {
			t.Errorf("VerifyInstall(%q) accepted a malicious atom", atom)
		} else if !strings.Contains(err.Error(), "invalid package atom") {
			t.Errorf("VerifyInstall(%q) failed with %v, want atom-validation error", atom, err)
		}
	}
}

func TestNativeVerifySeedRequiresTrustStoreOnlyForSignedBuilds(t *testing.T) {
	unsigned := nativeVerifySeedCommand("/tmp/verify-root", false)
	if strings.Contains(unsigned, "/etc/portage/gnupg") {
		t.Fatalf("unsigned verification unexpectedly requires a signing trust store: %s", unsigned)
	}
	if !strings.Contains(unsigned, "cp -a /var/db/pkg") {
		t.Fatalf("native verification does not seed the image baseline VDB: %s", unsigned)
	}
	if strings.Contains(unsigned, "openpgp-keys") {
		t.Fatalf("native verification copied the host release-key bundle: %s", unsigned)
	}
	if !strings.Contains(unsigned, "chmod 0755 /tmp/verify-root") {
		t.Fatalf("native verification root is not traversable by the GPG verify user: %s", unsigned)
	}

	signed := nativeVerifySeedCommand("/tmp/verify-root", true)
	for _, want := range []string{"install -d -m 0700", "/tmp/verify-root/etc/portage/gnupg"} {
		if !strings.Contains(signed, want) {
			t.Errorf("signed verification seed missing %q: %s", want, signed)
		}
	}
	if strings.Contains(signed, "cp -a /etc/portage/gnupg") {
		t.Fatalf("signed verification copied mutable host trust state: %s", signed)
	}
}

func TestInstallVerifierPublicKeyCreatesIsolatedTrustedKeyring(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg is not installed")
	}
	source, err := os.MkdirTemp("/tmp", "pe-verify-source-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(source) })
	if err := os.Chmod(source, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := "Portage Engine Verify Test <verify-test@portage-engine.invalid>"
	command := exec.Command("gpg", "--homedir", source, "--batch", "--passphrase", "",
		"--quick-generate-key", identity, "ed25519", "sign", "0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate key: %v: %s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", source, "--kill", "gpg-agent").Run()
	})
	publicKey, err := exec.Command("gpg", "--homedir", source, "--batch", "--armor", "--export", identity).Output()
	if err != nil {
		t.Fatal(err)
	}

	destinationRoot, err := os.MkdirTemp("/tmp", "pe-verify-destination-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(destinationRoot) })
	destination := filepath.Join(destinationRoot, "gnupg")
	fingerprint, err := installVerifierPublicKey(destination, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint == "" {
		t.Fatal("imported verifier key has no primary fingerprint")
	}
	list, err := exec.Command("gpg", "--homedir", destination, "--batch", "--with-colons", "--list-keys").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(list), "pub:u:") {
		t.Fatalf("imported verifier key is not ultimately trusted:\n%s", list)
	}
	ownerTrust, err := exec.Command("gpg", "--homedir", destination, "--batch", "--export-ownertrust").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ownerTrust), ":6:") {
		t.Fatalf("verifier key ownertrust is missing:\n%s", ownerTrust)
	}
}

func TestRemoveBuiltPackagesFromVDBKeepsImageBaseline(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"var/db/pkg/app-misc/jq-1.8.2",
		"var/db/pkg/app-misc/jq-extras-1.0",
		"var/db/pkg/dev-libs/oniguruma-6.9.10",
		"var/db/pkg/sys-libs/glibc-2.43-r2",
	} {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeBuiltPackagesFromVDB(root, []string{"app-misc/jq-1.8.2", "dev-libs/oniguruma-6.9.10"}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"var/db/pkg/app-misc/jq-1.8.2", "var/db/pkg/dev-libs/oniguruma-6.9.10"} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("job package remained in baseline VDB: %s", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "var/db/pkg/sys-libs/glibc-2.43-r2")); err != nil {
		t.Fatalf("image baseline package was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "var/db/pkg/app-misc/jq-extras-1.0")); err != nil {
		t.Fatalf("similarly prefixed package was removed: %v", err)
	}
	if err := removeBuiltPackagesFromVDB(root, []string{"app-misc/jq;reboot"}); err == nil {
		t.Fatal("invalid built package atom accepted")
	}
}
