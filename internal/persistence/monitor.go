package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
)

const (
	monitorRetentionDays  = 30
	monitorMinimumSamples = 5
	monitorSLOTarget      = 95.0
	monitorCacheTTL       = 30 * time.Second
)

// monitorLagBoundSeconds is the upper bound of the LagSeconds reading, not a
// threshold anything alerts or badges on. LagSeconds reports the age of the
// snapshot being served, and targetHistoryStatus reloads the moment a snapshot
// reaches monitorCacheTTL, so every reading a reader can ever see is strictly
// below this number by construction and comparing the two is always false.
// It is published so a reader can scale a lag reading against the refresh
// contract that produced it — 3s of 30s means something, 3s alone does not.
const monitorLagBoundSeconds = int64(monitorCacheTTL / time.Second)

func monitorTargetID(targetKey string) string {
	digest := sha256.Sum256([]byte(targetKey))
	return "target-" + hex.EncodeToString(digest[:8])
}

// targetHistoryStatus bounds the analytical projection to one query per
// control-plane replica every 30 seconds. Prometheus and dashboard polling
// therefore cannot turn the 30-day read model into a per-scrape database scan.
//
// A hit answers entirely from memory. It used to re-read the source watermark
// on every hit so it could say whether a terminal event had landed since the
// snapshot, and that read is a max() over every visible terminal build_jobs row
// with a correlated max(build_attempts.finished_at) inside the COALESCE — no
// index answers it, and it ran under monitorMu, so every Monitor reader and
// every Prometheus scrape queued behind one sequential scan. That is the cost
// the cache exists to avoid, being paid on the path that was supposed to avoid
// it. What the read bought is one bit: current versus lagging. A cached reading
// is `age` old either way, so the bit is now inferred rather than bought.
func (r *JobRepository) targetHistoryStatus(
	ctx context.Context,
) (builder.TargetHistoryStatus, error) {
	r.monitorMu.Lock()
	defer r.monitorMu.Unlock()
	age := time.Since(r.monitorAt)
	if !r.monitorAt.IsZero() && age >= 0 && age < monitorCacheTTL {
		status := r.monitor
		status.Projection = agedMonitorProjection(r.monitor.Projection, age)
		return status, nil
	}
	var status builder.TargetHistoryStatus
	err := r.db.WithTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	}, func(q Querier) error {
		var err error
		status, err = loadTargetHistory(ctx, q)
		if err != nil {
			return err
		}
		status.Projection, err = loadMonitorProjection(ctx, q)
		if err == nil {
			status.GeneratedAt = status.Projection.ObservedAt
		}
		return err
	})
	if err != nil {
		return builder.TargetHistoryStatus{}, err
	}
	r.monitor = status
	r.monitorAt = time.Now()
	return status, nil
}

func loadMonitorProjection(
	ctx context.Context,
	q Querier,
) (builder.MonitorProjectionStatus, error) {
	var now time.Time
	var source, projected *time.Time
	err := q.QueryRow(ctx, `
		SELECT clock_timestamp(),
		       (
		         -- This expression must match monitor_job_outcomes.completed_at.
		         -- Keep the base-table source independent from the view so a
		         -- projection omission still leaves the source watermark ahead.
		         SELECT max(COALESCE(
		           j.completed_at,
		           (
		             SELECT max(a.finished_at)
		             FROM build_attempts a
		             WHERE a.job_id = j.id
		           ),
		           j.updated_at
		         ))
		         FROM build_jobs j
		         WHERE j.legacy_visible = true
		           AND j.state IN ('completed', 'success', 'failed', 'canceled')
		       ),
		       (
		         SELECT max(outcomes.completed_at)
		         FROM monitor_job_outcomes outcomes
		       )
	`).Scan(&now, &source, &projected)
	if err != nil {
		return builder.MonitorProjectionStatus{},
			fmt.Errorf("read monitor projection watermarks: %w", err)
	}
	return monitorProjectionStatus(now, source, projected), nil
}

