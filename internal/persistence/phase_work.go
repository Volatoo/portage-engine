package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
)

// PhaseWorkClaim remains exported as an alias for callers which inspect the
// persistence package directly; the execution contract lives in builder to
// avoid a package cycle.
type PhaseWorkClaim = builder.PhaseWorkClaim

// PhaseWorkStatus is a low-cardinality operator view of the durable queue.
type PhaseWorkStatus struct {
	Shadow        int `json:"shadow"`
	Active        int `json:"active"`
	Blocked       int `json:"blocked"`
	Ready         int `json:"ready"`
	Unschedulable int `json:"unschedulable"`
	Claimed       int `json:"claimed"`
	Completed     int `json:"completed"`
	Failed        int `json:"failed"`
	Canceled      int `json:"canceled"`
}

// ActivatePhasePlan is the explicit cutover fence from the legacy
// whole-pipeline executor to independent phase claims. It is allowed only
// before the attempt has crossed its first durable phase boundary.
func (r *JobRepository) ActivatePhasePlan(
	ctx context.Context,
	status *builder.BuildStatus,
) error {
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		_, attemptID, err := validateMetadataFence(ctx, q, status, false)
		if err != nil {
			return err
		}
		var phase string
		if err := q.QueryRow(ctx, `
			SELECT phase
			FROM project_resource_reservations
			WHERE attempt_id = $1 AND state = 'active'
			FOR UPDATE
		`, attemptID).Scan(&phase); err != nil {
			return fmt.Errorf("lock phase-plan reservation: %w", err)
		}
		if phase != "claimed" || status.Status != "claimed" {
			return fmt.Errorf("phase plan can only activate at the claimed boundary")
		}
		tag, err := q.Exec(ctx, `
			UPDATE phase_work_items
			SET execution_mode = 'active',
			    ready_since = CASE
			      WHEN state = 'ready'
			      THEN COALESCE(ready_since, clock_timestamp())
			      ELSE ready_since
			    END,
			    updated_at = clock_timestamp()
			WHERE attempt_id = $1 AND execution_mode = 'shadow'
			  AND (
				(phase = 'provision' AND state = 'ready')
				OR (phase <> 'provision' AND state = 'blocked')
			  )
		`, attemptID)
		if err != nil {
			return fmt.Errorf("activate phase work plan: %w", err)
		}
		if tag.RowsAffected() != 4 {
			return fmt.Errorf("phase work plan is incomplete or already activated")
		}
		return nil
	})
	r.recordWrite(err)
	return err
}

