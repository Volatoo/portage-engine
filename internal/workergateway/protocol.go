// Package workergateway implements the attempt-bound reverse execution channel
// used by disposable native builders. Workers make outbound mTLS requests; the
// control plane never connects to a worker listener.
package workergateway

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	ActionBuild   = "build"
	ActionVerify  = "verify"
	ActionCollect = "collect"
)

// Identity is the complete authorization scope carried by a short-lived worker
// certificate. A certificate is useful only for one durable scheduler attempt.
type Identity struct {
	WorkerID   string `json:"worker_id"`
	JobID      string `json:"job_id"`
	AttemptID  string `json:"attempt_id"`
	FenceToken int64  `json:"fence_token"`
}

func (i Identity) Validate() error {
	if strings.TrimSpace(i.WorkerID) == "" || strings.TrimSpace(i.JobID) == "" ||
		strings.TrimSpace(i.AttemptID) == "" || i.FenceToken <= 0 {
		return fmt.Errorf("worker identity requires worker_id, job_id, attempt_id, and a positive fence_token")
	}
	for _, value := range []string{i.WorkerID, i.JobID, i.AttemptID} {
		if strings.ContainsAny(value, "/?#") {
			return fmt.Errorf("worker identity components must not contain URL delimiters")
		}
	}
	return nil
}

// URI returns the canonical URI SAN encoded into a worker certificate.
func (i Identity) URI() (*url.URL, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	return url.Parse(fmt.Sprintf(
		"spiffe://portage-engine/worker/%s/job/%s/attempt/%s/fence/%d",
		url.PathEscape(i.WorkerID), url.PathEscape(i.JobID),
		url.PathEscape(i.AttemptID), i.FenceToken,
	))
}

// ParseIdentityURI parses only the canonical Portage Engine worker URI SAN.
func ParseIdentityURI(uri *url.URL) (Identity, error) {
	if uri == nil || uri.Scheme != "spiffe" || uri.Host != "portage-engine" ||
		uri.RawQuery != "" || uri.Fragment != "" {
		return Identity{}, fmt.Errorf("invalid worker identity URI")
	}
	parts := strings.Split(strings.Trim(uri.EscapedPath(), "/"), "/")
	if len(parts) != 8 || parts[0] != "worker" || parts[2] != "job" ||
		parts[4] != "attempt" || parts[6] != "fence" {
		return Identity{}, fmt.Errorf("invalid worker identity path")
	}
	workerID, err := url.PathUnescape(parts[1])
	if err != nil {
		return Identity{}, fmt.Errorf("decode worker id: %w", err)
	}
	jobID, err := url.PathUnescape(parts[3])
	if err != nil {
		return Identity{}, fmt.Errorf("decode job id: %w", err)
	}
	attemptID, err := url.PathUnescape(parts[5])
	if err != nil {
		return Identity{}, fmt.Errorf("decode attempt id: %w", err)
	}
	fence, err := strconv.ParseInt(parts[7], 10, 64)
	if err != nil {
		return Identity{}, fmt.Errorf("decode fence token: %w", err)
	}
	identity := Identity{WorkerID: workerID, JobID: jobID, AttemptID: attemptID, FenceToken: fence}
	return identity, identity.Validate()
}

// Task is a control-plane command delivered by a worker long poll.
type Task struct {
	ID            string          `json:"id"`
	Action        string          `json:"action"`
	DeliveryFence int64           `json:"delivery_fence"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// Completion is the small JSON result of a task. Artifact bytes use the
// separate streaming upload endpoint and are never base64-encoded here.
type Completion struct {
	TaskID        string          `json:"task_id"`
	DeliveryFence int64           `json:"delivery_fence"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// CollectRequest asks the worker to stream one recorded local artifact into a
// pre-authorized, one-shot upload slot.
type CollectRequest struct {
	LocalJobID string `json:"local_job_id"`
	Relative   string `json:"relative"`
	UploadID   string `json:"upload_id"`
}

type CollectResult struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