// agedMonitorProjection restates a loaded projection as the reading it has
// become: one taken `age` ago.
//
// It reports lagging without asking whether anything actually landed in that
// window, and that is the honest direction to be wrong in. A caller cannot act
// on "nothing has happened since" — the freshest thing this replica can offer
// is still `age` old — but it can act on "what you are reading is up to 12
// seconds behind". Buying the distinction cost a sequential scan on every
// scrape, and the reading it produced was already stale by the time it was
// published.
//
// An empty or invalid projection is returned untouched: neither says anything
// about a lag, and both are facts about the durable rows rather than about the
// snapshot's age.
func agedMonitorProjection(
	cached builder.MonitorProjectionStatus,
	age time.Duration,
) builder.MonitorProjectionStatus {
	seconds := int64(age.Seconds())
	if !cached.Valid || !cached.SourceWatermarkPresent || seconds <= 0 {
		return cached
	}
	cached.State = "lagging"
	cached.LagSeconds = seconds
	return cached
}

// monitorProjectionStatus judges one load of the two watermarks.
//
// It reports no lag of its own: both watermarks come from a single statement
// over the same base tables, so what it describes is zero seconds old. A load
// that still finds the source ahead is saying the view omitted rows, which is a
// fact about the projection rather than about elapsed time — the age of a
// snapshot only starts accruing afterwards, and agedMonitorProjection is what
// adds it. The distance between the two watermarks is never the measure: it is
// the age of the previous terminal event, hours on a quiet control plane, and
// not the staleness of what the caller is reading.
func monitorProjectionStatus(
	now time.Time,
	source, projected *time.Time,
) builder.MonitorProjectionStatus {
	status := builder.MonitorProjectionStatus{
		Valid:                  true,
		State:                  "empty",
		ObservedAt:             now.UTC(),
		SourceWatermarkPresent: source != nil,
		SourceWatermarkAt:      source,
		ProjectedWatermarkAt:   projected,
		AlertThresholdSeconds:  monitorLagBoundSeconds,
	}
	if source == nil {
		if projected != nil {
			status.Valid = false
			status.State = "invalid"
		}
		return status
	}
	if projected != nil && !projected.Before(*source) {
		status.State = "current"
		return status
	}
	status.State = "lagging"
	return status
}