// ClaimPhaseWork claims one ready phase using SKIP LOCKED. Project phase
// capacity is checked under the same policy-row lock as the legacy checkpoint
// path. A full phase defers the work item without returning an error so another
// project can make progress.
func (r *JobRepository) ClaimPhaseWork(
	ctx context.Context,
	owner string,
	lease time.Duration,
	capabilities []string,
	phases ...string,
) (*PhaseWorkClaim, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > 256 {
		return nil, fmt.Errorf("phase work owner is required")
	}
	if lease <= 0 {
		return nil, fmt.Errorf("phase work lease must be positive")
	}
	capabilities, err := builder.NormalizeExecutorCapabilities(capabilities)
	if err != nil {
		return nil, err
	}
	for _, phase := range phases {
		if phaseWorkState(phase) == "" {
			return nil, fmt.Errorf("unsupported phase work filter %q", phase)
		}
	}
	var claim *PhaseWorkClaim
	err = r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		workerID, err := registerCapabilityWorker(ctx, q, owner, capabilities)
		if err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `
			UPDATE phase_work_items
			SET state = 'ready', claim_owner = '', lease_expires_at = NULL,
			    available_at = clock_timestamp(), updated_at = clock_timestamp(),
			    ready_since = COALESCE(ready_since, clock_timestamp()),
			    error = 'phase lease expired and was reclaimed'
			WHERE state = 'claimed' AND lease_expires_at <= clock_timestamp()
		`)
		if err != nil {
			return fmt.Errorf("recover expired phase work: %w", err)
		}
		if tag.RowsAffected() > 0 {
			if err := recordLeaseExpiry(
				ctx, q, "phase", "reclaimed", tag.RowsAffected(),
			); err != nil {
				return err
			}
		}

		fairness, found, err := selectFairPhaseProject(
			ctx, q, workerID, phases,
		)
		if err != nil || !found {
			return err
		}
		var item builder.PhaseWorkClaim
		var requestJSON, statusJSON []byte
		var leaseOwner string
		var readySince time.Time
		var workerCandidateCount, workerPressureScore, workerRecentFailures int
		err = q.QueryRow(ctx, `
			SELECT w.id::text, w.project_id::text, w.job_id::text,
			       w.attempt_id::text, a.fence_token, w.phase, w.sequence,
			       w.claim_fence, j.request, j.status_snapshot,
			       owner.stable_name,
			       COALESCE(w.ready_since, w.available_at),
			       selected_worker.candidate_count,
			       selected_worker.pressure_score,
			       selected_worker.recent_failures
			FROM phase_work_items w
			JOIN build_attempts a ON a.id = w.attempt_id
			JOIN build_jobs j ON j.id = w.job_id
			JOIN project_resource_reservations r
			  ON r.attempt_id = w.attempt_id AND r.state = 'active'
			JOIN project_attempt_usage u
			  ON u.attempt_id = w.attempt_id AND u.state = 'active'
			JOIN worker_leases l ON l.attempt_id = w.attempt_id
			JOIN workers owner ON owner.id = l.worker_id
			JOIN workers executor ON executor.id = $2
			JOIN LATERAL (
			  SELECT scored.id, scored.pressure_score,
			         scored.recent_failures, scored.candidate_count
			  FROM (
			    SELECT ranked.*,
			           count(*) OVER ()::integer AS candidate_count
			    FROM (
			    SELECT candidate.id,
			      CASE
			        WHEN candidate.capabilities ->> 'pressure_score'
			             ~ '^[0-9]{1,4}$'
			        THEN LEAST(
			          1000,
			          (candidate.capabilities ->> 'pressure_score')::integer
			        )
			        ELSE 500
			      END AS pressure_score,
			      (
			        SELECT count(*)::integer
			        FROM phase_work_items recent
			        WHERE recent.claim_owner = candidate.stable_name
			          AND recent.state = 'failed'
			          AND recent.updated_at >
			              clock_timestamp() - interval '1 hour'
			      ) AS recent_failures
			    FROM workers candidate
			    WHERE candidate.desired_state = 'active'
			      AND candidate.last_seen_at >
			          clock_timestamp() - interval '5 seconds'
			      AND candidate.executor_protocol >=
			          j.minimum_executor_protocol
			      AND NOT EXISTS (
			        SELECT 1
			        FROM jsonb_array_elements_text(
			          w.required_capabilities
			        ) required(label)
			        WHERE NOT (
			          COALESCE(
			            candidate.capabilities -> 'labels', '[]'::jsonb
			          ) ? required.label
			        )
			      )
			      AND NOT EXISTS (
			        SELECT 1
			        FROM phase_work_items busy
			        WHERE busy.claim_owner = candidate.stable_name
			          AND busy.state = 'claimed'
			          AND busy.lease_expires_at > clock_timestamp()
			      )
			      AND (
			        NOT EXISTS (
			          SELECT 1
			          FROM jsonb_array_elements_text(
			            COALESCE(
			              executor.capabilities -> 'labels', '[]'::jsonb
			            )
			          ) current_kind(label)
			          WHERE current_kind.label LIKE 'worker-kind:%'
			        )
			        OR EXISTS (
			          SELECT 1
			          FROM jsonb_array_elements_text(
			            COALESCE(
			              executor.capabilities -> 'labels', '[]'::jsonb
			            )
			          ) current_kind(label)
			          WHERE current_kind.label LIKE 'worker-kind:%'
			            AND COALESCE(
			              candidate.capabilities -> 'labels', '[]'::jsonb
			            ) ? current_kind.label
			        )
			      )
			    ) ranked
			  ) scored
			  WHERE scored.id = executor.id
			) selected_worker ON true
			WHERE w.project_id = $3
			  AND w.state = 'ready'
			  AND w.execution_mode = 'active'
			  AND w.available_at <= clock_timestamp()
			  AND (
			    COALESCE(cardinality($1::text[]), 0) = 0
			    OR w.phase = ANY($1::text[])
			  )
			  AND executor.desired_state = 'active'
			  AND executor.last_seen_at >
			      clock_timestamp() - interval '45 seconds'
			  AND executor.executor_protocol >= j.minimum_executor_protocol
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
			  AND j.state NOT IN ('completed', 'success', 'failed', 'canceled')
			  AND a.state NOT IN ('completed', 'success', 'failed', 'canceled')
			  AND l.fence_token = a.fence_token
			  AND l.expires_at > clock_timestamp()
			  AND u.metering_started_at
			      + make_interval(secs => u.max_runtime_seconds::double precision)
			      > clock_timestamp()
			ORDER BY (r.phase = w.phase) DESC,
			         w.available_at, w.created_at, w.id
			LIMIT 1
			FOR UPDATE OF w SKIP LOCKED
		`, phases, workerID, fairness.ProjectID).Scan(
			&item.ID, &item.ProjectID, &item.JobID, &item.AttemptID,
			&item.AttemptFence, &item.Phase, &item.Sequence, &item.ClaimFence,
			&requestJSON, &statusJSON, &leaseOwner, &readySince,
			&workerCandidateCount, &workerPressureScore,
			&workerRecentFailures,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("select ready phase work: %w", err)
		}
		request, err := decodeStoredRequest(requestJSON)
		if err != nil {
			return err
		}
		request.ProjectID = item.ProjectID
		status, err := decodeStoredStatus(statusJSON, request)
		if err != nil {
			return err
		}
		status.JobID, status.ProjectID = item.JobID, item.ProjectID
		status.AttemptID, status.FenceToken = item.AttemptID, item.AttemptFence
		status.LeaseOwner = leaseOwner
		item.Request, item.Status = request, status
		now, err := readDatabaseClock(ctx, q)
		if err != nil {
			return err
		}
		if err := updateResourceReservationPhase(
			ctx, q, item.AttemptID, phaseWorkState(item.Phase), now,
		); err != nil {
			if _, full := builder.AsPhaseCapacityError(err); full {
				if _, deferErr := q.Exec(ctx, `
					UPDATE phase_work_items
					SET available_at = clock_timestamp() + interval '250 milliseconds',
					    updated_at = clock_timestamp()
					WHERE id = $1
				`, item.ID); deferErr != nil {
					return fmt.Errorf("defer phase-capacity work: %w", deferErr)
				}
				return nil
			}
			return err
		}
		item.ClaimFence++
		item.ClaimOwner = owner
		err = q.QueryRow(ctx, `
			UPDATE phase_work_items
			SET state = 'claimed', claim_owner = $2, claim_fence = $3,
			    lease_expires_at = clock_timestamp() + $4::interval,
			    started_at = COALESCE(started_at, clock_timestamp()),
			    updated_at = clock_timestamp(), error = ''
			WHERE id = $1 AND state = 'ready'
			RETURNING lease_expires_at
		`, item.ID, owner, item.ClaimFence, lease.String()).Scan(
			&item.LeaseExpiresAt,
		)
		if err != nil {
			return fmt.Errorf("claim phase work: %w", err)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO scheduler_worker_decisions (
			  work_kind, job_id, phase, worker_id, candidate_count,
			  pressure_score, recent_failures, reason
			) VALUES (
			  'phase', $1, $2, $3, $4, $5, $6,
			  'eligible polling worker; pressure and recent failures observed'
			)
		`, item.JobID, item.Phase, workerID, workerCandidateCount,
			workerPressureScore, workerRecentFailures); err != nil {
			return fmt.Errorf("record phase worker decision: %w", err)
		}
		dispatch, err := recordFairDispatch(
			ctx, q, fairness, "phase", readySince,
		)
		if err != nil {
			return err
		}
		if err := r.insertJobEvent(
			ctx, q, item.JobID, "scheduler.fair_dispatch",
			map[string]any{
				"kind": "phase", "phase": item.Phase,
				"work_id": item.ID, "claim_fence": item.ClaimFence,
				"priority_weight":              fairness.PriorityWeight,
				"starvation_threshold_seconds": fairness.StarvationThreshold,
				"starved":                      fairness.Starved,
				"project_oldest_queued_at":     fairness.OldestAt,
				"virtual_runtime_before":       fairness.VRuntimeBefore,
				"virtual_runtime_after":        dispatch.VRuntimeAfter,
				"item_wait_seconds":            dispatch.ItemWaitSecond,
			},
		); err != nil {
			return err
		}
		claim = &item
		return nil
	})
	r.recordWrite(err)
	return claim, err
}

func registerCapabilityWorker(
	ctx context.Context,
	q Querier,
	stableName string,
	labels []string,
) (uuid.UUID, error) {
	telemetry := builder.CaptureExecutorTelemetry(labels)
	capabilities, err := json.Marshal(map[string]any{
		"role":              "phase-executor",
		"executor_protocol": builder.ExecutorProtocolVersion,
		"labels":            labels,
		"pressure_score":    telemetry.PressureScore,
		"cache_keys":        telemetry.CacheKeys,
		"telemetry":         telemetry,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal executor capabilities: %w", err)
	}
	workerID := uuid.New()
	if err := q.QueryRow(ctx, `
		INSERT INTO workers (
			id, stable_name, desired_state, capabilities, max_slots,
			last_seen_at, executor_protocol
		) VALUES (
			$1, $2, 'active', $3::jsonb, 1,
			clock_timestamp(), $4
		)
		ON CONFLICT (stable_name)
		DO UPDATE SET
			last_seen_at = clock_timestamp(),
			capabilities = EXCLUDED.capabilities,
			executor_protocol = EXCLUDED.executor_protocol,
			generation = workers.generation + CASE
			  WHEN workers.capabilities -> 'labels' IS DISTINCT FROM
			       EXCLUDED.capabilities -> 'labels'
			    OR workers.capabilities ->> 'role' IS DISTINCT FROM
			       EXCLUDED.capabilities ->> 'role'
			    OR workers.executor_protocol IS DISTINCT FROM
			       EXCLUDED.executor_protocol
			  THEN 1 ELSE 0 END,
			updated_at = clock_timestamp()
		RETURNING id
	`, workerID, stableName, string(capabilities), builder.ExecutorProtocolVersion).Scan(
		&workerID,
	); err != nil {
		return uuid.Nil, fmt.Errorf("register capability worker: %w", err)
	}
	return workerID, nil
}

func phaseWorkState(phase string) string {
	switch phase {
	case "provision":
		return "provisioning"
	case "build":
		return "building"
	case "verify":
		return "verifying"
	case "publish":
		return "publishing"
	default:
		return ""
	}
}

// RenewPhaseWork extends only the exact owner/fence lease.
func (r *JobRepository) RenewPhaseWork(
	ctx context.Context,
	claim *builder.PhaseWorkClaim,
	lease time.Duration,
) error {
	if claim == nil || lease <= 0 {
		return fmt.Errorf("phase claim and positive lease are required")
	}
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		// The renewal ticker and the phase-executing goroutine share this
		// claim pointer, so the renewed expiry is read into a local instead of
		// written back through it. PostgreSQL remains the only authority for
		// the lease; a caller that needs the new deadline must read it there.
		var leaseExpiresAt time.Time
		if err := q.QueryRow(ctx, `
			UPDATE phase_work_items
			SET lease_expires_at = clock_timestamp() + $4::interval,
			    updated_at = clock_timestamp()
			WHERE id = $1 AND state = 'claimed'
			  AND claim_owner = $2 AND claim_fence = $3
			  AND lease_expires_at > clock_timestamp()
			RETURNING lease_expires_at
		`, claim.ID, claim.ClaimOwner, claim.ClaimFence, lease.String()).Scan(
			&leaseExpiresAt,
		); err != nil {
			return err
		}
		// The attempt lease deliberately outlives the shorter phase lease.
		// If a control plane dies, another replica can reclaim this phase
		// before generic scheduler recovery expires the whole attempt.
		tag, err := q.Exec(ctx, `
			UPDATE worker_leases
			SET expires_at = LEAST(
			      (
			        SELECT u.metering_started_at
			          + make_interval(secs => u.max_runtime_seconds::double precision)
			        FROM project_attempt_usage u
			        WHERE u.attempt_id = worker_leases.attempt_id
			      ),
			      GREATEST(
			        expires_at, clock_timestamp() + interval '2 minutes'
			      )
			    ),
			    renewed_at = clock_timestamp()
			WHERE attempt_id = $1 AND fence_token = $2
			  AND expires_at > clock_timestamp()
			  AND EXISTS (
			    SELECT 1 FROM project_attempt_usage u
			    WHERE u.attempt_id = worker_leases.attempt_id
			      AND u.state = 'active'
			      AND u.metering_started_at
			          + make_interval(secs => u.max_runtime_seconds::double precision)
			          > clock_timestamp()
			  )
		`, claim.AttemptID, claim.AttemptFence)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("attempt lease is stale or expired")
		}
		return nil
	})
	if err != nil {
		r.recordWrite(err)
		return fmt.Errorf(
			"phase work lease is stale, expired, or no longer owned: %w", err,
		)
	}
	r.recordWrite(nil)
	return nil
}

// CompletePhaseWork releases the destination phase and makes the next item
// ready in the same transaction.
func (r *JobRepository) CompletePhaseWork(
	ctx context.Context,
	claim *builder.PhaseWorkClaim,
) error {
	return r.finishPhaseWork(ctx, claim, "completed", "")
}

// FailPhaseWork records a terminal phase error and cancels later work items.
func (r *JobRepository) FailPhaseWork(
	ctx context.Context,
	claim *builder.PhaseWorkClaim,
	detail string,
) error {
	return r.finishPhaseWork(ctx, claim, "failed", detail)
}

func (r *JobRepository) finishPhaseWork(
	ctx context.Context,
	claim *builder.PhaseWorkClaim,
	state, detail string,
) error {
	if claim == nil || (state != "completed" && state != "failed") {
		return fmt.Errorf("valid phase claim and terminal state are required")
	}
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		var attemptID, projectID, phase string
		var sequence int
		var leaseActive bool
		err := q.QueryRow(ctx, `
			SELECT attempt_id::text, project_id::text, phase, sequence,
			       lease_expires_at > clock_timestamp()
			FROM phase_work_items
			WHERE id = $1 AND state = 'claimed'
			  AND claim_owner = $2 AND claim_fence = $3
			FOR UPDATE
		`, claim.ID, claim.ClaimOwner, claim.ClaimFence).Scan(
			&attemptID, &projectID, &phase, &sequence, &leaseActive,
		)
		if err != nil {
			return fmt.Errorf("lock claimed phase work: %w", err)
		}
		if !leaseActive {
			return fmt.Errorf("phase work lease expired")
		}
		var now time.Time
		if err := q.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return fmt.Errorf("read database clock: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE phase_work_items
			SET state = $2, lease_expires_at = NULL, finished_at = $3,
			    updated_at = $3, error = $4
			WHERE id = $1
		`, claim.ID, state, now, strings.TrimSpace(detail)); err != nil {
			return fmt.Errorf("finish phase work: %w", err)
		}
		if state == "completed" {
			if _, err := q.Exec(ctx, `
				UPDATE phase_work_items
				SET state = 'ready', available_at = $3, ready_since = $3,
				    updated_at = $3
				WHERE attempt_id = $1 AND sequence > $2 AND state = 'blocked'
				  AND sequence = (
					SELECT min(sequence) FROM phase_work_items
					WHERE attempt_id = $1 AND sequence > $2 AND state = 'blocked'
				  )
			`, attemptID, sequence, now); err != nil {
				return fmt.Errorf("release next phase work: %w", err)
			}
		} else if _, err := q.Exec(ctx, `
			UPDATE phase_work_items
			SET state = 'canceled', finished_at = $3, updated_at = $3,
			    error = 'prior phase failed'
			WHERE attempt_id = $1 AND sequence > $2
			  AND state IN ('blocked', 'ready')
		`, attemptID, sequence, now); err != nil {
			return fmt.Errorf("cancel dependent phase work: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE project_resource_reservations
			SET phase = 'waiting', phase_updated_at = $2
			WHERE attempt_id = $1 AND state = 'active'
		`, attemptID, now); err != nil {
			return fmt.Errorf("release completed phase capacity: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE phase_work_items
			SET available_at = LEAST(available_at, $3), updated_at = $3
			WHERE project_id = $1 AND phase = $2 AND state = 'ready'
		`, projectID, phase, now); err != nil {
			return fmt.Errorf("wake phase-capacity waiters: %w", err)
		}
		return nil
	})
	r.recordWrite(err)
	return err
}

