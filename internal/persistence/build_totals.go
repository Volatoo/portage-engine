package persistence

import (
	"context"
	"fmt"
	"time"
)

// buildOutcomeTotalsTTL bounds how often a scrape turns into a count over
// build_jobs. Every replica in the api role scrapes independently, so without
// a read-through window the cheapest Prometheus interval decides the load a
// 30-day job table takes.
const buildOutcomeTotalsTTL = 15 * time.Second

// BuildOutcomeTotals counts build jobs by outcome for the whole life of the
// database. It is deliberately not windowed: these feed Prometheus counters,
// and a windowed reading would fall as old jobs leave the window, which rate()
// reads as a process restart.
type BuildOutcomeTotals struct {
	// Submitted counts every row, not Succeeded+Failed. It is the denominator
	// of the success-rate panel, and a denominator summed from the two terminal
	// counts reports 100% while a queue of running work drains and never
	// accounts for a canceled job at all.
	Submitted  int64     `json:"submitted"`
	Succeeded  int64     `json:"succeeded"`
	Failed     int64     `json:"failed"`
	ObservedAt time.Time `json:"observed_at"`
}

// BuildOutcomeTotals reports submitted, succeeded, and failed build totals from
// the durable job ledger, so any role that can read PostgreSQL can publish them.
// The per-job phase path that increments the in-process counters runs only in
// the executor role, which nothing scrapes, so those counters read zero on
// every replica an operator actually points Prometheus at.
func (r *JobRepository) BuildOutcomeTotals(
	ctx context.Context,
) (BuildOutcomeTotals, error) {
	r.buildTotalsMu.Lock()
	defer r.buildTotalsMu.Unlock()
	age := time.Since(r.buildTotalsAt)
	if !r.buildTotalsAt.IsZero() && age >= 0 && age < buildOutcomeTotalsTTL {
		return r.buildTotals, nil
	}
	var observed BuildOutcomeTotals
	// legacy_visible is not filtered: retention only hides terminal jobs from
	// operator listings, and a counter that fell every time retention ran would
	// be indistinguishable from a control-plane restart.
	err := r.db.Pool().QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE state IN ('completed', 'success')),
		       count(*) FILTER (WHERE state = 'failed')
		FROM build_jobs
	`).Scan(&observed.Submitted, &observed.Succeeded, &observed.Failed)
	if err != nil {
		return BuildOutcomeTotals{},
			fmt.Errorf("read terminal build outcome totals: %w", err)
	}
	observed.ObservedAt = time.Now().UTC()
	r.buildTotals = ratchetBuildOutcomeTotals(r.buildTotals, observed)
	r.buildTotalsAt = time.Now()
	return r.buildTotals, nil
}

// ratchetBuildOutcomeTotals keeps the published totals monotonic. The counts
// themselves only grow while one primary answers, but a read served by a
// lagging replica, a restore from an older base backup, or an operator purge
// can all return fewer rows than the last scrape saw. Prometheus reads any
// decrease as a counter reset and credits the whole series again, so a lower
// reading is dropped rather than published.
func ratchetBuildOutcomeTotals(
	previous, observed BuildOutcomeTotals,
) BuildOutcomeTotals {
	if observed.Submitted < previous.Submitted {
		observed.Submitted = previous.Submitted
	}
	if observed.Succeeded < previous.Succeeded {
		observed.Succeeded = previous.Succeeded
	}
	if observed.Failed < previous.Failed {
		observed.Failed = previous.Failed
	}
	return observed
}
