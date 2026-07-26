package imagefactory

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const maxFactoryStatusBytes int64 = 1 << 20

type FactoryEvidence struct {
	Label      string     `json:"label"`
	Digest     string     `json:"digest,omitempty"`
	Path       string     `json:"path,omitempty"`
	RecordedAt *time.Time `json:"recorded_at,omitempty"`
	SizeBytes  int64      `json:"size_bytes,omitempty"`
}

// FactoryStep is a bounded, operator-reviewed stage record for the WebUI. It
// deliberately references a digest-bound log artifact instead of embedding
// raw command output, which can be very large and may contain credentials.
type FactoryStep struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	State       string           `json:"state"`
	Summary     string           `json:"summary,omitempty"`
	StartedAt   *time.Time       `json:"started_at,omitempty"`
	CompletedAt *time.Time       `json:"completed_at,omitempty"`
	Log         *FactoryEvidence `json:"log,omitempty"`
}

type FactoryMilestone struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	State       string            `json:"state"`
	Summary     string            `json:"summary,omitempty"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	Evidence    []FactoryEvidence `json:"evidence,omitempty"`
	Steps       []FactoryStep     `json:"steps,omitempty"`
}

type FactoryBlocker struct {
	Code    string `json:"code"`
	Summary string `json:"summary"`
	Action  string `json:"action,omitempty"`
}

type FactoryDesktopE2E struct {
	State     string `json:"state"`
	Strategy  string `json:"strategy,omitempty"`
	AIPolicy  string `json:"ai_policy,omitempty"`
	Runner    string `json:"runner,omitempty"`
	Display   string `json:"display,omitempty"`
	Artifacts string `json:"artifacts,omitempty"`
}

type FactoryStatus struct {
	SchemaVersion int                `json:"schema_version"`
	UpdatedAt     time.Time          `json:"updated_at"`
	OverallState  string             `json:"overall_state"`
	Milestones    []FactoryMilestone `json:"milestones"`
	Blockers      []FactoryBlocker   `json:"blockers,omitempty"`
	DesktopE2E    FactoryDesktopE2E  `json:"desktop_e2e"`
}

func validFactoryState(value string) bool {
	switch value {
	case "not_started", "planned", "in_progress", "blocked", "passed", "failed":
		return true
	default:
		return false
	}
}

func (status *FactoryStatus) Validate() error {
	if status.SchemaVersion != 1 {
		return fmt.Errorf("unsupported image-factory status schema_version %d", status.SchemaVersion)
	}
	if status.UpdatedAt.IsZero() || !validFactoryState(status.OverallState) {
		return fmt.Errorf("image-factory status requires updated_at and a valid overall_state")
	}
	if !validFactoryState(status.DesktopE2E.State) {
		return fmt.Errorf("desktop_e2e requires a valid state")
	}
	seen := make(map[string]struct{}, len(status.Milestones))
	for i := range status.Milestones {
		m := &status.Milestones[i]
		m.ID, m.Title, m.Summary = strings.TrimSpace(m.ID), strings.TrimSpace(m.Title), strings.TrimSpace(m.Summary)
		if m.ID == "" || m.Title == "" || !validFactoryState(m.State) {
			return fmt.Errorf("milestone %d requires id, title, and a valid state", i)
		}
		if _, ok := seen[m.ID]; ok {
			return fmt.Errorf("duplicate milestone id %q", m.ID)
		}
		seen[m.ID] = struct{}{}
		if len(m.Evidence) > 64 || len(m.Steps) > 64 {
			return fmt.Errorf("milestone %q exceeds the 64 evidence/step limit", m.ID)
		}
		for j := range m.Evidence {
			if err := validateFactoryEvidence(&m.Evidence[j], fmt.Sprintf("milestone %q evidence %d", m.ID, j)); err != nil {
				return err
			}
		}
		stepIDs := make(map[string]struct{}, len(m.Steps))
		for j := range m.Steps {
			step := &m.Steps[j]
			step.ID, step.Title, step.Summary = strings.TrimSpace(step.ID), strings.TrimSpace(step.Title), strings.TrimSpace(step.Summary)
			if step.ID == "" || step.Title == "" || !validFactoryState(step.State) {
				return fmt.Errorf("milestone %q step %d requires id, title, and a valid state", m.ID, j)
			}
			if _, ok := stepIDs[step.ID]; ok {
				return fmt.Errorf("milestone %q has duplicate step id %q", m.ID, step.ID)
			}
			stepIDs[step.ID] = struct{}{}
			if step.StartedAt != nil && step.CompletedAt != nil && step.CompletedAt.Before(*step.StartedAt) {
				return fmt.Errorf("milestone %q step %q completed_at precedes started_at", m.ID, step.ID)
			}
			if step.Log != nil {
				if err := validateFactoryEvidence(step.Log, fmt.Sprintf("milestone %q step %q log", m.ID, step.ID)); err != nil {
					return err
				}
				if step.Log.Path == "" || step.Log.Digest == "" {
					return fmt.Errorf("milestone %q step %q log requires path and digest", m.ID, step.ID)
				}
			}
		}
	}
	for i, blocker := range status.Blockers {
		if strings.TrimSpace(blocker.Code) == "" || strings.TrimSpace(blocker.Summary) == "" {
			return fmt.Errorf("blocker %d requires code and summary", i)
		}
	}
	return nil
}

func validateFactoryEvidence(evidence *FactoryEvidence, field string) error {
	evidence.Label = strings.TrimSpace(evidence.Label)
	evidence.Digest = strings.TrimSpace(evidence.Digest)
	evidence.Path = strings.TrimSpace(evidence.Path)
	if evidence.Label == "" {
		return fmt.Errorf("%s requires label", field)
	}
	if len(evidence.Label) > 160 || len(evidence.Path) > 1024 || len(evidence.Digest) > 160 {
		return fmt.Errorf("%s exceeds display limits", field)
	}
	if evidence.SizeBytes < 0 {
		return fmt.Errorf("%s has negative size_bytes", field)
	}
	if evidence.Digest != "" {
		const prefix = "sha256:"
		if !strings.HasPrefix(evidence.Digest, prefix) || len(evidence.Digest) != len(prefix)+64 {
			return fmt.Errorf("%s digest must be sha256:<64 lowercase hex>", field)
		}
		raw := strings.TrimPrefix(evidence.Digest, prefix)
		if raw != strings.ToLower(raw) {
			return fmt.Errorf("%s digest must be sha256:<64 lowercase hex>", field)
		}
		if _, err := hex.DecodeString(raw); err != nil {
			return fmt.Errorf("%s digest must be sha256:<64 lowercase hex>", field)
		}
	}
	for name, value := range map[string]string{"label": evidence.Label, "digest": evidence.Digest, "path": evidence.Path} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s %s contains a control character", field, name)
		}
	}
	return nil
}

func LoadFactoryStatus(path string) (*FactoryStatus, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-selected status input.
	if err != nil {
		return nil, fmt.Errorf("open image-factory status: %w", err)
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(io.LimitReader(f, maxFactoryStatusBytes+1))
	dec.DisallowUnknownFields()
	var status FactoryStatus
	if err := dec.Decode(&status); err != nil {
		return nil, fmt.Errorf("decode image-factory status: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode image-factory status: multiple JSON values")
		}
		return nil, fmt.Errorf("decode image-factory status trailer: %w", err)
	}
	if err := status.Validate(); err != nil {
		return nil, err
	}
	return &status, nil
}
