package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
)

const fairnessVRuntimeScale = 1000

type fairProjectSelection struct {
	ProjectID           string
	PriorityWeight      int
	StarvationThreshold int
	VRuntimeBefore      int64
	OldestAt            time.Time
	Starved             bool
}

type fairDispatchRecord struct {
	VRuntimeAfter  int64
	ItemWaitSecond int64
}

func ensureProjectFairnessRows(ctx context.Context, q Querier) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO project_scheduler_fairness (
		  project_id, admission_vruntime, phase_vruntime
		)
		SELECT
		  policy.project_id,
		  baseline.admission_vruntime,
		  baseline.phase_vruntime
		FROM project_policies policy
		CROSS JOIN (
		  SELECT
		    COALESCE(max(admission_vruntime), 0) AS admission_vruntime,
		    COALESCE(max(phase_vruntime), 0) AS phase_vruntime
		  FROM project_scheduler_fairness
		) baseline
		ON CONFLICT (project_id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("ensure project scheduler fairness rows: %w", err)
	}
	return nil
}

// selectFairAdmissionProject locks one project-level virtual-runtime row.
// Eligibility remains executor-specific, while the lock prevents two
// control-plane replicas from independently selecting the same favored
// project at the same time.
func selectFairAdmissionProject(
	ctx context.Context,
	q Querier,
	workerID uuid.UUID,
) (fairProjectSelection, bool, error) {
	var selection fairProjectSelection
	if err := ensureProjectFairnessRows(ctx, q); err != nil {
		return selection, false, err
	}
	err := q.QueryRow(ctx, `
		WITH eligible AS (
			SELECT j.project_id, min(j.created_at) AS oldest_at
			FROM build_jobs j
			JOIN project_policies pp ON pp.project_id = j.project_id
			JOIN workers executor ON executor.id = $1
			WHERE j.state = 'queued'
			  AND j.cancel_requested_at IS NULL
			  AND j.legacy_visible = true
			  AND j.next_attempt_at <= clock_timestamp()
			  AND pp.suspended = false
			  AND (
			    pp.abuse_suspended_until IS NULL
			    OR pp.abuse_suspended_until <= clock_timestamp()
			  )
			  AND (
			    SELECT count(*)
			    FROM build_jobs active
			    WHERE active.project_id = j.project_id
			      AND active.legacy_visible = true
			      AND active.state IN (
			        'claimed', 'provisioning', 'forwarding', 'deploying',
			        'building', 'collecting', 'verifying', 'signing',
			        'publishing'
			      )
			  ) < pp.max_active_jobs
			  AND executor.desired_state = 'active'
			  AND executor.last_seen_at >
			      clock_timestamp() - interval '45 seconds'
			  AND executor.executor_protocol >= j.minimum_executor_protocol
			  AND NOT EXISTS (
			    SELECT 1
			    FROM jsonb_array_elements_text(
			      j.required_capabilities
			    ) AS required(label)
			    WHERE NOT (
			      COALESCE(executor.capabilities -> 'labels', '[]'::jsonb)
			      ? required.label
			    )
			  )
			GROUP BY j.project_id
		)
		SELECT
		  fairness.project_id::text,
		  policy.priority_weight,
		  policy.starvation_threshold_seconds,
		  fairness.admission_vruntime,
		  eligible.oldest_at,
		  eligible.oldest_at <=
		    clock_timestamp()
		      - make_interval(secs => policy.starvation_threshold_seconds)
		FROM project_scheduler_fairness fairness
		JOIN project_policies policy
		  ON policy.project_id = fairness.project_id
		JOIN eligible ON eligible.project_id = fairness.project_id
		ORDER BY
		  CASE WHEN eligible.oldest_at <=
		    clock_timestamp()
		      - make_interval(secs => policy.starvation_threshold_seconds)
		    THEN 0 ELSE 1 END,
		  CASE WHEN eligible.oldest_at <=
		    clock_timestamp()
		      - make_interval(secs => policy.starvation_threshold_seconds)
		    THEN eligible.oldest_at END,
		  fairness.admission_vruntime,
		  eligible.oldest_at,
		  fairness.project_id
		LIMIT 1
		FOR UPDATE OF fairness SKIP LOCKED
	`, workerID).Scan(
		&selection.ProjectID, &selection.PriorityWeight,
		&selection.StarvationThreshold, &selection.VRuntimeBefore,
		&selection.OldestAt, &selection.Starved,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return selection, false, nil
	}
	if err != nil {
		return selection, false,
			fmt.Errorf("select fair admission project: %w", err)
	}
	return selection, true, nil
}

