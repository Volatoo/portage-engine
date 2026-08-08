package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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
	if err := checkVerificationBinhostHost(parsed.Hostname()); err != nil {
		return err
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

// verificationBinhostReachError explains a refused verification address in the
// terms an operator debugging a failed job needs.
type verificationBinhostReachError struct {
	target string
	reason string
}

func (e *verificationBinhostReachError) Error() string {
	return fmt.Sprintf("verification binhost %s is a %s address the build VM must never fetch from", e.target, e.reason)
}

// unreachableVerificationAddress names the addresses a verification download is
// never allowed to reach. The builder lives inside the disposable build VM, so
// its socket sees a network the party supplying the binhost URL does not: the
// hypervisor's link-local metadata service, which hands out cloud-init user
// data and instance credentials, and whatever the VM image bound to loopback
// precisely because loopback was assumed private. That URL is untrusted input
// on both control surfaces — an operator POST to /api/v1/verify, a gateway task
// payload in pull mode — so it can name either of them.
//
// Private LAN ranges deliberately stay reachable. The quarantine binhost really
// does live on an internal address; SERVER_CALLBACK_URL is required to be
// routable from the VM, and refusing RFC1918 would refuse every real
// deployment.
func unreachableVerificationAddress(addr netip.Addr) string {
	switch addr = addr.Unmap(); {
	case !addr.IsValid():
		return "malformed"
	case addr.IsLoopback():
		return "loopback"
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		return "link-local"
	case addr.IsUnspecified():
		return "unspecified"
	case addr.IsInterfaceLocalMulticast(), addr.IsMulticast():
		return "multicast"
	}
	return ""
}

// checkVerificationBinhostHost rejects a binhost that spells the forbidden
// destination as a literal, so the request fails at the contract boundary with
// a legible reason instead of deep inside a download. A name is left to the
// dialer below: only the dialer sees which address the name actually resolved
// to at connect time.
func checkVerificationBinhostHost(host string) error {
	if strings.EqualFold(host, "localhost") {
		return &verificationBinhostReachError{target: host, reason: "loopback"}
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	if reason := unreachableVerificationAddress(addr); reason != "" {
		return &verificationBinhostReachError{target: host, reason: reason}
	}
	return nil
}

// refuseUnreachableVerificationDial is the dialer hook that makes the policy
// stick. It runs once per connection against the concrete address the socket is
// about to use, so a host name whose second DNS answer points at the metadata
// service is refused just as a literal is.
func refuseUnreachableVerificationDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return &verificationBinhostReachError{target: address, reason: "malformed"}
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return &verificationBinhostReachError{target: host, reason: "malformed"}
	}
	if reason := unreachableVerificationAddress(addr); reason != "" {
		return &verificationBinhostReachError{target: host, reason: reason}
	}
	return nil
}

// verificationHTTPClient builds the fetcher used for one generation. The
// control hook is a parameter so the transport's other guarantees can be
// exercised on their own; every production path passes
// refuseUnreachableVerificationDial.
//
// The transport carries no proxy. A proxy would resolve and connect on this
// process's behalf, which would hand the untrusted binhost URL a way around the
// address policy, and nothing in this system fetches a binhost through one:
// the image factory explicitly clears the proxy environment before it builds.
func verificationHTTPClient(control func(network, address string, conn syscall.RawConn) error) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: control}
	return &http.Client{
		Timeout: 15 * time.Minute,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("verification downloads must not redirect")
		},
	}
}

// prefetchVerificationGeneration retrieves one immutable Packages index and
// the exact manifest supplied by the control plane. Redirects and mismatched
// bytes are rejected before Portage starts.
func prefetchVerificationGeneration(baseURL, pkgDir string, artifacts []VerificationArtifact) error {
	return prefetchVerificationGenerationVia(
		verificationHTTPClient(refuseUnreachableVerificationDial), baseURL, pkgDir, artifacts)
}

func prefetchVerificationGenerationVia(client *http.Client, baseURL, pkgDir string, artifacts []VerificationArtifact) error {
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
		destination, err := verificationArtifactPath(pkgDir, artifact.RelativePath)
		if err != nil {
			return err
		}
		if err := downloadVerificationFile(client, baseURL, artifact.RelativePath, destination, artifact.Size, artifact.SHA256); err != nil {
			return err
		}
	}
	return verifyLocalArtifactSet(pkgDir, artifacts)
}

// verificationArtifactPath places one manifest entry inside the throwaway
// PKGDIR the builder just created. Every request that reaches the prefetch has
// already passed validateVerifyInstallRequest, whose signing.ValidateArtifact
// call refuses an absolute or non-canonical relative path, which is why the
// download, the signature preflight and the re-hash below can join a manifest
// path onto a directory at all. That is the whole reason those joins are safe,
// so the rule is spelled out here as a check rather than left as a claim in a
// scanner's dismissal: prefetchVerificationGeneration is reachable from
// anywhere in this package, and a future caller that skips the request
// validator still must not be able to write outside the directory.
func verificationArtifactPath(pkgDir, relativePath string) (string, error) {
	native := filepath.FromSlash(relativePath)
	if relativePath == "" || strings.Contains(relativePath, "\\") ||
		!filepath.IsLocal(native) ||
		filepath.ToSlash(filepath.Clean(native)) != relativePath {
		return "", fmt.Errorf("verification artifact path %q is not canonical and relative", relativePath)
	}
	return filepath.Join(pkgDir, native), nil
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
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil { // #nosec G301,G304 -- destination came from verificationArtifactPath, which confines it to the internally-created verify PKGDIR.
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".download-*") // #nosec G304 -- same confined destination directory.
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
	if err := os.Rename(tempPath, destination); err != nil { // #nosec G304 -- destination came from verificationArtifactPath and stays under the verify PKGDIR.
		return err
	}
	return nil
}

// verificationBinhostURLShape admits only a plain http(s) URL with a bare
// host[:port] authority and an optional plain path: no userinfo, query,
// fragment, or percent-escapes in the authority. The dial-time address policy
// still decides reachability; this lexical gate pins what a URL may spell.
var verificationBinhostURLShape = regexp.MustCompile(
	`^https?://[A-Za-z0-9.\-\[\]:]+(?:/[0-9A-Za-z._~%+/-]*)?$`)

func verificationObjectURL(baseURL, relativePath string) (string, error) {
	if !verificationBinhostURLShape.MatchString(baseURL) {
		return "", fmt.Errorf("verification binhost URL %q is not a plain http(s) URL", baseURL)
	}
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
		file, err := verificationArtifactPath(pkgDir, artifact.RelativePath)
		if err != nil {
			return err
		}
		handle, err := os.Open(file) // #nosec G304 -- verificationArtifactPath confines this read to the temporary PKGDIR.
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
