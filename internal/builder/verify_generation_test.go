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

// loopbackVerificationClient is the verification fetcher without its dial
// policy. The tests below cover the guarantees that policy is orthogonal to —
// exact bytes, digest binding, redirect refusal — and an httptest server can
// only be reached over loopback, which the shipped fetcher refuses on purpose.
// The policy itself is exercised by
// TestPrefetchVerificationGenerationRefusesWorkerLocalTargets.
func loopbackVerificationClient() *http.Client {
	return verificationHTTPClient(nil)
}

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
	if err := prefetchVerificationGenerationVia(loopbackVerificationClient(),
		server.URL+"/generation", pkgDir, []VerificationArtifact{artifact}); err != nil {
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
	err := prefetchVerificationGenerationVia(loopbackVerificationClient(),
		server.URL, t.TempDir(), []VerificationArtifact{artifact})
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
	err := prefetchVerificationGenerationVia(loopbackVerificationClient(),
		server.URL, t.TempDir(), []VerificationArtifact{artifact})
	if err == nil || !strings.Contains(err.Error(), "must not redirect") {
		t.Fatalf("prefetch error = %v, want redirect rejection", err)
	}
}

// TestPrefetchVerificationGenerationRefusesWorkerLocalTargets drives the
// request-forgery attack: a binhost URL is untrusted input, and the builder
// sits inside the build VM, so the URL is aimed at the two things the VM can
// reach and the party supplying the URL cannot — the hypervisor's link-local
// metadata service and the worker's own loopback surface.
func TestPrefetchVerificationGenerationRefusesWorkerLocalTargets(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("worker-only response that must never be fetched"))
	}))
	defer local.Close()

	artifact := VerificationArtifact{
		RelativePath: "app-misc/jq/jq-1.8.2.gpkg.tar",
		SHA256:       strings.Repeat("0", 64),
		Size:         1,
	}
	for name, baseURL := range map[string]string{
		"cloud metadata":  "http://169.254.169.254/latest/meta-data",
		"worker loopback": local.URL,
		"loopback by name": "http://localhost:" +
			local.URL[strings.LastIndex(local.URL, ":")+1:],
		"unspecified": "http://0.0.0.0:9090",
	} {
		t.Run(name, func(t *testing.T) {
			err := prefetchVerificationGeneration(baseURL, t.TempDir(), []VerificationArtifact{artifact})
			if err == nil {
				t.Fatalf("prefetch from %s succeeded", baseURL)
			}
			if !strings.Contains(err.Error(), "the build VM must never fetch from") {
				t.Fatalf("prefetch from %s failed with %v, want the address policy refusal", baseURL, err)
			}
		})
	}
}

// TestValidateVerifyInstallRequestRefusesWorkerLocalBinhost proves the same
// policy fails the request at the contract boundary, and that an ordinary
// private-LAN binhost — which is what every real deployment publishes — still
// passes.
func TestValidateVerifyInstallRequestRefusesWorkerLocalBinhost(t *testing.T) {
	base := VerifyInstallRequest{
		PackageName: "app-misc/jq",
		Generation:  "unsigned",
		Artifacts: []VerificationArtifact{{
			RelativePath: "app-misc/jq/jq-1.8.2.gpkg.tar",
			SHA256:       strings.Repeat("a", 64),
			Size:         1,
		}},
	}
	for _, blocked := range []string{
		"http://169.254.169.254/latest/meta-data",
		"http://127.0.0.1:9090/api/v1/status",
		"http://localhost:2112/metrics",
		"http://[::1]:9090/",
		"http://[::ffff:127.0.0.1]:9090/",
	} {
		base.BinhostURL = blocked
		err := validateVerifyInstallRequest(base)
		if err == nil || !strings.Contains(err.Error(), "the build VM must never fetch from") {
			t.Fatalf("validation of %s = %v, want the address policy refusal", blocked, err)
		}
	}
	for _, allowed := range []string{
		"http://10.31.0.2:8080/verify-binhost/abc",
		"http://10.42.0.156:8080/verify-binhost/abc",
		"https://binhost.example.org/verify-binhost/abc",
	} {
		base.BinhostURL = allowed
		if err := validateVerifyInstallRequest(base); err != nil {
			t.Fatalf("validation of legitimate binhost %s = %v", allowed, err)
		}
	}
}

// TestPrefetchVerificationGenerationConfinesArtifactPaths checks the rule the
// four dismissed path-injection alerts rest on, at the place that rests on it,
// rather than leaving it as an assertion in a scanner's UI.
func TestPrefetchVerificationGenerationConfinesArtifactPaths(t *testing.T) {
	pkgDir := t.TempDir()
	for _, escape := range []string{
		"../../../../etc/portage/make.conf",
		"/etc/portage/make.conf",
		"app-misc/../../escape.gpkg.tar",
		"./app-misc/jq.gpkg.tar",
		"",
	} {
		if _, err := verificationArtifactPath(pkgDir, escape); err == nil {
			t.Fatalf("verification artifact path %q was accepted", escape)
		}
	}
	got, err := verificationArtifactPath(pkgDir, "app-misc/jq/jq-1.8.2.gpkg.tar")
	if err != nil {
		t.Fatalf("legitimate artifact path rejected: %v", err)
	}
	if want := filepath.Join(pkgDir, "app-misc", "jq", "jq-1.8.2.gpkg.tar"); got != want {
		t.Fatalf("verification artifact path = %q, want %q", got, want)
	}

	index := []byte("VERSION: 0\nPACKAGES: 1\n\nCPV: app-misc/jq-1.8.2\nPATH: ../../escape.gpkg.tar\n\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(index)
	}))
	defer server.Close()
	err = prefetchVerificationGenerationVia(loopbackVerificationClient(), server.URL, pkgDir,
		[]VerificationArtifact{{
			RelativePath: "../../escape.gpkg.tar",
			SHA256:       strings.Repeat("0", 64),
			Size:         1,
		}})
	if err == nil || !strings.Contains(err.Error(), "not canonical and relative") {
		t.Fatalf("prefetch of an escaping manifest path = %v, want a confinement refusal", err)
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