func selectFairPhaseProject(
	ctx context.Context,
	q Querier,
	workerID uuid.UUID,
	phases []string,
) (fairProjectSelection, bool, error) {
	var selection fairProjectSelection
	if err := ensureProjectFairnessRows(ctx, q); err != nil {
		return selection, false, err
	}
	err := q.QueryRow(ctx, `
		WITH eligible AS (
			SELECT
			  w.project_id,
			  min(COALESCE(w.ready_since, w.available_at)) AS oldest_at,
			  bool_or(reservation.phase = w.phase) AS has_replay
			FROM phase_work_items w
			JOIN build_attempts attempt ON attempt.id = w.attempt_id
			JOIN build_jobs job ON job.id = w.job_id
			JOIN project_policies policy ON policy.project_id = w.project_id
			JOIN project_resource_reservations reservation
			  ON reservation.attempt_id = w.attempt_id
			 AND reservation.state = 'active'
			JOIN project_attempt_usage usage
			  ON usage.attempt_id = w.attempt_id
			 AND usage.state = 'active'
			JOIN worker_leases lease ON lease.attempt_id = w.attempt_id
			JOIN workers executor ON executor.id = $2
			WHERE w.state = 'ready'
			  AND w.execution_mode = 'active'
			  AND w.available_at <= clock_timestamp()
			  AND (
			    COALESCE(cardinality($1::text[]), 0) = 0
			    OR w.phase = ANY($1::text[])
			  )
			  AND executor.desired_state = 'active'
			  AND executor.last_seen_at >
			      clock_timestamp() - interval '45 seconds'
			  AND executor.executor_protocol >= job.minimum_executor_protocol
			  AND NOT EXISTS (
			    SELECT 1
			    FROM jsonb_array_elements_text(
			      w.required_capabilities
			    ) AS required(label)
			    WHERE NOT (
			      COALESCE(executor.capabilities -> 'labels', '[]'::jsonb)
			      ? required.label
			    )
			  )
			  AND job.state NOT IN (
			    'completed', 'success', 'failed', 'canceled'
			  )
			  AND attempt.state NOT IN (
			    'completed', 'success', 'failed', 'canceled'
			  )
			  AND lease.fence_token = attempt.fence_token
			  AND lease.expires_at > clock_timestamp()
			  AND usage.metering_started_at
			      + make_interval(
			          secs => usage.max_runtime_seconds::double precision
			        ) > clock_timestamp()
			  AND (
			    reservation.phase = w.phase
			    OR (
			      SELECT count(*)
			      FROM project_resource_reservations used
			      WHERE used.project_id = w.project_id
			        AND used.state = 'active'
			        AND used.phase = w.phase
			        AND used.attempt_id <> w.attempt_id
			    ) < CASE w.phase
			      WHEN 'provision' THEN policy.max_provision_attempts
			      WHEN 'build' THEN policy.max_build_attempts
			      WHEN 'verify' THEN policy.max_verify_attempts
			      WHEN 'publish' THEN policy.max_publish_attempts
			    END
			  )
			GROUP BY w.project_id
		)
		SELECT
		  fairness.project_id::text,
		  policy.priority_weight,
		  policy.starvation_threshold_seconds,
		  fairness.phase_vruntime,
		  eligible.oldest_at,
		  eligible.oldest_at <=
		    clock_timestamp()
		      - make_interval(secs => policy.starvation_threshold_seconds)
		FROM project_scheduler_fairness fairness
		JOIN project_policies policy
		  ON policy.project_id = fairness.project_id
		JOIN eligible ON eligible.project_id = fairness.project_id
		ORDER BY
		  eligible.has_replay DESC,
		  CASE WHEN eligible.oldest_at <=
		    clock_timestamp()
		      - make_interval(secs => policy.starvation_threshold_seconds)
		    THEN 0 ELSE 1 END,
		  CASE WHEN eligible.oldest_at <=
		    clock_timestamp()
		      - make_interval(secs => policy.starvation_threshold_seconds)
		    THEN eligible.oldest_at END,
		  fairness.phase_vruntime,
		  eligible.oldest_at,
		  fairness.project_id
		LIMIT 1
		FOR UPDATE OF fairness SKIP LOCKED
	`, phases, workerID).Scan(
		&selection.ProjectID, &selection.PriorityWeight,
		&selection.StarvationThreshold, &selection.VRuntimeBefore,
		&selection.OldestAt, &selection.Starved,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return selection, false, nil
	}
	if err != nil {
		return selection, false,
			fmt.Errorf("select fair phase project: %w", err)
	}
	return selection, true, nil
}

