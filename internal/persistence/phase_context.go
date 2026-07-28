package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
)

// SavePhaseExecutionContext stores only the exact phase claimant's hand-off
// document. A reclaimed/stale executor cannot overwrite a newer replica.
func (r *JobRepository) SavePhaseExecutionContext(
	ctx context.Context,
	claim *builder.PhaseWorkClaim,
	value *builder.PhaseExecutionContext,
) error {
	if claim == nil || value == nil {
		return fmt.Errorf("phase claim and execution context are required")
	}
	if err := validatePhaseContext(value); err != nil {
		return err
	}
	document, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode phase execution context: %w", err)
	}
	tag, err := r.db.Pool().Exec(ctx, `
		INSERT INTO phase_execution_contexts (
			attempt_id, project_id, job_id, attempt_fence,
			context_sequence, context, revision, created_at, updated_at
		)
		SELECT w.attempt_id, w.project_id, w.job_id, $4,
		       w.sequence, $5::jsonb, 1, clock_timestamp(), clock_timestamp()
		FROM phase_work_items w
		WHERE w.id = $1 AND w.claim_owner = $2 AND w.claim_fence = $3
		  AND w.state = 'claimed' AND w.lease_expires_at > clock_timestamp()
		  AND EXISTS (
		      SELECT 1 FROM build_attempts a
		      JOIN worker_leases l ON l.attempt_id = a.id
		      WHERE a.id = w.attempt_id AND a.fence_token = $4
		        AND l.fence_token = $4 AND l.expires_at > clock_timestamp()
		  )
		ON CONFLICT (attempt_id) DO UPDATE
		SET context_sequence = GREATEST(
		      phase_execution_contexts.context_sequence,
		      EXCLUDED.context_sequence
		    ),
		    context = EXCLUDED.context,
		    revision = phase_execution_contexts.revision + 1,
		    updated_at = clock_timestamp()
		WHERE phase_execution_contexts.job_id = EXCLUDED.job_id
		  AND phase_execution_contexts.attempt_fence = EXCLUDED.attempt_fence
	`, claim.ID, claim.ClaimOwner, claim.ClaimFence, claim.AttemptFence,
		string(document))
	if err != nil {
		r.recordWrite(err)
		return fmt.Errorf("save phase execution context: %w", err)
	}
	if tag.RowsAffected() != 1 {
		err = fmt.Errorf("phase execution context fence is stale or expired")
		r.recordWrite(err)
		return err
	}
	r.recordWrite(nil)
	return nil
}

func validatePhaseContext(value *builder.PhaseExecutionContext) error {
	if value.Instance == nil {
		return nil
	}
	for key := range value.Instance.Metadata {
		lower := strings.ToLower(key)
		for _, forbidden := range []string{
			"password", "secret", "credential", "private_key", "token", "cert_pem",
		} {
			if strings.Contains(lower, forbidden) {
				return fmt.Errorf("phase execution context rejects secret-like metadata key %q", key)
			}
		}
	}
	return nil
}

// LoadPhaseExecutionContext returns the latest context only to the currently
// leased phase claimant.
func (r *JobRepository) LoadPhaseExecutionContext(
	ctx context.Context,
	claim *builder.PhaseWorkClaim,
) (*builder.PhaseExecutionContext, error) {
	if claim == nil {
		return nil, fmt.Errorf("phase claim is required")
	}
	var document []byte
	err := r.db.Pool().QueryRow(ctx, `
		SELECT COALESCE(c.context, '{}'::jsonb)
		FROM phase_work_items w
		LEFT JOIN phase_execution_contexts c ON c.attempt_id = w.attempt_id
		WHERE w.id = $1 AND w.claim_owner = $2 AND w.claim_fence = $3
		  AND w.state = 'claimed' AND w.lease_expires_at > clock_timestamp()
		  AND w.attempt_id = $4
	`, claim.ID, claim.ClaimOwner, claim.ClaimFence, claim.AttemptID).Scan(&document)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("phase execution context fence is stale or expired")
	}
	if err != nil {
		return nil, fmt.Errorf("load phase execution context: %w", err)
	}
	if len(document) == 0 {
		return &builder.PhaseExecutionContext{}, nil
	}
	var value builder.PhaseExecutionContext
	if err := json.Unmarshal(document, &value); err != nil {
		return nil, fmt.Errorf("decode phase execution context: %w", err)
	}
	return &value, nil
}

