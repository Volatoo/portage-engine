package signing

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CapabilityMarkerName identifies a validated quarantine capability root.
const CapabilityMarkerName = ".portage-engine-capability"

var (
	digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	tokenPattern  = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// Artifact binds a signing request to one exact unsigned input. Output fields
// are populated only after the isolated signer has generated and verified the
// signed GPKG.
type Artifact struct {
	RelativePath string `json:"relative_path"`
	InputDigest  string `json:"input_digest"`
	InputSize    int64  `json:"input_size"`
	OutputDigest string `json:"output_digest,omitempty"`
	OutputSize   int64  `json:"output_size,omitempty"`
}

// Request binds a job attempt to exact unsigned artifacts for signing.
type Request struct {
	JobID          string     `json:"job_id"`
	AttemptID      string     `json:"attempt_id"`
	AttemptFence   int64      `json:"attempt_fence"`
	LeaseOwner     string     `json:"-"`
	SourceToken    string     `json:"source_token"`
	Architecture   string     `json:"architecture"`
	MaxOutputBytes int64      `json:"max_output_bytes"`
	Artifacts      []Artifact `json:"artifacts"`
}

// Task is a leased, durable signing queue item.
type Task struct {
	ID             string     `json:"id"`
	JobID          string     `json:"job_id"`
	AttemptID      string     `json:"attempt_id"`
	AttemptFence   int64      `json:"attempt_fence"`
	State          string     `json:"state"`
	SourceToken    string     `json:"source_token"`
	Architecture   string     `json:"architecture"`
	MaxOutputBytes int64      `json:"max_output_bytes"`
	Artifacts      []Artifact `json:"artifacts"`
	SigningKeyID   string     `json:"signing_key_id,omitempty"`
	Owner          string     `json:"owner,omitempty"`
	ClaimFence     int64      `json:"claim_fence,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	Error          string     `json:"error,omitempty"`
}

// RuntimeStatus summarizes the isolated signing queue.
type RuntimeStatus struct {
	Queued        int        `json:"queued"`
	Claimed       int        `json:"claimed"`
	Completed     int        `json:"completed"`
	Failed        int        `json:"failed"`
	Canceled      int        `json:"canceled"`
	OldestQueued  *time.Time `json:"oldest_queued_at,omitempty"`
	OldestClaimed *time.Time `json:"oldest_claimed_at,omitempty"`
}

// Coordinator is used by the control plane. It can enqueue and observe work,
// but it has no method that accepts key material or performs a signature.
type Coordinator interface {
	EnqueueSigning(context.Context, Request) (*Task, error)
	GetSigningTask(context.Context, string) (*Task, error)
}

// Queue is used by the isolated signer process. PostgreSQL leases make signer
// restarts and multiple signer replicas safe.
type Queue interface {
	ClaimSigning(context.Context, string, time.Duration) (*Task, error)
	RenewSigning(context.Context, *Task, time.Duration) error
	CompleteSigning(context.Context, *Task, string, []Artifact) error
	FailSigning(context.Context, *Task, string) error
}

// ValidateArtifact checks an input or signed-output artifact contract.
func ValidateArtifact(a Artifact, output bool) error {
	if a.RelativePath == "" || filepath.IsAbs(a.RelativePath) {
		return fmt.Errorf("artifact relative path is required")
	}
	clean := filepath.ToSlash(filepath.Clean(a.RelativePath))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != filepath.ToSlash(a.RelativePath) {
		return fmt.Errorf("artifact path %q is not canonical and relative", a.RelativePath)
	}
	if !strings.HasSuffix(clean, ".gpkg.tar") {
		return fmt.Errorf("isolated signer accepts only .gpkg.tar artifacts")
	}
	if !digestPattern.MatchString(a.InputDigest) || a.InputSize < 0 {
		return fmt.Errorf("artifact %q has an invalid input digest or size", clean)
	}
	if output && (!digestPattern.MatchString(a.OutputDigest) || a.OutputSize < 0) {
		return fmt.Errorf("artifact %q has an invalid output digest or size", clean)
	}
	return nil
}

// ValidateRequest checks a digest-bound signing request.
func ValidateRequest(req Request) error {
	if req.JobID == "" || req.AttemptID == "" || req.AttemptFence <= 0 || req.LeaseOwner == "" {
		return fmt.Errorf("signing request requires job, attempt, fence, and lease owner")
	}
	if !tokenPattern.MatchString(req.SourceToken) {
		return fmt.Errorf("signing request source token is invalid")
	}
	if req.Architecture == "" || req.MaxOutputBytes <= 0 ||
		len(req.Artifacts) == 0 || len(req.Artifacts) > 256 {
		return fmt.Errorf("signing request requires architecture and 1..256 artifacts")
	}
	seen := make(map[string]struct{}, len(req.Artifacts))
	for _, artifact := range req.Artifacts {
		if err := ValidateArtifact(artifact, false); err != nil {
			return err
		}
		if _, exists := seen[artifact.RelativePath]; exists {
			return fmt.Errorf("duplicate signing artifact %q", artifact.RelativePath)
		}
		seen[artifact.RelativePath] = struct{}{}
	}
	return nil
}

// OutputToken derives a capability token from a UUID signing task ID.
func OutputToken(taskID string) (string, error) {
	token := strings.ReplaceAll(taskID, "-", "")
	if !tokenPattern.MatchString(token) {
		return "", fmt.Errorf("signing task id does not map to a capability token")
	}
	return token, nil
}

// QuarantineBase returns the private quarantine root beside the public binhost.
func QuarantineBase(binpkgPath string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(binpkgPath)), ".portage-engine-quarantine")
}

// TokenRoot resolves and validates one capability-scoped quarantine root.
func TokenRoot(binpkgPath, token string) (string, error) {
	if !tokenPattern.MatchString(token) {
		return "", fmt.Errorf("invalid quarantine token")
	}
	return filepath.Join(QuarantineBase(binpkgPath), token), nil
}
