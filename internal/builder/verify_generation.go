package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/signing"
)

const (
	maxVerificationIndexBytes = 16 << 20
	maxVerificationArtifacts  = 256
)

// VerificationArtifact binds an install gate to exact bytes accepted by the
// control plane. The builder downloads only these paths into a fresh PKGDIR.
type VerificationArtifact struct {
	RelativePath string `json:"relative_path"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
}

// VerifyInstallRequest is the authenticated control-plane-to-builder contract
// for one hermetic install check.
type VerifyInstallRequest struct {
	PackageName      string                 `json:"package_name"`
	BinhostURL       string                 `json:"binhost_url"`
	Generation       string                 `json:"generation"`
	GPGPubkey        string                 `json:"gpg_pubkey,omitempty"`
	ExpectedKeyID    string                 `json:"expected_key_id,omitempty"`
	BuiltPackages    []string               `json:"built_packages"`
	Artifacts        []VerificationArtifact `json:"artifacts"`
	RequireSignature bool                   `json:"require_signature"`
}

func validateVerifyInstallRequest(request VerifyInstallRequest) error {
	if !atomPattern.MatchString(request.PackageName) {
		return fmt.Errorf("invalid package atom %q", request.PackageName)
	}
	if request.Generation != "unsigned" && request.Generation != "signed" {
		return fmt.Errorf("verification generation must be unsigned or signed")
	}
	if request.RequireSignature != (request.Generation == "signed") {
		return fmt.Errorf("signed generation and signature policy disagree")
	}
	if request.RequireSignature && (request.GPGPubkey == "" || request.ExpectedKeyID == "") {
		return fmt.Errorf("signed verification requires a public key and expected key ID")
	}
	parsed, err := neturl.Parse(request.BinhostURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("verification binhost must be an absolute http or https URL without credentials, query, or fragment")
	}
	if len(request.Artifacts) == 0 || len(request.Artifacts) > maxVerificationArtifacts {
		return fmt.Errorf("verification requires 1..%d artifacts", maxVerificationArtifacts)
	}
	seen := make(map[string]struct{}, len(request.Artifacts))
	for _, artifact := range request.Artifacts {
		candidate := signing.Artifact{
			RelativePath: artifact.RelativePath,
			InputDigest:  artifact.SHA256,
			InputSize:    artifact.Size,
		}
		if err := signing.ValidateArtifact(candidate, false); err != nil {
			return err
		}
		if artifact.Size <= 0 {
			return fmt.Errorf("verification artifact %q must not be empty", artifact.RelativePath)
		}
		if _, exists := seen[artifact.RelativePath]; exists {
			return fmt.Errorf("duplicate verification artifact %q", artifact.RelativePath)
		}
		seen[artifact.RelativePath] = struct{}{}
	}
	for _, cpv := range request.BuiltPackages {
		if !atomPattern.MatchString(cpv) || strings.Count(cpv, "/") != 1 {
			return fmt.Errorf("invalid built package CPV %q", cpv)
		}
	}
	return nil
}

// prefetchVerificationGeneration retrieves one immutable Packages index and
// the exact manifest supplied by the control plane. Redirects and mismatched
// bytes are rejected before Portage starts.
func prefetchVerificationGeneration(baseURL, pkgDir string, artifacts []VerificationArtifact) error {
	client := &http.Client{
		Timeout: 15 * time.Minute,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("verification downloads must not redirect")
		},
	}
	indexPath := filepath.Join(pkgDir, "Packages")
	if err := downloadVerificationFile(client, baseURL, "Packages", indexPath, maxVerificationIndexBytes, ""); err != nil {
		return err
	}
	index, err := os.ReadFile(indexPath) // #nosec G304 -- indexPath is the fixed Packages file beneath the internally-created verify PKGDIR.
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		if !indexContainsArtifact(index, artifact.RelativePath) {
			return fmt.Errorf("packages index does not contain exact path %q", artifact.RelativePath)
		}
		destination := filepath.Join(pkgDir, filepath.FromSlash(artifact.RelativePath))
		if err := downloadVerificationFile(client, baseURL, artifact.RelativePath, destination, artifact.Size, artifact.SHA256); err != nil {
			return err
		}
	}
	return verifyLocalArtifactSet(pkgDir, artifacts)
}

func indexContainsArtifact(index []byte, relativePath string) bool {
	needle := "PATH: " + filepath.ToSlash(relativePath)
	for _, line := range strings.Split(string(index), "\n") {
		if strings.TrimSpace(line) == needle {
			return true
		}
	}
	return false
}

func downloadVerificationFile(client *http.Client, baseURL, relativePath, destination string, expectedSize int64, expectedDigest string) error {
	url, err := verificationObjectURL(baseURL, relativePath)
	if err != nil {
		return err
	}
	response, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("download %q: %w", relativePath, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %q returned HTTP %d", relativePath, response.StatusCode)
	}
	if expectedDigest != "" && response.ContentLength >= 0 && response.ContentLength != expectedSize {
		return fmt.Errorf("download %q size header is %d, expected %d", relativePath, response.ContentLength, expectedSize)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil { // #nosec G301 -- the isolated verify root must be traversable by Portage.
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".download-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()

	hash := sha256.New()
	limit := expectedSize
	if expectedDigest == "" {
		limit = maxVerificationIndexBytes
	}
	size, copyErr := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(response.Body, limit+1))
	closeErr := temp.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if size > limit {
		return fmt.Errorf("download %q exceeds %d bytes", relativePath, limit)
	}
	if expectedDigest != "" {
		digest := hex.EncodeToString(hash.Sum(nil))
		if size != expectedSize || digest != expectedDigest {
			return fmt.Errorf("download %q proof mismatch: size=%d sha256=%s", relativePath, size, digest)
		}
	}
	if err := os.Chmod(tempPath, 0o644); err != nil { // #nosec G302 -- this is a public signing key copied into the verification binhost.
		return err
	}
	if err := os.Rename(tempPath, destination); err != nil {
		return err
	}
	return nil
}

func verificationObjectURL(baseURL, relativePath string) (string, error) {
	parsed, err := neturl.Parse(baseURL)
	if err != nil {
		return "", err
	}
	clean := filepath.ToSlash(filepath.Clean(relativePath))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("invalid verification object path %q", relativePath)
	}
	escaped := make([]string, 0, strings.Count(clean, "/")+1)
	for _, part := range strings.Split(clean, "/") {
		escaped = append(escaped, neturl.PathEscape(part))
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.Join(escaped, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

func verifyLocalArtifactSet(pkgDir string, artifacts []VerificationArtifact) error {
	for _, artifact := range artifacts {
		file := filepath.Join(pkgDir, filepath.FromSlash(artifact.RelativePath))
		handle, err := os.Open(file) // #nosec G304 -- artifact.RelativePath is validated and confined to the temporary PKGDIR.
		if err != nil {
			return err
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, handle)
		closeErr := handle.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		if size != artifact.Size || digest != artifact.SHA256 {
			return fmt.Errorf("artifact %q changed: size=%d sha256=%s", artifact.RelativePath, size, digest)
		}
	}
	return nil
}

func parsePrimaryFingerprints(colonOutput []byte) []string {
	var fingerprints []string
	wantFingerprint := false
	for _, line := range strings.Split(string(colonOutput), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "pub" {
			wantFingerprint = true
			continue
		}
		if wantFingerprint && fields[0] == "fpr" && len(fields) > 9 && fields[9] != "" {
			fingerprints = append(fingerprints, fields[9])
			wantFingerprint = false
		}
	}
	return fingerprints
}

func assertVerifierKeyring(gpgHome, expectedFingerprint string) error {
	command := exec.Command("gpg", "--homedir", gpgHome, "--batch", "--with-colons", "--list-keys") // #nosec G204 -- fixed gpg executable and isolated internally-created keyring.
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("list verifier keyring: %w: %s", err, strings.TrimSpace(string(output)))
	}
	fingerprints := parsePrimaryFingerprints(output)
	if len(fingerprints) != 1 || fingerprints[0] != expectedFingerprint {
		return fmt.Errorf("verifier keyring changed during emerge: expected only %s, got %s",
			expectedFingerprint, strings.Join(fingerprints, ","))
	}
	return nil
}

func verificationDigestSummary(artifacts []VerificationArtifact) string {
	parts := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		parts = append(parts, artifact.RelativePath+"="+artifact.SHA256)
	}
	return strings.Join(parts, ",")
}
