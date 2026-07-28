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

const (
	legacyMaxRuntimeMinutes            = 60
	legacyCloudCostMicrounitsPerMinute = int64(1000)
)

type runtimeBudgetSpec struct {
	MaxRuntimeSeconds            int64
	CloudCostMicrounitsPerMinute int64
	ReservedCloudCostMicrounits  int64
}

func runtimeBudgetSpecFromRequest(request *builder.BuildRequest) (runtimeBudgetSpec, error) {
	minutes := legacyMaxRuntimeMinutes
	rate := legacyCloudCostMicrounitsPerMinute
	if request == nil {
		return runtimeBudgetSpec{}, fmt.Errorf("runtime accounting requires a build request")
	}
	if request.ResolvedContext != nil {
		resolvedMinutes := request.ResolvedContext.MaxRuntimeMinutes
		resolvedRate := request.ResolvedContext.CloudCostMicrounitsPerMinute
		// Historical resolved contexts created before schema v17 have neither
		// field and use the compatibility accounting class. A partially
		// populated new contract is rejected.
		if resolvedMinutes != 0 || resolvedRate != 0 {
			minutes, rate = resolvedMinutes, resolvedRate
		}
		if minutes <= 0 || rate <= 0 {
			return runtimeBudgetSpec{}, fmt.Errorf(
				"resource class %q has no positive runtime/cost accounting contract",
				request.ResourceClass,
			)
		}
	}
	if minutes <= 0 || minutes > 24*60 || rate <= 0 ||
		rate > 1_000_000_000_000 {
		return runtimeBudgetSpec{}, fmt.Errorf("runtime/cost accounting contract is outside supported bounds")
	}
	seconds := int64(minutes) * 60
	if rate > (1<<63-1)/int64(minutes) {
		return runtimeBudgetSpec{}, fmt.Errorf("runtime/cost accounting contract overflows")
	}
	return runtimeBudgetSpec{
		MaxRuntimeSeconds:            seconds,
		CloudCostMicrounitsPerMinute: rate,
		ReservedCloudCostMicrounits:  int64(minutes) * rate,
	}, nil
}

func insertAttemptUsage(
	ctx context.Context,
	q Querier,
	attemptID uuid.UUID,
	projectID string,
	jobID uuid.UUID,
	spec runtimeBudgetSpec,
	startedAt time.Time,
) error {
	_, err := q.Exec(ctx, `
		INSERT INTO project_attempt_usage (
			attempt_id, project_id, job_id, budget_day,
			max_runtime_seconds, cloud_cost_microunits_per_minute,
			reserved_build_seconds, reserved_cloud_cost_microunits,
			metering_started_at
		) VALUES (
			$1, $2, $3, ($4::timestamptz AT TIME ZONE 'UTC')::date,
			$5, $6, $5, $7, $4
		)
	`, attemptID, projectID, jobID, startedAt, spec.MaxRuntimeSeconds,
		spec.CloudCostMicrounitsPerMinute, spec.ReservedCloudCostMicrounits)
	if err != nil {
		return fmt.Errorf("reserve project runtime budget: %w", err)
	}
	return nil
}

func markAttemptCloudStarted(
	ctx context.Context,
	q Querier,
	attemptID string,
	startedAt time.Time,
) error {
	if _, err := q.Exec(ctx, `
		UPDATE project_attempt_usage
		SET cloud_started_at = COALESCE(cloud_started_at, $2)
		WHERE attempt_id = $1 AND state = 'active'
	`, attemptID, startedAt); err != nil {
		return fmt.Errorf("start cloud cost meter: %w", err)
	}
	return nil
}

