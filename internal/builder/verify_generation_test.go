package builder

import (
	"archive/tar"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"

	"github.com/slchris/portage-engine/internal/signing"
)

func TestPrefetchVerificationGenerationBindsExactBytes(t *testing.T) {
	const rel = "app-misc/jq/jq-1.8.2.gpkg.tar"
	payload := []byte("exact signed generation bytes")
	digest := sha256.Sum256(payload)
	index := []byte("VERSION: 0\nPACKAGES: 1\n\nCPV: app-misc/jq-1.8.2\nPATH: " + rel + "\nSIZE: 29\n\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/generation/Packages":
			_, _ = w.Write(index)
		case "/generation/" + rel:
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pkgDir := t.TempDir()
	artifact := VerificationArtifact{
		RelativePath: rel,
		SHA256:       hex.EncodeToString(digest[:]),
		Size:         int64(len(payload)),
	}
	if err := prefetchVerificationGeneration(server.URL+"/generation", pkgDir, []VerificationArtifact{artifact}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(pkgDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("downloaded payload = %q, want %q", got, payload)
	}
}

func TestPrefetchVerificationGenerationRejectsDigestMismatch(t *testing.T) {
	const rel = "app-misc/jq/jq-1.8.2.gpkg.tar"
	payload := []byte("unsigned or tampered bytes")
	index := []byte("VERSION: 0\nPACKAGES: 1\n\nCPV: app-misc/jq-1.8.2\nPATH: " + rel + "\n\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Packages") {
			_, _ = w.Write(index)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	artifact := VerificationArtifact{
		RelativePath: rel,
		SHA256:       strings.Repeat("0", 64),
		Size:         int64(len(payload)),
	}
	err := prefetchVerificationGeneration(server.URL, t.TempDir(), []VerificationArtifact{artifact})
	if err == nil || !strings.Contains(err.Error(), "proof mismatch") {
		t.Fatalf("prefetch error = %v, want digest proof mismatch", err)
	}
}

func TestPrefetchVerificationGenerationRejectsRedirect(t *testing.T) {
	const rel = "app-misc/jq/jq-1.8.2.gpkg.tar"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("unexpected redirected content"))
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL, http.StatusFound)
	}))
	defer server.Close()

	artifact := VerificationArtifact{
		RelativePath: rel,
		SHA256:       strings.Repeat("0", 64),
		Size:         1,
	}
	err := prefetchVerificationGeneration(server.URL, t.TempDir(), []VerificationArtifact{artifact})
	if err == nil || !strings.Contains(err.Error(), "must not redirect") {
		t.Fatalf("prefetch error = %v, want redirect rejection", err)
	}
}

func TestValidateVerifyInstallRequestFailsClosed(t *testing.T) {
	base := VerifyInstallRequest{
		PackageName:      "app-misc/jq",
		BinhostURL:       "http://binhost.internal/generation",
		Generation:       "signed",
		RequireSignature: true,
		Artifacts: []VerificationArtifact{{
			RelativePath: "app-misc/jq/jq-1.8.2.gpkg.tar",
			SHA256:       strings.Repeat("a", 64),
			Size:         1,
		}},
	}
	if err := validateVerifyInstallRequest(base); err == nil || !strings.Contains(err.Error(), "public key") {
		t.Fatalf("missing-key validation error = %v", err)
	}
	base.Generation = "unsigned"
	if err := validateVerifyInstallRequest(base); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("signature downgrade validation error = %v", err)
	}
}

func TestIndependentSignedPreflightRejectsUnsignedGPKG(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg is not installed")
	}
	home, err := os.MkdirTemp("/tmp", "pe-unsigned-negative-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "unsigned.gpkg.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	version := []byte("1\n")
	sha := sha512.Sum512(version)
	blake := blake2b.Sum512(version)
	manifest := []byte("DATA gpkg-1 " + strconv.Itoa(len(version)) +
		" BLAKE2B " + hex.EncodeToString(blake[:]) +
		" SHA512 " + hex.EncodeToString(sha[:]) + "\n")
	writer := tar.NewWriter(file)
	for _, member := range []struct {
		name string
		data []byte
	}{
		{name: "unsigned/gpkg-1", data: version},
		{name: "unsigned/Manifest", data: manifest},
	} {
		header := &tar.Header{Name: member.name, Mode: 0o644, Size: int64(len(member.data))}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(member.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if err := (signing.GPG{Home: home, KeyID: "unused"}).VerifyGPKG(path); err == nil ||
		!strings.Contains(err.Error(), "Manifest signature") {
		t.Fatal("unsigned GPKG passed the independent signed-artifact preflight")
	}
}
