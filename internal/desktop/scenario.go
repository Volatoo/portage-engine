// Package desktop implements deterministic native-desktop verification. It is
// deliberately separate from the package builder and treats the console
// adapter as an untrusted, capability-limited driver.
package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const maxScenarioBytes int64 = 1 << 20

// maxStepTimeoutSeconds is the longest a single step may declare. It is also
// the ceiling the guest agent applies to a wait budget it is handed, so the two
// sides cannot disagree about how long a step is allowed to last.
const maxStepTimeoutSeconds = 300

var scenarioIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)

var (
	imageGenerationPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	packageAtomPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._-]*/[A-Za-z0-9][A-Za-z0-9+._-]*$`)
	applicationIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._-]{0,127}\.desktop$`)
	fixtureNamePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	signerFingerprintPattern = regexp.MustCompile(`^(?:[A-F0-9]{40}|[A-F0-9]{64})$`)
)

// Scenario is a validated sequence of deterministic desktop actions.
type Scenario struct {
	SchemaVersion   int    `json:"schema_version"`
	ID              string `json:"id"`
	ProfileID       string `json:"profile_id"`
	ImageID         string `json:"image_id"`
	ImageGeneration string `json:"image_generation,omitempty"`
	DisplayServer   string `json:"display_server,omitempty"`
	ApplicationKind string `json:"application_kind,omitempty"`
	Resolution      string `json:"resolution"`
	Locale          string `json:"locale"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	Steps           []Step `json:"steps"`
}

// Step describes one action and its expected observation.
type Step struct {
	ID             string            `json:"id"`
	Action         string            `json:"action"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Input          map[string]string `json:"input,omitempty"`
}

var actionInputs = map[string]map[string]bool{
	"restore":               {"snapshot": true},
	"start":                 {},
	"install":               {"atom": true, "staging_digest": true, "signer_fingerprint": false},
	"launch":                {"application_id": true},
	"launch_fixture":        {"application_id": true, "fixture": true, "fixture_digest": true},
	"wait_accessible":       {"role": true, "name": true, "state": true},
	"close":                 {"role": true, "name": true, "state": true},
	"key":                   {"keys": true},
	"type":                  {"text": true},
	"click":                 {"accessibility_id": false, "needle": false, "x": false, "y": false},
	"assert_needle":         {"needle": true, "threshold": true},
	"screenshot":            {"name": true},
	"collect_accessibility": {"name": true},
	"collect_logs":          {"scope": true, "application_id": false},
	"stop":                  {},
}

// LoadScenario loads a strict JSON desktop scenario.
func LoadScenario(path string) (*Scenario, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-selected scenario.
	if err != nil {
		return nil, fmt.Errorf("open desktop scenario: %w", err)
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(io.LimitReader(f, maxScenarioBytes+1))
	dec.DisallowUnknownFields()
	var scenario Scenario
	if err := dec.Decode(&scenario); err != nil {
		return nil, fmt.Errorf("decode desktop scenario: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode desktop scenario: multiple JSON values")
		}
		return nil, fmt.Errorf("decode desktop scenario trailer: %w", err)
	}
	if err := scenario.Validate(); err != nil {
		return nil, err
	}
	return &scenario, nil
}