func (r *JobRepository) PhaseWorkStatus(
	ctx context.Context,
	projectID string,
) (PhaseWorkStatus, error) {
	var status PhaseWorkStatus
	err := r.db.Pool().QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE execution_mode = 'shadow'),
			count(*) FILTER (WHERE execution_mode = 'active'),
			count(*) FILTER (WHERE state = 'blocked'),
			count(*) FILTER (WHERE state = 'ready'),
			count(*) FILTER (
			  WHERE execution_mode = 'active' AND state = 'ready'
			    AND NOT EXISTS (
			      SELECT 1
			      FROM build_jobs capability_job
			      JOIN workers capability_worker
			        ON capability_worker.desired_state = 'active'
			       AND capability_worker.last_seen_at >
			           clock_timestamp() - interval '45 seconds'
			       AND capability_worker.executor_protocol >=
			           capability_job.minimum_executor_protocol
			      WHERE capability_job.id = phase_work_items.job_id
			        AND NOT EXISTS (
			          SELECT 1
			          FROM jsonb_array_elements_text(
			            phase_work_items.required_capabilities
			          ) AS required(label)
			          WHERE NOT (
			            COALESCE(
			              capability_worker.capabilities -> 'labels',
			              '[]'::jsonb
			            ) ? required.label
			          )
			        )
			    )
			),
			count(*) FILTER (WHERE state = 'claimed'),
			count(*) FILTER (WHERE state = 'completed'),
			count(*) FILTER (WHERE state = 'failed'),
			count(*) FILTER (WHERE state = 'canceled')
		FROM phase_work_items
		WHERE project_id = $1
	`, projectID).Scan(
		&status.Shadow, &status.Active,
		&status.Blocked, &status.Ready, &status.Unschedulable, &status.Claimed,
		&status.Completed, &status.Failed, &status.Canceled,
	)
	if err != nil {
		return status, fmt.Errorf("read phase work status: %w", err)
	}
	return status, nil
}

// GlobalPhaseWorkStatus is the low-cardinality operator view used by the
// control-plane monitor. Project-scoped APIs must continue using
// PhaseWorkStatus so tenant activity is never disclosed across projects.
func (r *JobRepository) GlobalPhaseWorkStatus(
	ctx context.Context,
) (PhaseWorkStatus, error) {
	var status PhaseWorkStatus
	err := r.db.Pool().QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE execution_mode = 'shadow'),
			count(*) FILTER (WHERE execution_mode = 'active'),
			count(*) FILTER (WHERE state = 'blocked'),
			count(*) FILTER (WHERE state = 'ready'),
			count(*) FILTER (
			  WHERE execution_mode = 'active' AND state = 'ready'
			    AND NOT EXISTS (
			      SELECT 1
			      FROM build_jobs capability_job
			      JOIN workers capability_worker
			        ON capability_worker.desired_state = 'active'
			       AND capability_worker.last_seen_at >
			           clock_timestamp() - interval '45 seconds'
			       AND capability_worker.executor_protocol >=
			           capability_job.minimum_executor_protocol
			      WHERE capability_job.id = phase_work_items.job_id
			        AND NOT EXISTS (
			          SELECT 1
			          FROM jsonb_array_elements_text(
			            phase_work_items.required_capabilities
			          ) AS required(label)
			          WHERE NOT (
			            COALESCE(
			              capability_worker.capabilities -> 'labels',
			              '[]'::jsonb
			            ) ? required.label
			          )
			        )
			    )
			),
			count(*) FILTER (WHERE state = 'claimed'),
			count(*) FILTER (WHERE state = 'completed'),
			count(*) FILTER (WHERE state = 'failed'),
			count(*) FILTER (WHERE state = 'canceled')
		FROM phase_work_items
	`).Scan(
		&status.Shadow, &status.Active,
		&status.Blocked, &status.Ready, &status.Unschedulable, &status.Claimed,
		&status.Completed, &status.Failed, &status.Canceled,
	)
	if err != nil {
		return status, fmt.Errorf("read global phase work status: %w", err)
	}
	return status, nil
}
