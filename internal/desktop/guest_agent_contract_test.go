package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("desktop guest agent is missing %q", required)
		}
	}
	if strings.Contains(contents, `"/usr/bin/systemd-run", "--user", "--collect"`) {
		t.Fatal("desktop launcher must not collect the transient unit before its spawned GUI process exits")
	}
}
