package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ActionRequest describes one bounded GUI-driver action.
type ActionRequest struct {
	ScenarioID      string            `json:"scenario_id"`
	ProfileID       string            `json:"profile_id,omitempty"`
	ImageID         string            `json:"image_id,omitempty"`
	ImageGeneration string            `json:"image_generation,omitempty"`
	DisplayServer   string            `json:"display_server,omitempty"`
	ApplicationKind string            `json:"application_kind,omitempty"`
	StepID          string            `json:"step_id"`
	Action          string            `json:"action"`
	Input           map[string]string `json:"input,omitempty"`
}

// Observation contains the driver result and optional screenshot evidence.
type Observation struct {
	State     string   `json:"state"`
	Message   string   `json:"message,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

// Driver executes one desktop action against an isolated test environment.
type Driver interface {
	Do(context.Context, ActionRequest) (Observation, error)
}

// StepResult records the outcome and evidence for one scenario step.
type StepResult struct {
	ID        string        `json:"id"`
	Action    string        `json:"action"`
	State     string        `json:"state"`
	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"duration"`
	Message   string        `json:"message,omitempty"`
	Artifacts []string      `json:"artifacts,omitempty"`
}

// Result records the complete deterministic scenario outcome.
type Result struct {
	SchemaVersion   int          `json:"schema_version"`
	ScenarioID      string       `json:"scenario_id"`
	ProfileID       string       `json:"profile_id"`
	ImageID         string       `json:"image_id"`
	ImageGeneration string       `json:"image_generation,omitempty"`
	DisplayServer   string       `json:"display_server,omitempty"`
	ApplicationKind string       `json:"application_kind,omitempty"`
	State           string       `json:"state"`
	StartedAt       time.Time    `json:"started_at"`
	CompletedAt     time.Time    `json:"completed_at"`
	Steps           []StepResult `json:"steps"`
}

// Run executes a validated scenario through the supplied driver.
func Run(ctx context.Context, scenario *Scenario, driver Driver, now func() time.Time) Result {
	started := now().UTC()
	result := Result{
		SchemaVersion: 1, ScenarioID: scenario.ID, ProfileID: scenario.ProfileID, ImageID: scenario.ImageID,
		ImageGeneration: scenario.ImageGeneration, DisplayServer: scenario.DisplayServer, ApplicationKind: scenario.ApplicationKind,
		State: "passed", StartedAt: started,
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(scenario.TimeoutSeconds)*time.Second)
	defer cancel()
	failed := false
	for _, step := range scenario.Steps {
		// After the first functional failure, skip further input but still collect
		// declared evidence and always execute the final stop action.
		if failed && !isEvidenceOrCleanup(step.Action) {
			continue
		}
		stepStarted := now().UTC()
		timeout := step.TimeoutSeconds
		if timeout == 0 {
			timeout = 60
		}
		stepParent := runCtx
		if step.Action == "close" || step.Action == "stop" {
			// VM cleanup must retain its own bounded chance to run even when the
			// functional scenario deadline or caller context has expired.
			stepParent = context.WithoutCancel(ctx)
		}
		stepCtx, stepCancel := context.WithTimeout(stepParent, time.Duration(timeout)*time.Second)
		observation, err := driver.Do(stepCtx, ActionRequest{
			ScenarioID: scenario.ID, ProfileID: scenario.ProfileID, ImageID: scenario.ImageID,
			ImageGeneration: scenario.ImageGeneration, DisplayServer: scenario.DisplayServer, ApplicationKind: scenario.ApplicationKind,
			StepID: step.ID, Action: step.Action, Input: step.Input,
		})
		stepCancel()
		stepResult := StepResult{ID: step.ID, Action: step.Action, State: observation.State, StartedAt: stepStarted, Duration: now().Sub(stepStarted), Message: observation.Message, Artifacts: append([]string(nil), observation.Artifacts...)}
		if err != nil {
			stepResult.State = "failed"
			stepResult.Message = err.Error()
		}
		if stepResult.State != "passed" {
			result.State = "failed"
			failed = true
		}
		result.Steps = append(result.Steps, stepResult)
	}
	result.CompletedAt = now().UTC()
	return result
}

func isEvidenceOrCleanup(action string) bool {
	switch action {
	case "screenshot", "collect_accessibility", "collect_logs", "close", "stop":
		return true
	default:
		return false
	}
}

// HTTPDriver executes desktop actions through a constrained HTTP adapter.
type HTTPDriver struct {
	endpoint string
	token    string
	client   *http.Client
}

// NewHTTPDriver creates a constrained desktop adapter client.
func NewHTTPDriver(endpoint, token string, allowHTTPControlPlane bool) (*HTTPDriver, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("desktop driver endpoint must be an origin URL without path, userinfo, query, or fragment")
	}
	if u.Scheme != "https" && (!allowHTTPControlPlane || u.Scheme != "http") {
		return nil, fmt.Errorf("desktop driver endpoint must use HTTPS unless trusted-LAN HTTP is explicitly enabled")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("desktop driver token is required")
	}
	client := &http.Client{
		Timeout: 310 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &HTTPDriver{endpoint: strings.TrimRight(u.String(), "/"), token: token, client: client}, nil
}

// Do sends one bounded action to the desktop adapter.
func (d *HTTPDriver) Do(ctx context.Context, action ActionRequest) (Observation, error) {
	body, err := json.Marshal(action)
	if err != nil {
		return Observation{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint+"/v1/actions", bytes.NewReader(body))
	if err != nil {
		return Observation{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := d.client.Do(req)
	if err != nil {
		return Observation{}, fmt.Errorf("desktop driver action %q: %w", action.StepID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Observation{}, fmt.Errorf("desktop driver action %q returned HTTP %d: %s", action.StepID, resp.StatusCode, strings.TrimSpace(string(message)))
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.DisallowUnknownFields()
	var observation Observation
	if err := dec.Decode(&observation); err != nil {
		return Observation{}, fmt.Errorf("decode desktop driver response: %w", err)
	}
	if observation.State != "passed" && observation.State != "failed" {
		return Observation{}, fmt.Errorf("desktop driver returned invalid state %q", observation.State)
	}
	return observation, nil
}
