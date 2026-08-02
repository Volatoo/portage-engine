package persistence

import (
	"context"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/builder"
)

// ledgerRowFor builds the durable row a ledger in agreement with this status
// would hold, so a test that wants disagreement has to ask for it explicitly.
func ledgerRowFor(t *testing.T, status *builder.BuildStatus) ledgerRow {
	t.Helper()
	statusJSON, statusDigest, err := statusDocument(status)
	if err != nil {
		t.Fatalf("statusDocument: %v", err)
	}
	row := ledgerRow{
		State: status.Status, StatusDigest: statusDigest, StatusJSON: statusJSON,
	}
	if status.Request != nil {
		_, requestDigest, err := requestDocument(status.Request)
		if err != nil {
			t.Fatalf("requestDocument: %v", err)
		}
		row.RequestDigest = requestDigest
	}
	return row
}

func inMemoryJob(id, state string) *builder.BuildStatus {
	return &builder.BuildStatus{
		JobID: id, Status: state, PackageName: "app-editors/vim", Version: "9.1",
		Arch: "amd64", CreatedAt: time.Unix(1700000000, 0).UTC(),
		UpdatedAt: time.Unix(1700000060, 0).UTC(),
	}
}

// TestDiffLedgerReportsEveryShapeOfDivergence covers the three ways the
// in-memory projection and the durable rows can disagree, because until this
// compare was scheduled none of them had ever been observed on a running
// server: the report existed and nothing produced one.
func TestDiffLedgerReportsEveryShapeOfDivergence(t *testing.T) {
	agreed := inMemoryJob("11111111-1111-1111-1111-111111111111", "completed")
	drifted := inMemoryJob("22222222-2222-2222-2222-222222222222", "completed")
	absent := inMemoryJob("33333333-3333-3333-3333-333333333333", "building")
	orphan := inMemoryJob("44444444-4444-4444-4444-444444444444", "queued")

	// The drifted row still says "building" while memory has moved to
	// "completed" — the state column and the snapshot digest both disagree.
	driftedRow := ledgerRowFor(t, inMemoryJob(drifted.JobID, "building"))

	report, divergences := diffLedger(
		map[string]*builder.BuildStatus{
			agreed.JobID: agreed, drifted.JobID: drifted, absent.JobID: absent,
		},
		map[string]ledgerRow{
			agreed.JobID:  ledgerRowFor(t, agreed),
			drifted.JobID: driftedRow,
			orphan.JobID:  ledgerRowFor(t, orphan),
		},
	)

	if report.LegacyCount != 3 || report.LedgerCount != 3 {
		t.Fatalf("counted %d in memory and %d in the ledger, want 3 and 3",
			report.LegacyCount, report.LedgerCount)
	}
	if report.Missing != 1 {
		t.Errorf("missing = %d, want 1 for the job no durable row describes", report.Missing)
	}
	if report.Mismatched != 1 {
		t.Errorf("mismatched = %d, want 1 for the row still reading %q",
			report.Mismatched, driftedRow.State)
	}
	if report.Extra != 1 {
		t.Errorf("extra = %d, want 1 for the durable row memory never saw", report.Extra)
	}
	if report.Consistent {
		t.Error("a projection missing a job, disagreeing about another, and blind " +
			"to a third reported itself consistent")
	}
	if report.Repaired != 0 {
		t.Errorf("repaired = %d: the comparison must not write anything", report.Repaired)
	}
	if report.CheckedAt.IsZero() {
		t.Error("the report carries no checked-at, which is the field /monitor " +
			"renders as 'never'")
	}

	// The divergences are what lets Reconcile still repair off the same compare.
	if len(divergences) != 2 {
		t.Fatalf("divergences = %d, want the missing job and the drifted row", len(divergences))
	}
	for _, divergence := range divergences {
		switch divergence.JobID {
		case absent.JobID:
			if !divergence.Absent {
				t.Error("the job with no durable row was not reported as absent")
			}
		case drifted.JobID:
			if divergence.Absent || divergence.Row.State != "building" {
				t.Errorf("drifted divergence carries %#v, want the stale row", divergence.Row)
			}
		default:
			t.Errorf("unexpected divergence for %s", divergence.JobID)
		}
	}
}

// TestDiffLedgerAgreesWhenTheProjectionMatches keeps the check from reporting a
// divergence on every tick of a healthy server, which would pin readiness red.
func TestDiffLedgerAgreesWhenTheProjectionMatches(t *testing.T) {
	job := inMemoryJob("55555555-5555-5555-5555-555555555555", "completed")
	job.Request = &builder.BuildRequest{
		PackageName: job.PackageName, Version: job.Version, Arch: job.Arch,
	}
	rows := map[string]ledgerRow{job.JobID: ledgerRowFor(t, job)}

	report, divergences := diffLedger(map[string]*builder.BuildStatus{job.JobID: job}, rows)

	if !report.Consistent || len(divergences) != 0 {
		t.Fatalf("a projection equal to the ledger reported %#v with %d divergences",
			report, len(divergences))
	}
	// The rows belong to the caller: a reporting pass that emptied them would
	// make the next compare see every job as missing.
	if len(rows) != 1 {
		t.Errorf("the compare consumed the caller's rows: %d left", len(rows))
	}
}

// TestDiffLedgerCountsARequestRewriteAsDivergence pins the half of the compare
// that watches the immutable side of a job. A status that drifts is a stale
// projection; a request digest that drifts means the durable record of what was
// asked for has changed underneath a job that is already running.
func TestDiffLedgerCountsARequestRewriteAsDivergence(t *testing.T) {
	job := inMemoryJob("66666666-6666-6666-6666-666666666666", "building")
	job.Request = &builder.BuildRequest{
		PackageName: job.PackageName, Version: job.Version, Arch: job.Arch,
	}
	row := ledgerRowFor(t, job)
	row.RequestDigest = "0000000000000000000000000000000000000000000000000000000000000000"

	report, divergences := diffLedger(
		map[string]*builder.BuildStatus{job.JobID: job},
		map[string]ledgerRow{job.JobID: row},
	)

	if report.RequestMismatch != 1 || report.Mismatched != 1 || report.Consistent {
		t.Fatalf("a rewritten request digest reported %#v", report)
	}
	if len(divergences) != 1 || !divergences[0].Request {
		t.Fatalf("the divergence does not name the request: %#v", divergences)
	}
}

// TestInspectLedgerPublishesItsReport pins the path health reads. The report is
// only useful if it reaches JobLedgerStatus, which is what /health, /monitor and
// the operator console all render.
func TestInspectLedgerPublishesItsReport(t *testing.T) {
	// A repository with no pool stands in for a ledger that cannot be read at
	// all: the compare has to report that rather than panic a timer goroutine.
	repository := &JobRepository{}

	report := repository.InspectLedger(context.Background(), nil)

	if report.Error == "" || report.Consistent {
		t.Fatalf("an unreadable ledger compared clean: %#v", report)
	}
	published := repository.Status().LastReconcile
	if published.CheckedAt != report.CheckedAt || published.Error != report.Error {
		t.Fatalf("Status().LastReconcile = %#v, want the report just produced %#v",
			published, report)
	}
}