func settleAttemptUsage(
	ctx context.Context,
	q Querier,
	attemptID, reason, terminalState string,
	settledAt time.Time,
) error {
	var projectID string
	err := q.QueryRow(ctx, `
		UPDATE project_attempt_usage
		SET charged_build_seconds = LEAST(
		      reserved_build_seconds,
		      GREATEST(
		        1, CEIL(EXTRACT(EPOCH FROM ($2::timestamptz - metering_started_at)))
		      )::bigint
		    ),
		    charged_cloud_cost_microunits = CASE
		      WHEN cloud_started_at IS NULL THEN 0
		      ELSE LEAST(
		        reserved_cloud_cost_microunits,
		        GREATEST(
		          1, CEIL(EXTRACT(EPOCH FROM ($2::timestamptz - cloud_started_at)) / 60)
		        )::bigint * cloud_cost_microunits_per_minute
		      )
		    END,
		    state = 'settled', settled_at = $2, settlement_reason = $3
		WHERE attempt_id = $1 AND state = 'active'
		RETURNING project_id::text
	`, attemptID, settledAt, reason).Scan(&projectID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("settle project runtime budget: %w", err)
	}
	if _, err := q.Exec(ctx, `
		UPDATE build_jobs
		SET next_attempt_at = LEAST(next_attempt_at, $2)
		WHERE project_id = $1 AND state = 'queued'
		  AND cancel_requested_at IS NULL
	`, projectID, settledAt); err != nil {
		return fmt.Errorf("wake runtime-budget waiters: %w", err)
	}
	if terminalState == "failed" || terminalState == "expired" {
		return applyFailureStormSuspension(ctx, q, projectID, settledAt)
	}
	return nil
}

func settleJobAttemptUsage(
	ctx context.Context,
	q Querier,
	jobID, reason, terminalState string,
	settledAt time.Time,
) error {
	rows, err := q.Query(ctx, `
		SELECT attempt_id::text
		FROM project_attempt_usage
		WHERE job_id = $1 AND state = 'active'
		ORDER BY metering_started_at
	`, jobID)
	if err != nil {
		return fmt.Errorf("list active job runtime budgets: %w", err)
	}
	var attemptIDs []string
	for rows.Next() {
		var attemptID string
		if err := rows.Scan(&attemptID); err != nil {
			rows.Close()
			return err
		}
		attemptIDs = append(attemptIDs, attemptID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, attemptID := range attemptIDs {
		if err := settleAttemptUsage(
			ctx, q, attemptID, reason, terminalState, settledAt,
		); err != nil {
			return err
		}
	}
	return nil
}

func applyFailureStormSuspension(
	ctx context.Context,
	q Querier,
	projectID string,
	now time.Time,
) error {
	var maxFailures, cooldownSeconds int
	if err := q.QueryRow(ctx, `
		SELECT max_failures_per_hour, abuse_cooldown_seconds
		FROM project_policies
		WHERE project_id = $1
		FOR UPDATE
	`, projectID).Scan(&maxFailures, &cooldownSeconds); err != nil {
		return fmt.Errorf("lock project abuse policy: %w", err)
	}
	var failures int
	if err := q.QueryRow(ctx, `
		SELECT count(*)
		FROM build_attempts a
		JOIN build_jobs j ON j.id = a.job_id
		WHERE j.project_id = $1
		  AND a.state IN ('failed', 'expired')
		  AND a.finished_at >= $2::timestamptz - interval '1 hour'
		  AND a.finished_at <= $2
	`, projectID, now).Scan(&failures); err != nil {
		return fmt.Errorf("read project failure window: %w", err)
	}
	if failures < maxFailures {
		return nil
	}
	until := now.Add(time.Duration(cooldownSeconds) * time.Second)
	var generation int64
	err := q.QueryRow(ctx, `
		UPDATE project_policies
		SET abuse_suspended_until = GREATEST(
		      COALESCE(abuse_suspended_until, '-infinity'::timestamptz), $2
		    ),
		    abuse_reason = $3,
		    abuse_generation = abuse_generation + 1,
		    updated_by = 'scheduler.abuse-gate',
		    updated_at = $4
		WHERE project_id = $1
		RETURNING abuse_generation
	`, projectID, until,
		fmt.Sprintf("%d failed or expired attempts in the trailing hour", failures),
		now).Scan(&generation)
	if err != nil {
		return fmt.Errorf("apply project abuse suspension: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO audit_events (
			actor, action, resource_type, resource_id, project_id,
			outcome, detail, occurred_at
		) VALUES (
			'scheduler', 'project.abuse_suspended', 'project',
			$1::text, $1::uuid,
			'success',
			jsonb_build_object(
			  'failures', $2::integer,
			  'limit', $3::integer,
			  'suspended_until', $4::timestamptz,
			  'generation', $5::bigint
			),
			$6
		)
	`, projectID, failures, maxFailures, until, generation, now); err != nil {
		return fmt.Errorf("audit project abuse suspension: %w", err)
	}
	return nil
}