// loadTargetHistory derives a bounded operator view from terminal jobs. It
// deliberately excludes package atoms, logs, request payloads, and subjects.
func loadTargetHistory(
	ctx context.Context,
	q Querier,
) (builder.TargetHistoryStatus, error) {
	status := builder.TargetHistoryStatus{
		RetentionDays:    monitorRetentionDays,
		SLOTargetPercent: monitorSLOTarget,
		MinimumSamples:   monitorMinimumSamples,
	}
	rows, err := q.Query(ctx, `
		WITH target_set AS (
		  SELECT target_key, project_id::text, project_name,
		         provider, execution_zone, architecture, build_mode,
		         profile_id, image_id, image_generation, resource_class,
		         count(*) AS samples_30d
		  FROM monitor_job_outcomes
		  WHERE completed_at >
		        clock_timestamp() - interval '30 days'
		  GROUP BY target_key, project_id, project_name, provider,
		           execution_zone, architecture, build_mode, profile_id,
		           image_id, image_generation, resource_class
		  ORDER BY samples_30d DESC, target_key
		  LIMIT 50
		),
		windows(name, hours, ordering) AS (
		  VALUES ('24h', 24, 1), ('7d', 168, 2), ('30d', 720, 3)
		)
		SELECT targets.target_key, targets.project_id, targets.project_name,
		       targets.provider, targets.execution_zone, targets.architecture,
		       targets.build_mode, targets.profile_id, targets.image_id,
		       targets.image_generation, targets.resource_class,
		       windows.name, windows.hours,
		       count(outcomes.job_id)::integer,
		       count(*) FILTER (
		         WHERE outcomes.outcome = 'success'
		       )::integer,
		       count(*) FILTER (
		         WHERE outcomes.outcome = 'failure'
		       )::integer,
		       count(*) FILTER (
		         WHERE outcomes.outcome = 'canceled'
		       )::integer,
		       COALESCE(
		         percentile_cont(0.5) WITHIN GROUP (
		           ORDER BY outcomes.queue_seconds
		         ) FILTER (WHERE outcomes.job_id IS NOT NULL), 0
		       )::double precision,
		       COALESCE(
		         percentile_cont(0.95) WITHIN GROUP (
		           ORDER BY outcomes.queue_seconds
		         ) FILTER (WHERE outcomes.job_id IS NOT NULL), 0
		       )::double precision,
		       COALESCE(
		         percentile_cont(0.5) WITHIN GROUP (
		           ORDER BY outcomes.run_seconds
		         ) FILTER (WHERE outcomes.job_id IS NOT NULL), 0
		       )::double precision,
		       COALESCE(
		         percentile_cont(0.95) WITHIN GROUP (
		           ORDER BY outcomes.run_seconds
		         ) FILTER (WHERE outcomes.job_id IS NOT NULL), 0
		       )::double precision,
		       COALESCE(sum(
		         outcomes.reserved_cloud_cost_microunits
		       ), 0)::bigint,
		       COALESCE(sum(
		         outcomes.charged_cloud_cost_microunits
		       ), 0)::bigint,
		       COALESCE(
		         mode() WITHIN GROUP (
		           ORDER BY NULLIF(outcomes.failure_class, '')
		         ) FILTER (WHERE outcomes.outcome = 'failure'),
		         ''
		       )
		FROM target_set targets
		CROSS JOIN windows
		LEFT JOIN monitor_job_outcomes outcomes
		  ON outcomes.target_key = targets.target_key
		 AND outcomes.completed_at >
		     clock_timestamp() - make_interval(hours => windows.hours)
		GROUP BY targets.target_key, targets.project_id,
		         targets.project_name, targets.provider,
		         targets.execution_zone, targets.architecture,
		         targets.build_mode, targets.profile_id, targets.image_id,
		         targets.image_generation, targets.resource_class,
		         targets.samples_30d, windows.name, windows.hours,
		         windows.ordering
		ORDER BY targets.samples_30d DESC, targets.target_key,
		         windows.ordering
	`)
	if err != nil {
		return status, fmt.Errorf("read target history: %w", err)
	}
	defer rows.Close()

	targetIndexes := make(map[string]int)
	for rows.Next() {
		var (
			targetKey                          string
			target                             builder.TargetReliabilityStatus
			window                             builder.TargetReliabilityWindow
			queueP50, queueP95, runP50, runP95 float64
		)
		if err := rows.Scan(
			&targetKey, &target.ProjectID, &target.ProjectName,
			&target.Provider, &target.ExecutionZone, &target.Architecture,
			&target.BuildMode, &target.ProfileID, &target.ImageID,
			&target.ImageGeneration, &target.ResourceClass,
			&window.Name, &window.Hours, &window.Samples,
			&window.Successes, &window.Failures, &window.Canceled,
			&queueP50, &queueP95, &runP50, &runP95,
			&window.ReservedCostMicrounits,
			&window.ChargedCostMicrounits,
			&window.DominantFailureClass,
		); err != nil {
			return status, fmt.Errorf("scan target history: %w", err)
		}
		denominator := window.Successes + window.Failures
		if denominator > 0 {
			window.SuccessRatePercent =
				100 * float64(window.Successes) / float64(denominator)
		}
		window.InsufficientData = denominator < monitorMinimumSamples
		window.SLOMet = !window.InsufficientData &&
			window.SuccessRatePercent >= monitorSLOTarget
		window.QueueP50Seconds = int64(math.Round(queueP50))
		window.QueueP95Seconds = int64(math.Round(queueP95))
		window.RunP50Seconds = int64(math.Round(runP50))
		window.RunP95Seconds = int64(math.Round(runP95))

		targetID := monitorTargetID(targetKey)
		index, found := targetIndexes[targetID]
		if !found {
			target.TargetID = targetID
			target.Windows = make([]builder.TargetReliabilityWindow, 0, 3)
			status.Targets = append(status.Targets, target)
			index = len(status.Targets) - 1
			targetIndexes[targetID] = index
		}
		status.Targets[index].Windows = append(
			status.Targets[index].Windows, window,
		)
	}
	if err := rows.Err(); err != nil {
		return status, fmt.Errorf("iterate target history: %w", err)
	}
	return status, nil
}
