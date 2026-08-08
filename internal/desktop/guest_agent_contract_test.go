package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGuestAgentWaitsForXFCEAndInjectsUnitEnvironment(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "image-factory", "desktop", "guest-agent.py")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	for _, required := range []string{
		`user_process_running(user, {"xfce4-session"})`,
		`user_process_running(user, {"xfwm4"})`,
		`command.extend(f"--setenv={key}={value}" for key, value in environment.items())`,
		`"--property=KillMode=process"`,
		`fail("desktop user configuration directory is not owner-writable")`,
		`"verify-signature = {'true' if signed else 'false'}\n"`,
		`environment["FEATURES"] = "binpkg-request-signature"`,
		`environment["BINPKG_GPG_VERIFY_GPG_HOME"] = str(STAGING_GNUPG)`,
		`fingerprints != [expected_fingerprint]`,
		`reviewed_fixture(fixture, digest)`,
		`close_accessible(*values)`,
		`assert_image(*values)`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("desktop guest agent is missing %q", required)
		}
	}
	if strings.Contains(contents, `"/usr/bin/systemd-run", "--user", "--collect"`) {
		t.Fatal("desktop launcher must not collect the transient unit before its spawned GUI process exits")
	}
}

// TestTheGuestAgentHonorsTheDeclaredStepBudget covers both halves of the wait
// budget: the driver has to send the step's own remaining time, and the agent
// has to use it.
//
// The agent hardcoded 60 seconds in both accessibility waits. Scenarios in
// tests/desktop/scenarios declare 90 for exactly these steps, and scenario.go
// validates up to 300, so the declared budget was unreachable — a window that
// took 70 seconds to appear failed at 60 with "accessibility selector was not
// found", which reads as a desktop that never drew it.
func TestTheGuestAgentHonorsTheDeclaredStepBudget(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(
		filepath.Join("..", "..", "image-factory", "desktop", "guest-agent.py"),
	)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	for _, required := range []string{
		"def accessibility_budget(seconds: str | None) -> float:",
		"deadline = time.monotonic() + accessibility_budget(seconds)",
		`elif action == "wait-accessible" and len(values) in {3, 4}:`,
		`elif action == "close" and len(values) in {3, 4}:`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("desktop guest agent is missing %q", required)
		}
	}
	// The literal that made the declared budget unreachable.
	if strings.Contains(contents, "deadline = time.monotonic() + 60") {
		t.Fatal("an accessibility wait is still capped at a hardcoded 60 seconds")
	}
	// The two ceilings have to be the same number or the agent rejects a budget
	// a scenario is allowed to declare.
	if !strings.Contains(contents,
		"MAX_ACCESSIBILITY_BUDGET = "+strconv.Itoa(maxStepTimeoutSeconds)) {
		t.Fatalf("the agent's wait ceiling is not the scenario per-step maximum of %d",
			maxStepTimeoutSeconds)
	}
}

// TestTheDriverSendsTheRemainingStepBudget is the other half: what the runner
// gave the step is what the guest is told it may spend.
func TestTheDriverSendsTheRemainingStepBudget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		timeout time.Duration
		want    []string
	}{
		{name: "no deadline sends nothing, agent uses its default"},
		{name: "declared 90s step", timeout: 90 * time.Second, want: []string{"89"}},
		{name: "default 60s step", timeout: 60 * time.Second, want: []string{"59"}},
		{
			name:    "a step longer than the per-step maximum is capped",
			timeout: 20 * time.Minute,
			want:    []string{strconv.Itoa(maxStepTimeoutSeconds)},
		},
		{name: "already out of time sends nothing", timeout: time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, test.timeout)
				defer cancel()
			}
			got := accessibilityBudgetArgument(ctx)
			if len(got) != len(test.want) {
				t.Fatalf("budget argument = %v, want %v", got, test.want)
			}
			for index := range got {
				if got[index] != test.want[index] {
					t.Fatalf("budget argument = %v, want %v", got, test.want)
				}
			}
		})
	}
}
