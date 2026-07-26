package imagefactory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFactoryStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	data := `{"schema_version":1,"updated_at":"2026-07-23T00:00:00Z","overall_state":"blocked","milestones":[{"id":"IMG-4C","title":"Candidate build","state":"blocked"}],"blockers":[{"code":"OFFLINE_CLOSURE","summary":"Closure required"}],"desktop_e2e":{"state":"planned"}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := LoadFactoryStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if status.OverallState != "blocked" || status.Milestones[0].ID != "IMG-4C" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestLoadFactoryStatusRejectsDuplicateMilestone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	data := `{"schema_version":1,"updated_at":"2026-07-23T00:00:00Z","overall_state":"failed","milestones":[{"id":"IMG-1","title":"one","state":"passed"},{"id":"IMG-1","title":"again","state":"failed"}],"desktop_e2e":{"state":"planned"}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFactoryStatus(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate milestone") {
		t.Fatalf("expected duplicate milestone error, got %v", err)
	}
}

func TestLoadFactoryStatusAcceptsStructuredStepLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	data := `{
		"schema_version":1,
		"updated_at":"2026-07-26T02:26:11Z",
		"overall_state":"passed",
		"milestones":[{
			"id":"IMG-1",
			"title":"Offline image",
			"state":"passed",
			"completed_at":"2026-07-26T02:20:00Z",
			"steps":[{
				"id":"packer",
				"title":"Packer build",
				"state":"passed",
				"started_at":"2026-07-26T01:00:00Z",
				"completed_at":"2026-07-26T02:00:00Z",
				"summary":"Template 145 created",
				"log":{"label":"Packer log","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","path":"evidence/packer.log","size_bytes":2048}
			}]
		}],
		"desktop_e2e":{"state":"passed"}
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := LoadFactoryStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	step := status.Milestones[0].Steps[0]
	if step.ID != "packer" || step.Log == nil || step.Log.SizeBytes != 2048 {
		t.Fatalf("unexpected step: %+v", step)
	}
}

func TestLoadFactoryStatusRejectsInvalidStructuredStepLog(t *testing.T) {
	tests := []struct {
		name string
		step string
		want string
	}{
		{
			name: "duplicate step",
			step: `{"id":"gate","title":"one","state":"passed"},{"id":"gate","title":"two","state":"passed"}`,
			want: "duplicate step id",
		},
		{
			name: "backwards time",
			step: `{"id":"gate","title":"gate","state":"failed","started_at":"2026-07-26T02:00:00Z","completed_at":"2026-07-26T01:00:00Z"}`,
			want: "precedes started_at",
		},
		{
			name: "unbound log",
			step: `{"id":"gate","title":"gate","state":"passed","log":{"label":"raw","path":"gate.log"}}`,
			want: "requires path and digest",
		},
		{
			name: "invalid digest",
			step: `{"id":"gate","title":"gate","state":"passed","log":{"label":"raw","path":"gate.log","digest":"sha256:not-a-digest"}}`,
			want: "digest must be",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "status.json")
			data := `{"schema_version":1,"updated_at":"2026-07-26T02:26:11Z","overall_state":"failed","milestones":[{"id":"IMG-1","title":"image","state":"failed","steps":[` + tc.step + `]}],"desktop_e2e":{"state":"planned"}}`
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFactoryStatus(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}
