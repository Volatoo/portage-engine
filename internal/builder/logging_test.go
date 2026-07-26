package builder

import (
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/pkg/config"
)

func TestAppendJobLogAddsTimestampAndRedactsCredentials(t *testing.T) {
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0})
	mgr.jobsMu.Lock()
	mgr.jobs["job-1"] = &BuildStatus{JobID: "job-1", Status: "building"}
	mgr.jobsMu.Unlock()

	mgr.appendJobLog("job-1", "[build] Authorization: Bearer top-secret password=hunter2 https://user:pass@example.test/verify-binhost/0123456789abcdef0123456789abcdef/object?token=query-secret")

	mgr.jobsMu.RLock()
	logged := mgr.jobs["job-1"].Log
	mgr.jobsMu.RUnlock()

	if strings.Contains(logged, "top-secret") || strings.Contains(logged, "hunter2") ||
		strings.Contains(logged, "user:pass") || strings.Contains(logged, "query-secret") ||
		strings.Contains(logged, "0123456789abcdef") {
		t.Fatalf("job log retained a credential: %q", logged)
	}
	if got := strings.Count(logged, "<redacted>"); got != 5 {
		t.Fatalf("redaction count = %d, want 5; log=%q", got, logged)
	}
	fields := strings.Fields(logged)
	if len(fields) == 0 {
		t.Fatal("job log is empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, fields[0]); err != nil {
		t.Fatalf("log does not begin with an RFC3339 timestamp: %q", logged)
	}
	if !strings.Contains(logged, "[build]") {
		t.Fatalf("stage marker was lost: %q", logged)
	}
}

func TestAppendJobLogTimestampsEveryMultilineEntry(t *testing.T) {
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0})
	mgr.jobsMu.Lock()
	mgr.jobs["job-2"] = &BuildStatus{JobID: "job-2", Status: "verifying"}
	mgr.jobsMu.Unlock()

	mgr.appendJobLog("job-2", "[verify] first\n[verify] second")

	mgr.jobsMu.RLock()
	lines := strings.Split(strings.TrimSpace(mgr.jobs["job-2"].Log), "\n")
	mgr.jobsMu.RUnlock()
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), lines)
	}
	for _, line := range lines {
		if _, err := time.Parse(time.RFC3339Nano, strings.Fields(line)[0]); err != nil {
			t.Fatalf("line is not timestamped: %q", line)
		}
	}
}

func TestUpdateStatusRedactsErrorBeforeAPIRead(t *testing.T) {
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0})
	mgr.jobsMu.Lock()
	mgr.jobs["job-3"] = &BuildStatus{JobID: "job-3", Status: "building"}
	mgr.jobsMu.Unlock()

	mgr.updateStatus("job-3", "failed", "", "upload failed: password=do-not-expose")
	status, err := mgr.GetStatus("job-3")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status.Error, "do-not-expose") || !strings.Contains(status.Error, "<redacted>") {
		t.Fatalf("status error was not redacted: %q", status.Error)
	}
}

func TestAppendRemoteBuildLogUsesLogSurfaceWithoutDuplicates(t *testing.T) {
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0})
	defer mgr.Shutdown()

	mgr.jobsMu.Lock()
	mgr.jobs["remote-log-test"] = &BuildStatus{
		JobID:     "remote-log-test",
		Status:    "building",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	mgr.jobsMu.Unlock()

	previous := ""
	first := "compile one\npassword=remote-secret\n"
	mgr.appendRemoteBuildLog("remote-log-test", first, &previous)
	mgr.appendRemoteBuildLog("remote-log-test", first, &previous)
	mgr.appendRemoteBuildLog("remote-log-test", first+"compile two\n", &previous)

	status, err := mgr.GetStatus("remote-log-test")
	if err != nil {
		t.Fatalf("get job status: %v", err)
	}
	if strings.Contains(status.Log, "remote-secret") {
		t.Fatalf("remote secret was not redacted: %s", status.Log)
	}
	if count := strings.Count(status.Log, "[build]"); count != 3 {
		t.Fatalf("expected three unique build log lines, got %d: %s", count, status.Log)
	}
	if status.Error != "" {
		t.Fatalf("build output leaked into error field: %q", status.Error)
	}
}

func TestAppendRemoteBuildLogDetectsReplacementThatIsNotShorter(t *testing.T) {
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0})
	defer mgr.Shutdown()

	mgr.jobsMu.Lock()
	mgr.jobs["remote-log-restart-test"] = &BuildStatus{JobID: "remote-log-restart-test"}
	mgr.jobsMu.Unlock()

	previous := ""
	mgr.appendRemoteBuildLog("remote-log-restart-test", "old output\n", &previous)
	mgr.appendRemoteBuildLog("remote-log-restart-test", "replacement output is longer\n", &previous)

	status, err := mgr.GetStatus("remote-log-restart-test")
	if err != nil {
		t.Fatalf("get job status: %v", err)
	}
	if !strings.Contains(status.Log, "remote log restarted or was truncated") ||
		!strings.Contains(status.Log, "replacement output is longer") {
		t.Fatalf("log replacement was not surfaced: %s", status.Log)
	}
}

func TestUpdateStatusClearsStaleErrorOnRecovery(t *testing.T) {
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0})
	defer mgr.Shutdown()

	mgr.jobsMu.Lock()
	mgr.jobs["stale-error-test"] = &BuildStatus{
		JobID:     "stale-error-test",
		Status:    "failed",
		Error:     "temporary failure",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	mgr.jobsMu.Unlock()

	mgr.updateStatus("stale-error-test", "building", "", "")

	status, err := mgr.GetStatus("stale-error-test")
	if err != nil {
		t.Fatalf("get job status: %v", err)
	}
	if status.Error != "" {
		t.Fatalf("expected stale error to be cleared, got %q", status.Error)
	}
}

func TestFormatLocalLogsDoesNotInventBuildOutput(t *testing.T) {
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0})
	defer mgr.Shutdown()

	now := time.Now()
	output := mgr.formatLocalLogs(&BuildStatus{
		JobID:     "empty-log-test",
		Status:    "queued",
		CreatedAt: now,
		UpdatedAt: now,
	})

	if strings.Contains(output, "Compiling package") || strings.Contains(output, "Running configure") {
		t.Fatalf("formatter invented build activity: %s", output)
	}
	if !strings.Contains(output, "(no build output recorded)") {
		t.Fatalf("expected explicit empty-output marker, got: %s", output)
	}
}
