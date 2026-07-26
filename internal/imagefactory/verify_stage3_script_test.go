package imagefactory

import (
	"crypto/sha512"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyStage3DistinguishesSHA512FromBLAKE2B(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage3-test.tar.xz")
	payload := []byte("reviewed stage3 fixture")
	if err := os.WriteFile(stage, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha512.Sum512(payload)
	sha := hex.EncodeToString(digest[:])
	blakeLike := strings.Repeat("b", 128)
	digests := strings.Join([]string{
		"-----BEGIN PGP SIGNED MESSAGE-----",
		"Hash: SHA256",
		"",
		"# SHA512 HASH",
		sha + "  stage3-test.tar.xz",
		"# BLAKE2B HASH",
		blakeLike + "  stage3-test.tar.xz",
		"-----BEGIN PGP SIGNATURE-----",
		"fixture-signature-is-verified-by-the-caller",
	}, "\n")
	digestsPath := filepath.Join(dir, "stage3-test.tar.xz.DIGESTS")
	if err := os.WriteFile(digestsPath, []byte(digests), 0o600); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join("..", "..", "image-factory", "catalyst", "verify-stage3.py")
	command := exec.Command("python3", script, stage, digestsPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("verify-stage3 rejected a valid SHA512 entry followed by BLAKE2B: %v\n%s", err, output)
	}
}

func TestVerifyStage3RejectsDigestOutsideSHA512Section(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage3-test.tar.xz")
	if err := os.WriteFile(stage, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	digestsPath := filepath.Join(dir, "stage3-test.tar.xz.DIGESTS")
	bogus := "# BLAKE2B HASH\n" + strings.Repeat("a", 128) + "  stage3-test.tar.xz\n"
	if err := os.WriteFile(digestsPath, []byte(bogus), 0o600); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join("..", "..", "image-factory", "catalyst", "verify-stage3.py")
	command := exec.Command("python3", script, stage, digestsPath)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("verify-stage3 accepted BLAKE2B as SHA512: %s", output)
	}
}
