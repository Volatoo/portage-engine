// Package imagefactory validates offline image-factory inputs and produces
// immutable image provenance manifests without performing network access.
package imagefactory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const maxDefinitionBytes int64 = 4 << 20

var objectIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._/-]{0,255}$`)
var platformPattern = regexp.MustCompile(`^[a-z0-9]+-[a-z0-9]+$`)

// InputLock is the reviewed inventory copied into an air-gapped image factory.
type InputLock struct {
	Version        int           `json:"version"`
	BundleID       string        `json:"bundle_id"`
	StrictOffline  bool          `json:"strict_offline"`
	AllowedHosts   []string      `json:"allowed_hosts"`
	AdvisoryCutoff string        `json:"advisory_cutoff,omitempty"`
	Objects        []InputObject `json:"objects"`
}

// InputObject identifies one content-addressed file under the offline root.
type InputObject struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Path        string   `json:"path"`
	Platform    string   `json:"platform,omitempty"`
	SHA256      string   `json:"sha256"`
	Size        int64    `json:"size"`
	Executable  bool     `json:"executable,omitempty"`
	RequiredFor []string `json:"required_for,omitempty"`
}

// Finding describes a missing or invalid offline object.
type Finding struct {
	Kind            string `json:"kind"`
	ID              string `json:"id"`
	Path            string `json:"path"`
	ExpectedDigest  string `json:"expected_digest,omitempty"`
	ActualDigest    string `json:"actual_digest,omitempty"`
	Reason          string `json:"reason"`
	FallbackAllowed bool   `json:"fallback_allowed"`
}

// PreflightReport is deterministic apart from CheckedAt and is safe to retain
// as build evidence. Missing is the complete set, not the first error.
type PreflightReport struct {
	CatalogVersion int       `json:"catalog_version"`
	BundleID       string    `json:"mirror_bundle_id"`
	Target         string    `json:"target"`
	StrictOffline  bool      `json:"strict_offline"`
	RunnerPlatform string    `json:"runner_platform"`
	CheckedAt      time.Time `json:"checked_at"`
	Verified       int       `json:"verified"`
	Missing        []Finding `json:"missing"`
}

// LoadInputLock strictly decodes and validates an input lock.
func LoadInputLock(path string) (*InputLock, error) {
	var lock InputLock
	if err := decodeStrictFile(path, &lock); err != nil {
		return nil, err
	}
	if err := lock.Validate(); err != nil {
		return nil, fmt.Errorf("validate input lock: %w", err)
	}
	return &lock, nil
}

// MaterializeInputLock replaces draft digests and sizes with measurements of
// the exact regular files under root. The returned lock still requires normal
// bundle sealing/signing before it becomes trusted input.
func MaterializeInputLock(draftPath, root string) (*InputLock, error) {
	lock, err := LoadInputLock(draftPath)
	if err != nil {
		return nil, fmt.Errorf("load input-lock draft: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve offline root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("offline root is not a directory")
	}
	for index := range lock.Objects {
		object := &lock.Objects[index]
		path := filepath.Join(root, object.Path)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("resolve object %q: %w", object.ID, err)
		}
		relative, err := filepath.Rel(root, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || resolved != path {
			return nil, fmt.Errorf("object %q is a symlink or escapes the offline root", object.ID)
		}
		fileInfo, err := os.Lstat(resolved)
		if err != nil || !fileInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("object %q is not a regular file", object.ID)
		}
		if object.Executable && fileInfo.Mode()&0o111 == 0 {
			return nil, fmt.Errorf("object %q is not executable", object.ID)
		}
		digest, err := digestFile(resolved)
		if err != nil {
			return nil, err
		}
		object.SHA256 = digest
		object.Size = fileInfo.Size()
	}
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	return lock, nil
}

// Validate rejects ambiguous paths, weak digests, duplicate IDs/paths and any
// configuration that permits public fallback.
func (l *InputLock) Validate() error {
	if l == nil || l.Version < 1 || !objectIDPattern.MatchString(l.BundleID) {
		return fmt.Errorf("invalid version or bundle_id")
	}
	if !l.StrictOffline {
		return fmt.Errorf("strict_offline must be true")
	}
	if len(l.AllowedHosts) == 0 {
		return fmt.Errorf("allowed_hosts must name the internal endpoints")
	}
	seenHosts := make(map[string]struct{}, len(l.AllowedHosts))
	for _, host := range l.AllowedHosts {
		if host == "" || strings.ContainsAny(host, "/:@?# \\\r\n\x00") || strings.EqualFold(host, "localhost") {
			return fmt.Errorf("invalid allowed host %q", host)
		}
		if _, exists := seenHosts[host]; exists {
			return fmt.Errorf("duplicate allowed host %q", host)
		}
		seenHosts[host] = struct{}{}
	}
	if len(l.Objects) == 0 {
		return fmt.Errorf("objects must not be empty")
	}
	seenIDs := make(map[string]struct{}, len(l.Objects))
	seenPaths := make(map[string]struct{}, len(l.Objects))
	for _, object := range l.Objects {
		if !objectIDPattern.MatchString(object.ID) {
			return fmt.Errorf("invalid object ID %q", object.ID)
		}
		if !validObjectKind(object.Kind) {
			return fmt.Errorf("object %q has unsupported kind %q", object.ID, object.Kind)
		}
		if platformRequired(object.Kind) && !platformPattern.MatchString(object.Platform) {
			return fmt.Errorf("object %q requires a goos-goarch platform", object.ID)
		}
		if object.Path == "" || filepath.IsAbs(object.Path) || filepath.Clean(object.Path) != object.Path || strings.HasPrefix(object.Path, ".."+string(filepath.Separator)) || object.Path == ".." {
			return fmt.Errorf("object %q has unsafe path %q", object.ID, object.Path)
		}
		if object.Kind == "terraform-provider" {
			filename := filepath.Base(object.Path)
			expectedSuffix := "_" + strings.ReplaceAll(object.Platform, "-", "_") + ".zip"
			if object.Executable || !strings.HasPrefix(filename, "terraform-provider-") || !strings.HasSuffix(filename, expectedSuffix) {
				return fmt.Errorf("object %q must be a non-executable packed Terraform provider for %s", object.ID, object.Platform)
			}
		}
		if object.Kind == "ca-bundle" && (object.Executable || filepath.Ext(object.Path) != ".crt") {
			return fmt.Errorf("object %q must be a non-executable .crt CA bundle", object.ID)
		}
		if len(object.SHA256) != 64 {
			return fmt.Errorf("object %q requires a lowercase SHA-256", object.ID)
		}
		if _, err := hex.DecodeString(object.SHA256); err != nil || object.SHA256 != strings.ToLower(object.SHA256) {
			return fmt.Errorf("object %q requires a lowercase SHA-256", object.ID)
		}
		if object.Size < 0 {
			return fmt.Errorf("object %q has a negative size", object.ID)
		}
		if _, exists := seenIDs[object.ID]; exists {
			return fmt.Errorf("duplicate object ID %q", object.ID)
		}
		if _, exists := seenPaths[object.Path]; exists {
			return fmt.Errorf("duplicate object path %q", object.Path)
		}
		seenIDs[object.ID] = struct{}{}
		seenPaths[object.Path] = struct{}{}
		for _, target := range object.RequiredFor {
			if !objectIDPattern.MatchString(target) {
				return fmt.Errorf("object %q has invalid target %q", object.ID, target)
			}
		}
	}
	return nil
}

// Preflight verifies every object required for target and returns all findings.
func Preflight(root string, lock *InputLock, target string) (*PreflightReport, error) {
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	if target == "" || !objectIDPattern.MatchString(target) {
		return nil, fmt.Errorf("invalid target %q", target)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve offline root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve offline root symlinks: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return nil, fmt.Errorf("offline root is not a directory")
	}
	report := &PreflightReport{
		CatalogVersion: lock.Version, BundleID: lock.BundleID, Target: target,
		StrictOffline: true, RunnerPlatform: runtime.GOOS + "-" + runtime.GOARCH,
		CheckedAt: time.Now().UTC(), Missing: []Finding{},
	}
	for _, object := range lock.Objects {
		if !requiredFor(object, target) {
			continue
		}
		if hostRuntimeKind(object.Kind) && object.Platform != report.RunnerPlatform {
			report.Missing = append(report.Missing, *newFinding(object,
				fmt.Sprintf("runner platform mismatch: got %s, requires %s", report.RunnerPlatform, object.Platform), ""))
			continue
		}
		finding := verifyObject(root, object)
		if finding != nil {
			report.Missing = append(report.Missing, *finding)
			continue
		}
		report.Verified++
	}
	sort.Slice(report.Missing, func(i, j int) bool { return report.Missing[i].ID < report.Missing[j].ID })
	return report, nil
}

func verifyObject(root string, object InputObject) *Finding {
	fullPath := filepath.Join(root, object.Path)
	resolvedPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		reason := err.Error()
		if errors.Is(err, os.ErrNotExist) {
			reason = "not present in offline root"
		}
		return newFinding(object, reason, "")
	}
	relative, err := filepath.Rel(root, resolvedPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return newFinding(object, "object resolves outside the offline root", "")
	}
	info, err := os.Lstat(resolvedPath)
	if err != nil {
		return newFinding(object, err.Error(), "")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return newFinding(object, "object is not a regular file", "")
	}
	if info.Size() != object.Size {
		return newFinding(object, fmt.Sprintf("size mismatch: got %d, expected %d", info.Size(), object.Size), "")
	}
	if object.Executable && info.Mode()&0o111 == 0 {
		return newFinding(object, "object is not executable", "")
	}
	digest, err := digestFile(resolvedPath)
	if err != nil {
		return newFinding(object, err.Error(), "")
	}
	if digest != object.SHA256 {
		return newFinding(object, "SHA-256 mismatch", digest)
	}
	if object.Kind == "pbs-source-attestation" {
		if _, err := LoadPBSSourceAttestation(resolvedPath); err != nil {
			return newFinding(object, "invalid PBS source attestation: "+err.Error(), digest)
		}
	}
	return nil
}

func newFinding(object InputObject, reason, actual string) *Finding {
	return &Finding{
		Kind: object.Kind, ID: object.ID, Path: object.Path,
		ExpectedDigest: "sha256:" + object.SHA256, ActualDigest: prefixDigest(actual),
		Reason: reason, FallbackAllowed: false,
	}
}

func requiredFor(object InputObject, target string) bool {
	if len(object.RequiredFor) == 0 {
		return true
	}
	for _, candidate := range object.RequiredFor {
		if candidate == target {
			return true
		}
	}
	return false
}

func validObjectKind(kind string) bool {
	switch kind {
	case "packer", "packer-plugin", "plugin-checksum", "terraform", "terraform-provider", "terraform-lock", "service-binary", "seed", "pbs-source-attestation", "repository-snapshot", "distfile", "binpkg", "manifest", "distfile-manifest", "package-set-catalog", "build-plan", "image-manifest", "script",
		"catalyst-plan", "catalyst-runtime", "stage3", "stage3-digests", "release-key", "ca-bundle", "rootfs-manifest", "qcow2-manifest":
		return true
	default:
		return false
	}
}

func platformRequired(kind string) bool {
	return hostRuntimeKind(kind) || kind == "service-binary"
}

func hostRuntimeKind(kind string) bool {
	switch kind {
	case "packer", "packer-plugin", "plugin-checksum", "terraform", "terraform-provider", "catalyst-runtime":
		return true
	default:
		return false
	}
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- path is validated under operator-provided offline root.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func decodeStrictFile(path string, dst any) error {
	file, err := os.Open(path) // #nosec G304 -- operator-provided definition path.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, maxDefinitionBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("definition contains trailing JSON")
	}
	return nil
}

func prefixDigest(digest string) string {
	if digest == "" {
		return ""
	}
	return "sha256:" + digest
}
