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

var scenarioIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)

// Scenario is a validated sequence of deterministic desktop actions.
type Scenario struct {
	SchemaVersion  int    `json:"schema_version"`
	ID             string `json:"id"`
	ProfileID      string `json:"profile_id"`
	ImageID        string `json:"image_id"`
	Resolution     string `json:"resolution"`
	Locale         string `json:"locale"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Steps          []Step `json:"steps"`
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
	"install":               {"atom": true, "staging_digest": true},
	"launch":                {"application_id": true},
	"wait_accessible":       {"role": true, "name": true, "state": true},
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
	if s.SchemaVersion != 1 || !scenarioIDPattern.MatchString(s.ID) || !scenarioIDPattern.MatchString(s.ProfileID) || !scenarioIDPattern.MatchString(s.ImageID) {
		return fmt.Errorf("desktop scenario requires schema_version 1 and valid id/profile_id/image_id")
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
		if err := validateScenarioStep(index, &s.Steps[index], seen, lifecycle); err != nil {
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

func validateScenarioStep(index int, step *Step, seen map[string]struct{}, lifecycle map[string]int) error {
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
	if step.TimeoutSeconds < 0 || step.TimeoutSeconds > 300 {
		return fmt.Errorf("step %q timeout must be at most 300 seconds", step.ID)
	}
	if step.Action == "restore" || step.Action == "start" || step.Action == "stop" {
		lifecycle[step.Action]++
	}
	if err := validateScenarioInputs(step, allowed); err != nil {
		return err
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