func recordFairDispatch(
	ctx context.Context,
	q Querier,
	selection fairProjectSelection,
	kind string,
	queuedAt time.Time,
) (fairDispatchRecord, error) {
	var record fairDispatchRecord
	var query string
	switch kind {
	case "admission":
		query = `
			UPDATE project_scheduler_fairness fairness
			SET admission_vruntime = admission_vruntime + GREATEST(
			      1,
			      ($3::bigint + policy.priority_weight - 1)
			        / policy.priority_weight
			    ),
			    admission_dispatches = admission_dispatches + 1,
			    last_admission_wait_seconds = GREATEST(
			      0,
			      floor(extract(
			        epoch FROM clock_timestamp() - $2::timestamptz
			      ))::bigint
			    ),
			    last_admission_at = clock_timestamp(),
			    updated_at = clock_timestamp()
			FROM project_policies policy
			WHERE fairness.project_id = $1
			  AND policy.project_id = fairness.project_id
			RETURNING admission_vruntime, last_admission_wait_seconds
		`
	case "phase":
		query = `
			UPDATE project_scheduler_fairness fairness
			SET phase_vruntime = phase_vruntime + GREATEST(
			      1,
			      ($3::bigint + policy.priority_weight - 1)
			        / policy.priority_weight
			    ),
			    phase_dispatches = phase_dispatches + 1,
			    last_phase_wait_seconds = GREATEST(
			      0,
			      floor(extract(
			        epoch FROM clock_timestamp() - $2::timestamptz
			      ))::bigint
			    ),
			    last_phase_at = clock_timestamp(),
			    updated_at = clock_timestamp()
			FROM project_policies policy
			WHERE fairness.project_id = $1
			  AND policy.project_id = fairness.project_id
			RETURNING phase_vruntime, last_phase_wait_seconds
		`
	default:
		return record,
			fmt.Errorf("unsupported fairness dispatch kind %q", kind)
	}
	err := q.QueryRow(
		ctx, query, selection.ProjectID, queuedAt,
		int64(fairnessVRuntimeScale),
	).Scan(&record.VRuntimeAfter, &record.ItemWaitSecond)
	if err != nil {
		return record,
			fmt.Errorf("record %s fairness dispatch: %w", kind, err)
	}
	return record, nil
}

func readFairnessRuntimeStatus(
	ctx context.Context,
	q Querier,
) (builder.SchedulerFairnessStatus, error) {
	var status builder.SchedulerFairnessStatus
	err := q.QueryRow(ctx, `
		WITH project_wait AS (
		  SELECT
		    policy.project_id,
		    policy.starvation_threshold_seconds,
		    LEAST(job.oldest_at, work.oldest_at) AS oldest_at
		  FROM project_policies policy
		  LEFT JOIN LATERAL (
		    SELECT min(created_at) AS oldest_at
		    FROM build_jobs
		    WHERE project_id = policy.project_id
		      AND state = 'queued' AND legacy_visible = true
		  ) job ON true
		  LEFT JOIN LATERAL (
		    SELECT min(COALESCE(ready_since, available_at)) AS oldest_at
		    FROM phase_work_items
		    WHERE project_id = policy.project_id
		      AND execution_mode = 'active' AND state = 'ready'
		  ) work ON true
		  WHERE policy.suspended = false
		    AND (
		      policy.abuse_suspended_until IS NULL
		      OR policy.abuse_suspended_until <= clock_timestamp()
		    )
		)
		SELECT
		  count(*) FILTER (WHERE wait.oldest_at IS NOT NULL),
		  count(*) FILTER (
		    WHERE wait.oldest_at <=
		      clock_timestamp()
		        - make_interval(secs => wait.starvation_threshold_seconds)
		  ),
		  COALESCE(sum(fairness.admission_dispatches), 0),
		  COALESCE(sum(fairness.phase_dispatches), 0),
		  GREATEST(
		    COALESCE(max(fairness.last_admission_wait_seconds), 0),
		    COALESCE(max(fairness.last_phase_wait_seconds), 0)
		  )
		FROM project_scheduler_fairness fairness
		JOIN project_wait wait ON wait.project_id = fairness.project_id
	`).Scan(
		&status.EligibleProjects, &status.StarvedProjects,
		&status.AdmissionDispatches, &status.PhaseDispatches,
		&status.MaxQueueWaitSeconds,
	)
	if err != nil {
		return status, fmt.Errorf("read scheduler fairness status: %w", err)
	}
	status.Enabled = true
	return status, nil
}
