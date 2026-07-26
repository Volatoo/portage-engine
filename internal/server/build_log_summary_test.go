package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestSummarizeBuildLogStages(t *testing.T) {
	logs := "2026-07-26T01:00:00Z [queued] accepted\n" +
		"2026-07-26T01:01:00Z [provision] creating VM\n" +
		"2026-07-26T01:02:00Z [provision] VM ready\n" +
		"2026-07-26T01:03:00Z [build] emerge started\n"
	stages := summarizeBuildLogStages(logs)
	if len(stages) != len(buildLogStages) {
		t.Fatalf("got %d stages, want %d", len(stages), len(buildLogStages))
	}
	byID := make(map[string]buildLogStageSummary, len(stages))
	for _, stage := range stages {
		byID[stage.ID] = stage
	}
	provision := byID["provision"]
	if provision.LineCount != 2 || provision.StartedAt == nil || provision.UpdatedAt == nil {
		t.Fatalf("unexpected provision summary: %+v", provision)
	}
	if got := provision.UpdatedAt.Sub(*provision.StartedAt); got != time.Minute {
		t.Fatalf("provision duration = %s, want 1m", got)
	}
	if byID["cleanup"].LineCount != 0 {
		t.Fatalf("empty cleanup stage gained lines: %+v", byID["cleanup"])
	}
}

func TestTruncateLogSummaryUsesRunes(t *testing.T) {
	if got := truncateLogSummary("桌面验证完成", 4); got != "桌面验证…" {
		t.Fatalf("truncateLogSummary = %q", got)
	}
}

func TestBuildLogsAPIIncludesStructuredStageDetails(t *testing.T) {
	s := New(&config.ServerConfig{BinpkgPath: t.TempDir(), MaxWorkers: 0})
	jobID, err := s.builder.SubmitBuild(&builder.BuildRequest{
		PackageName: "app-misc/jq",
		Version:     "1.8.2",
		Arch:        "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.handleBuildLogs(rec, httptest.NewRequest(http.MethodGet, "/api/v1/builds/logs?job_id="+jobID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Bytes       int                    `json:"bytes"`
		GeneratedAt time.Time              `json:"generated_at"`
		Truncated   bool                   `json:"truncated"`
		Stages      []buildLogStageSummary `json:"stages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Bytes == 0 || response.GeneratedAt.IsZero() || response.Truncated {
		t.Fatalf("unexpected metadata: %+v", response)
	}
	if len(response.Stages) != len(buildLogStages) || response.Stages[0].ID != "queued" || response.Stages[0].LineCount != 1 {
		t.Fatalf("unexpected stage summaries: %+v", response.Stages)
	}
}
