package builder

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMarkerGPKG writes a tar whose single member name is not the
// "<prefix>/<name>" shape a GPKG uses. VerifyGPKG rejects that name by quoting
// it, which is what lets the test below say which file the preflight opened
// rather than only that it failed.
func writeMarkerGPKG(t *testing.T, path, member string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path) // #nosec G304 -- test-owned temporary path.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	writer := tar.NewWriter(file)
	body := []byte("marker payload")
	if err := writer.WriteHeader(&tar.Header{
		Name: member, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatPAX,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

// The signature preflight runs against a manifest the control plane sent, in a
// verify root the builder created. verificationArtifactPath documents itself as
// the rule that makes every join onto that root safe, including this one, so
// the preflight must not reach a file the manifest points at from outside it -
// even though today's only caller has already been through the prefetch.
func TestSignaturePreflightRefusesAManifestPathOutOfTheVerifyPkgDir(t *testing.T) {
	host := t.TempDir()
	// The verify root is a temporary directory of the builder's own making, and
	// PKGDIR sits three levels inside it, so the escape is spelled from there.
	root := filepath.Join(host, "pe-verify-root")
	pkgDir := filepath.Join(root, "var", "cache", "binpkgs")
	gpgHome := filepath.Join(root, "etc", "portage", "gnupg")
	writeMarkerGPKG(t, filepath.Join(host, "outside.gpkg.tar"), "./pe-host-file-marker")
	const inside = "app-misc/jq-1.8.2-1.gpkg.tar"
	writeMarkerGPKG(t, filepath.Join(pkgDir, filepath.FromSlash(inside)), "./pe-verify-pkgdir-marker")

	escape := filepath.ToSlash(filepath.Join("..", "..", "..", "..", "outside.gpkg.tar"))
	err := verifySignedArtifactSet(pkgDir, gpgHome, "DEADBEEFDEADBEEF",
		[]VerificationArtifact{{RelativePath: escape}})
	if err == nil {
		t.Fatal("the signature preflight accepted a manifest path outside the verify PKGDIR")
	}
	if strings.Contains(err.Error(), "pe-host-file-marker") {
		t.Fatalf("the signature preflight opened a file outside the verify PKGDIR: %v", err)
	}

	// The gate rejects escapes, not the artifacts every real verification job
	// carries: a canonical entry must still be handed to GPG verification.
	err = verifySignedArtifactSet(pkgDir, gpgHome, "DEADBEEFDEADBEEF",
		[]VerificationArtifact{{RelativePath: inside}})
	if err == nil || !strings.Contains(err.Error(), "pe-verify-pkgdir-marker") {
		t.Fatalf("a canonical manifest entry no longer reaches GPG verification: %v", err)
	}
}
