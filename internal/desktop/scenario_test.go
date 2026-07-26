package desktop

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func validScenario() *Scenario {
	return &Scenario{SchemaVersion: 1, ID: "desktop/test-v1", ProfileID: "pe/amd64/systemd/desktop-v1", ImageID: "pe/amd64/desktop-g1", Resolution: "1280x720", Locale: "C.UTF-8", TimeoutSeconds: 60, Steps: []Step{
		{ID: "restore", Action: "restore", Input: map[string]string{"snapshot": "clean"}},
		{ID: "start", Action: "start"},
		{ID: "launch", Action: "launch", Input: map[string]string{"application_id": "test.desktop"}},
		{ID: "logs", Action: "collect_logs", Input: map[string]string{"scope": "application", "application_id": "test.desktop"}},
		{ID: "stop", Action: "stop"},
	}}
}

func TestScenarioValidation(t *testing.T) {
	scenario := validScenario()
	if err := scenario.Validate(); err != nil {
		t.Fatal(err)
	}
	scenario.Steps[2].Input["password"] = "must-not-pass"
	if err := scenario.Validate(); err == nil {
		t.Fatal("secret-shaped input was accepted")
	}
	scenario = validScenario()
	delete(scenario.Steps[2].Input, "application_id")
	if err := scenario.Validate(); err == nil {
		t.Fatal("required launch input was accepted as missing")
	}
}

func TestLoadScenarioRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario.json")
	data := `{"schema_version":1,"id":"desktop/test","profile_id":"pe/profile","image_id":"pe/image","resolution":"1280x720","locale":"C.UTF-8","timeout_seconds":60,"steps":[],"command":"rm"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadScenario(path); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestTrackedDesktopScenarioLoads(t *testing.T) {
	for _, name := range []string{"application-smoke.json", "image-baseline.json"} {
		if _, err := LoadScenario(filepath.Join("..", "..", "tests", "desktop", "scenarios", name)); err != nil {
			t.Errorf("tracked scenario %s is invalid: %v", name, err)
		}
	}
}

type recordingDriver struct {
	actions []string
	failAt  string
}

type timeoutDriver struct {
	stopCalled bool
}

func (d *timeoutDriver) Do(ctx context.Context, request ActionRequest) (Observation, error) {
	if request.Action == "launch" {
		<-ctx.Done()
		return Observation{}, ctx.Err()
	}
	if request.Action == "stop" {
		d.stopCalled = true
		if ctx.Err() != nil {
			return Observation{}, errors.New("cleanup inherited expired context")
		}
	}
	return Observation{State: "passed"}, nil
}

func (d *recordingDriver) Do(_ context.Context, request ActionRequest) (Observation, error) {
	d.actions = append(d.actions, request.Action)
	if request.Action == d.failAt {
		return Observation{}, errors.New("injected failure")
	}
	return Observation{State: "passed", Artifacts: []string{request.StepID + ".json"}}, nil
}

func TestRunCollectsEvidenceAndStopsAfterFailure(t *testing.T) {
	scenario := validScenario()
	scenario.Steps = append(scenario.Steps[:3], append([]Step{
		{ID: "a11y", Action: "collect_accessibility", Input: map[string]string{"name": "failure-tree"}},
	}, scenario.Steps[3:]...)...)
	driver := &recordingDriver{failAt: "launch"}
	now := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	result := Run(context.Background(), scenario, driver, func() time.Time { now = now.Add(time.Second); return now })
	if result.State != "failed" {
		t.Fatalf("state = %q", result.State)
	}
	want := []string{"restore", "start", "launch", "collect_accessibility", "collect_logs", "stop"}
	if len(driver.actions) != len(want) {
		t.Fatalf("actions = %v", driver.actions)
	}
	for i := range want {
		if driver.actions[i] != want[i] {
			t.Fatalf("actions = %v", driver.actions)
		}
	}
}

func TestScenarioRejectsInvalidLogScope(t *testing.T) {
	scenario := validScenario()
	scenario.Steps[3].Input["scope"] = "everything"
	if err := scenario.Validate(); err == nil {
		t.Fatal("unsupported log scope was accepted")
	}
	scenario = validScenario()
	scenario.Steps[3].Input["scope"] = "system"
	if err := scenario.Validate(); err == nil {
		t.Fatal("system log scope accepted an application ID")
	}
}

func TestRunStopGetsIndependentCleanupDeadline(t *testing.T) {
	scenario := validScenario()
	scenario.TimeoutSeconds = 1
	driver := &timeoutDriver{}
	result := Run(context.Background(), scenario, driver, time.Now)
	if result.State != "failed" || !driver.stopCalled {
		t.Fatalf("cleanup was not attempted after timeout: result=%+v stop=%v", result, driver.stopCalled)
	}
	last := result.Steps[len(result.Steps)-1]
	if last.Action != "stop" || last.State != "passed" {
		t.Fatalf("cleanup did not receive an independent deadline: %+v", last)
	}
}

func TestHTTPDriverRequiresExplicitTrustedLANOptIn(t *testing.T) {
	if _, err := NewHTTPDriver("http://driver.internal", "token", false); err == nil {
		t.Fatal("non-TLS driver was accepted without explicit opt-in")
	}
	if _, err := NewHTTPDriver("http://driver.internal/adapter", "token", true); err == nil {
		t.Fatal("non-origin adapter URL was accepted")
	}
	if _, err := NewHTTPDriver("http://driver.internal", "token", true); err != nil {
		t.Fatalf("explicit trusted-LAN HTTP driver was rejected: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatal("missing driver authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"passed","artifacts":["screen.png"]}`))
	}))
	defer server.Close()
	driver, err := NewHTTPDriver(server.URL, "test-token", true)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := driver.Do(context.Background(), ActionRequest{ScenarioID: "s", StepID: "screen", Action: "screenshot"})
	if err != nil || observation.State != "passed" {
		t.Fatalf("observation = %+v, err = %v", observation, err)
	}
}

func TestHTTPDriverRejectsRedirectWithoutForwardingBearerToken(t *testing.T) {
	var redirectedAuthorization string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"passed"}`))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	driver, err := NewHTTPDriver(source.URL, "test-token", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Do(context.Background(), ActionRequest{ScenarioID: "s", StepID: "screen", Action: "screenshot"}); err == nil {
		t.Fatal("desktop adapter redirect was accepted")
	}
	if redirectedAuthorization != "" {
		t.Fatal("bearer token was forwarded to a redirect target")
	}
}