// FinalizePhaseWork atomically marks publish and its job/attempt completed.
// There is no restart window with completed phase work and a live publishing
// job whose attempt lease can no longer be renewed.
func (r *JobRepository) FinalizePhaseWork(
	ctx context.Context,
	claim *builder.PhaseWorkClaim,
	previous, completed *builder.BuildStatus,
) error {
	if claim == nil || previous == nil || completed == nil ||
		claim.Phase != "publish" || completed.Status != "completed" {
		return fmt.Errorf("final publish claim and completed status are required")
	}
	statusJSON, statusDigest, err := statusDocument(completed)
	if err != nil {
		return err
	}
	err = r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := lockTerminalPhaseClaim(ctx, q, claim); err != nil {
			return err
		}
		var state string
		if err := q.QueryRow(ctx, `
			SELECT state FROM build_jobs WHERE id = $1 FOR UPDATE
		`, claim.JobID).Scan(&state); err != nil {
			return fmt.Errorf("lock final phase job: %w", err)
		}
		if state != previous.Status {
			return fmt.Errorf("job %s ledger state is %s, expected %s",
				claim.JobID, state, previous.Status)
		}
		now := time.Now().UTC()
		if _, err := q.Exec(ctx, `
			UPDATE phase_work_items
			SET state = 'completed', lease_expires_at = NULL,
			    finished_at = $2, updated_at = $2, error = ''
			WHERE id = $1
		`, claim.ID, now); err != nil {
			return fmt.Errorf("complete final phase: %w", err)
		}
		if err := finishPhaseAttemptTx(
			ctx, q, claim, completed, string(statusJSON), statusDigest, now, "",
		); err != nil {
			return err
		}
		return r.insertJobEvent(ctx, q, claim.JobID, "job.phase_plan_completed",
			map[string]any{
				"attempt_id": claim.AttemptID, "attempt_fence": claim.AttemptFence,
				"phase_claim_fence": claim.ClaimFence,
			})
	})
	r.recordWrite(err)
	return err
}