// Validate checks scenario bounds, action types, and evidence requirements.
func (s *Scenario) Validate() error {
	if (s.SchemaVersion != 1 && s.SchemaVersion != 2) || !scenarioIDPattern.MatchString(s.ID) || !scenarioIDPattern.MatchString(s.ProfileID) || !scenarioIDPattern.MatchString(s.ImageID) {
		return fmt.Errorf("desktop scenario requires schema_version 1 or 2 and valid id/profile_id/image_id")
	}
	if s.SchemaVersion == 1 {
		if s.ImageGeneration != "" || s.DisplayServer != "" || s.ApplicationKind != "" {
			return fmt.Errorf("desktop scenario schema_version 1 must not use version 2 runtime identity fields")
		}
	} else {
		if !imageGenerationPattern.MatchString(s.ImageGeneration) || (s.DisplayServer != "x11" && s.DisplayServer != "wayland") {
			return fmt.Errorf("desktop scenario schema_version 2 requires a valid image_generation and display_server")
		}
		switch s.ApplicationKind {
		case "gtk", "qt", "electron", "webview":
		default:
			return fmt.Errorf("desktop scenario schema_version 2 requires a reviewed application_kind")
		}
	}
	if s.Resolution != "1280x720" || (s.Locale != "C.UTF-8" && s.Locale != "en_US.UTF-8") {
		return fmt.Errorf("desktop scenario requires reviewed resolution 1280x720 and locale")
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > 1800 || len(s.Steps) == 0 || len(s.Steps) > 100 {
		return fmt.Errorf("desktop scenario timeout must be 1..1800 seconds and steps 1..100")
	}
	seen := make(map[string]struct{}, len(s.Steps))
	lifecycle := map[string]int{}
	for index := range s.Steps {
		if err := validateScenarioStep(s.SchemaVersion, index, &s.Steps[index], seen, lifecycle); err != nil {
			return err
		}
	}
	if len(s.Steps) < 3 || s.Steps[0].Action != "restore" || s.Steps[1].Action != "start" || s.Steps[len(s.Steps)-1].Action != "stop" {
		return fmt.Errorf("desktop scenario must start with restore/start and end with stop")
	}
	if lifecycle["restore"] != 1 || lifecycle["start"] != 1 || lifecycle["stop"] != 1 {
		return fmt.Errorf("desktop scenario requires exactly one restore, start, and stop action")
	}
	return nil
}

// ValidateRunnable rejects syntactically valid tracked templates whose all-zero
// staging locks have not yet been materialized from a real signed candidate.
func (s *Scenario) ValidateRunnable() error {
	if err := s.Validate(); err != nil {
		return err
	}
	zeroDigest := "sha256:" + strings.Repeat("0", 64)
	zeroFingerprint := strings.Repeat("0", 40)
	for _, step := range s.Steps {
		if step.Action != "install" {
			continue
		}
		if step.Input["staging_digest"] == zeroDigest || step.Input["signer_fingerprint"] == zeroFingerprint {
			return fmt.Errorf("desktop scenario contains an unmaterialized all-zero staging lock")
		}
	}
	return nil
}

func validateScenarioStep(schemaVersion, index int, step *Step, seen map[string]struct{}, lifecycle map[string]int) error {
	if !scenarioIDPattern.MatchString(step.ID) {
		return fmt.Errorf("step %d has invalid id", index)
	}
	if _, ok := seen[step.ID]; ok {
		return fmt.Errorf("duplicate step id %q", step.ID)
	}
	seen[step.ID] = struct{}{}
	allowed, ok := actionInputs[step.Action]
	if !ok {
		return fmt.Errorf("step %q has unsupported action %q", step.ID, step.Action)
	}
	if step.TimeoutSeconds < 0 || step.TimeoutSeconds > maxStepTimeoutSeconds {
		return fmt.Errorf("step %q timeout must be at most 300 seconds", step.ID)
	}
	if step.Action == "restore" || step.Action == "start" || step.Action == "stop" {
		lifecycle[step.Action]++
	}
	if schemaVersion == 1 && (step.Action == "launch_fixture" || step.Action == "close") {
		return fmt.Errorf("step %q action %q requires desktop scenario schema_version 2", step.ID, step.Action)
	}
	if err := validateScenarioInputs(step, allowed); err != nil {
		return err
	}
	if schemaVersion == 2 && step.Action == "install" && !signerFingerprintPattern.MatchString(step.Input["signer_fingerprint"]) {
		return fmt.Errorf("step %q schema_version 2 install requires a full uppercase signer_fingerprint", step.ID)
	}
	return validateScenarioAction(step)
}

func validateScenarioInputs(step *Step, allowed map[string]bool) error {
	for key, value := range step.Input {
		if _, accepted := allowed[key]; !accepted {
			return fmt.Errorf("step %q action %q does not allow input %q", step.ID, step.Action, key)
		}
		if len(value) > 4096 || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("step %q input %q is too large or contains NUL", step.ID, key)
		}
		lower := strings.ToLower(key)
		if strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") {
			return fmt.Errorf("step %q must not carry secrets", step.ID)
		}
	}
	for key, required := range allowed {
		if required && strings.TrimSpace(step.Input[key]) == "" {
			return fmt.Errorf("step %q action %q requires input %q", step.ID, step.Action, key)
		}
	}
	return nil
}

func validateScenarioAction(step *Step) error {
	switch step.Action {
	case "install":
		if !packageAtomPattern.MatchString(step.Input["atom"]) || !sha256Pattern.MatchString(step.Input["staging_digest"]) {
			return fmt.Errorf("step %q requires a package atom and full staging SHA-256", step.ID)
		}
	case "launch":
		if !applicationIDPattern.MatchString(step.Input["application_id"]) {
			return fmt.Errorf("step %q has an invalid application_id", step.ID)
		}
	case "launch_fixture":
		if !applicationIDPattern.MatchString(step.Input["application_id"]) || !fixtureNamePattern.MatchString(step.Input["fixture"]) || !sha256Pattern.MatchString(step.Input["fixture_digest"]) {
			return fmt.Errorf("step %q requires a reviewed application_id, fixture name, and full fixture SHA-256", step.ID)
		}
	case "wait_accessible", "close":
		if strings.TrimSpace(step.Input["role"]) == "" || strings.TrimSpace(step.Input["name"]) == "" {
			return fmt.Errorf("step %q requires a non-empty accessibility role and name", step.ID)
		}
		switch step.Input["state"] {
		case "visible", "showing", "enabled", "focused":
		default:
			return fmt.Errorf("step %q has unsupported accessibility state %q", step.ID, step.Input["state"])
		}
	case "click":
		coordinates := step.Input["x"] != "" && step.Input["y"] != ""
		selectors := 0
		for _, key := range []string{"accessibility_id", "needle"} {
			if step.Input[key] != "" {
				selectors++
			}
		}
		if coordinates {
			selectors++
		}
		if selectors != 1 || (step.Input["x"] == "") != (step.Input["y"] == "") {
			return fmt.Errorf("step %q click requires exactly one selector or an x/y pair", step.ID)
		}
	case "collect_logs":
		scope := step.Input["scope"]
		if scope != "application" && scope != "desktop" && scope != "system" {
			return fmt.Errorf("step %q has unsupported log scope %q", step.ID, scope)
		}
		applicationID := strings.TrimSpace(step.Input["application_id"])
		if scope == "application" && applicationID == "" {
			return fmt.Errorf("step %q application logs require application_id", step.ID)
		}
		if scope != "application" && applicationID != "" {
			return fmt.Errorf("step %q %s logs must not include application_id", step.ID, scope)
		}
	}
	return nil
}
