package persistence

import (
	"context"
	"testing"
	"time"
)

// A scrape that reads fewer terminal jobs than the previous one must not
// publish the smaller number: Prometheus treats a counter going down as a
// process restart and credits the entire series again, so one read from a
// lagging replica would fabricate a second lifetime of builds.
func TestBuildOutcomeTotalsNeverGoBackwards(t *testing.T) {
	observedAt := time.Date(2026, time.August, 1, 8, 0, 0, 0, time.UTC)
	previous := BuildOutcomeTotals{
		Submitted: 604, Succeeded: 412, Failed: 37,
		ObservedAt: observedAt.Add(-buildOutcomeTotalsTTL),
	}
	behind := ratchetBuildOutcomeTotals(previous, BuildOutcomeTotals{
		Submitted: 12, Succeeded: 9, Failed: 1, ObservedAt: observedAt,
	})
	if behind.Submitted != 604 || behind.Succeeded != 412 || behind.Failed != 37 {
		t.Fatalf("a lower reading was published: %+v", behind)
	}
	if !behind.ObservedAt.Equal(observedAt) {
		t.Fatalf("held totals kept a stale observation time: %+v", behind)
	}
	// Only the field that went backwards is held: a run of failures while no
	// build succeeds must still move the failure counter.
	mixed := ratchetBuildOutcomeTotals(previous, BuildOutcomeTotals{
		Submitted: 620, Succeeded: 400, Failed: 41, ObservedAt: observedAt,
	})
	if mixed.Submitted != 620 || mixed.Succeeded != 412 || mixed.Failed != 41 {
		t.Fatalf("mixed reading=%+v", mixed)
	}
	ahead := ratchetBuildOutcomeTotals(previous, BuildOutcomeTotals{
		Submitted: 700, Succeeded: 500, Failed: 40, ObservedAt: observedAt,
	})
	if ahead.Submitted != 700 || ahead.Succeeded != 500 || ahead.Failed != 40 {
		t.Fatalf("a higher reading was held back: %+v", ahead)
	}
}

// A scrape inside the window must cost a map lookup rather than a count over
// build_jobs: every api replica scrapes on its own schedule, so without the
// read-through window the cheapest Prometheus interval anyone configures decides
// the load a 30-day job table takes. The repository is built on a nil database
// here precisely so a read that reaches SQL cannot quietly pass.
func TestBuildOutcomeTotalsInsideTTLServeFromCache(t *testing.T) {
	repo := &JobRepository{
		buildTotals: BuildOutcomeTotals{
			Submitted: 20, Succeeded: 9, Failed: 3,
		},
		buildTotalsAt: time.Now(),
	}
	totals, err := repo.BuildOutcomeTotals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if totals.Submitted != 20 || totals.Succeeded != 9 || totals.Failed != 3 {
		t.Fatalf("cached totals=%+v", totals)
	}
}