// FailPhaseWorkAndJob atomically fails one phase, cancels its dependants and
// terminates the owning job/attempt.
func (r *JobRepository) FailPhaseWorkAndJob(
	ctx context.Context,
	claim *builder.PhaseWorkClaim,
	previous *builder.BuildStatus,
	detail string,
) error {
	if claim == nil || previous == nil {
		return fmt.Errorf("phase claim and previous job status are required")
	}
	detail = strings.TrimSpace(detail)
	if detail == "" {
		detail = "phase execution failed"
	}
	if len(detail) > maxLedgerErrorBytes {
		detail = detail[:maxLedgerErrorBytes]
	}
	failed := *previous
	failed.Status, failed.Error, failed.FailedStage = "failed", detail, claim.Phase
	failed.UpdatedAt = time.Now().UTC()
	statusJSON, statusDigest, err := statusDocument(&failed)
	if err != nil {
		return err
	}
	err = r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := lockTerminalPhaseClaim(ctx, q, claim); err != nil {
			return err
		}
		var state string
		if err := q.QueryRow(ctx, `
			SELECT state FROM build_jobs WHERE id = $1 FOR UPDATE
		`, claim.JobID).Scan(&state); err != nil {
			return fmt.Errorf("lock failed phase job: %w", err)
		}
		if state != previous.Status {
			return fmt.Errorf("job %s ledger state is %s, expected %s",
				claim.JobID, state, previous.Status)
		}
		now := failed.UpdatedAt
		if _, err := q.Exec(ctx, `
			UPDATE phase_work_items
			SET state = 'failed', lease_expires_at = NULL,
			    finished_at = $2, updated_at = $2, error = $3
			WHERE id = $1
		`, claim.ID, now, detail); err != nil {
			return fmt.Errorf("fail active phase: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE phase_work_items
			SET state = 'canceled', finished_at = $3, updated_at = $3,
			    error = 'prior phase failed'
			WHERE attempt_id = $1 AND sequence > $2
			  AND state IN ('blocked', 'ready')
		`, claim.AttemptID, claim.Sequence, now); err != nil {
			return fmt.Errorf("cancel failed phase dependants: %w", err)
		}
		if err := finishPhaseAttemptTx(
			ctx, q, claim, &failed, string(statusJSON), statusDigest, now, detail,
		); err != nil {
			return err
		}
		return r.insertJobEvent(ctx, q, claim.JobID, "job.phase_failed",
			map[string]any{
				"attempt_id": claim.AttemptID, "phase": claim.Phase,
				"phase_claim_fence": claim.ClaimFence, "detail": detail,
			})
	})
	r.recordWrite(err)
	return err
}

func lockTerminalPhaseClaim(
	ctx context.Context,
	q Querier,
	claim *builder.PhaseWorkClaim,
) error {
	var valid bool
	err := q.QueryRow(ctx, `
		SELECT (
		  w.lease_expires_at > clock_timestamp()
		  AND a.fence_token = $5
		  AND l.fence_token = $5
		  AND l.expires_at > clock_timestamp()
		)
		FROM phase_work_items w
		JOIN build_attempts a ON a.id = w.attempt_id
		JOIN worker_leases l ON l.attempt_id = a.id
		JOIN project_attempt_usage u ON u.attempt_id = a.id
		WHERE w.id = $1 AND w.claim_owner = $2 AND w.claim_fence = $3
		  AND w.state = 'claimed' AND w.attempt_id = $4
		  AND u.state = 'active'
		  AND u.metering_started_at
		      + make_interval(secs => u.max_runtime_seconds::double precision)
		      > clock_timestamp()
		FOR UPDATE OF w, a, l
	`, claim.ID, claim.ClaimOwner, claim.ClaimFence, claim.AttemptID,
		claim.AttemptFence).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) || !valid {
		return fmt.Errorf("terminal phase claim is stale or expired")
	}
	if err != nil {
		return fmt.Errorf("lock terminal phase claim: %w", err)
	}
	return nil
}

func finishPhaseAttemptTx(
	ctx context.Context,
	q Querier,
	claim *builder.PhaseWorkClaim,
	status *builder.BuildStatus,
	statusJSON, statusDigest string,
	now time.Time,
	detail string,
) error {
	tag, err := q.Exec(ctx, `
		UPDATE build_attempts
		SET state = $2, failure_detail = $3, finished_at = $4, updated_at = $4
		WHERE id = $1 AND job_id = $5 AND fence_token = $6
	`, claim.AttemptID, status.Status, detail, now, claim.JobID,
		claim.AttemptFence)
	if err != nil {
		return fmt.Errorf("finish phase attempt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("finish phase attempt: attempt fence is stale")
	}
	if err := settleAttemptUsage(
		ctx, q, claim.AttemptID, "job_"+status.Status,
		status.Status, now,
	); err != nil {
		return err
	}
	tag, err = q.Exec(ctx, `
		UPDATE build_jobs
		SET state = $2, status_snapshot = $3::jsonb, status_digest = $4,
		    terminal_reason = $5, updated_at = $6, completed_at = $6,
		    ledger_revision = ledger_revision + 1
		WHERE id = $1
	`, claim.JobID, status.Status, statusJSON, statusDigest, detail, now)
	if err != nil {
		return fmt.Errorf("finish phase job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("finish phase job: job disappeared")
	}
	if _, err := q.Exec(ctx, `
		DELETE FROM worker_leases WHERE attempt_id = $1 AND fence_token = $2
	`, claim.AttemptID, claim.AttemptFence); err != nil {
		return fmt.Errorf("release final phase attempt lease: %w", err)
	}
	if err := releaseResourceReservation(
		ctx, q, claim.AttemptID, "job_"+status.Status, now,
	); err != nil {
		return err
	}
	if err := releaseArtifactBudgetReservation(
		ctx, q, claim.AttemptID, "job_"+status.Status, now,
	); err != nil {
		return err
	}
	return revokeWorkerGatewayAttempt(
		ctx, q, claim.AttemptID, "job_"+status.Status, now,
	)
}
