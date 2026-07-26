package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
)

const schedulerRecoveryBatch = 32

func decodeStoredRequest(data []byte) (*builder.BuildRequest, error) {
	var stored storedBuildRequest
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("decode stored build request: %w", err)
	}
	return &builder.BuildRequest{
		PackageName: stored.PackageName, Version: stored.Version, Arch: stored.Arch,
		UseFlags: append([]string(nil), stored.UseFlags...), CloudProvider: stored.CloudProvider,
		ProfileID: stored.ProfileID, RepositoryIDs: append([]string(nil), stored.RepositoryIDs...),
		ResourceClass: stored.ResourceClass, MachineSpec: stored.MachineSpec,
		ResolvedContext: stored.ResolvedContext, ConfigBundle: stored.ConfigBundle,
		IdempotencyKey: stored.IdempotencyKey,
	}, nil
}

func decodeStoredStatus(data []byte, request *builder.BuildRequest) (*builder.BuildStatus, error) {
	var status builder.BuildStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("decode stored job status: %w", err)
	}
	status.Request = request
	return &status, nil
}

// ClaimNext recovers expired leases and atomically claims one eligible job
// using SKIP LOCKED. Every executor slot has a durable worker identity.
func (r *JobRepository) ClaimNext(ctx context.Context, stableWorker string, leaseDuration time.Duration) (*builder.SchedulerClaim, error) {
	if stableWorker == "" {
		return nil, fmt.Errorf("stable worker name is required")
	}
	if leaseDuration <= 0 {
		return nil, fmt.Errorf("lease duration must be positive")
	}
	var claim *builder.SchedulerClaim
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		workerID, err := r.schedulerWorker(ctx, q, stableWorker)
		if err != nil {
			return err
		}

		if _, err := r.recoverExpiredTx(ctx, q, schedulerRecoveryBatch); err != nil {
			return err
		}

		var jobID uuid.UUID
		var requestJSON, statusJSON []byte
		var maxAttempts int
		err = q.QueryRow(ctx, `
			SELECT id, request, status_snapshot, max_attempts
			FROM build_jobs
			WHERE state = 'queued'
			  AND cancel_requested_at IS NULL
			  AND legacy_visible = true
			  AND next_attempt_at <= clock_timestamp()
			ORDER BY priority DESC, next_attempt_at, created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		`).Scan(&jobID, &requestJSON, &statusJSON, &maxAttempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("select claimable job: %w", err)
		}

		request, err := decodeStoredRequest(requestJSON)
		if err != nil {
			return err
		}
		status, err := decodeStoredStatus(statusJSON, request)
		if err != nil {
			return err
		}
		status.JobID = jobID.String()

		var attemptNo int
		var fenceToken int64
		if err := q.QueryRow(ctx, `
			SELECT COALESCE(max(attempt_no), 0) + 1,
			       COALESCE(max(fence_token), 0) + 1
			FROM build_attempts
			WHERE job_id = $1
		`, jobID).Scan(&attemptNo, &fenceToken); err != nil {
			return fmt.Errorf("allocate attempt fence: %w", err)
		}
		if attemptNo > maxAttempts {
			return fmt.Errorf("job %s exceeded max attempts", jobID)
		}

		now := time.Now().UTC()
		attemptID := uuid.New()
		if _, err := q.Exec(ctx, `
			INSERT INTO build_attempts (
				id, job_id, attempt_no, state, worker_id, fence_token,
				started_at, created_at, updated_at
			) VALUES ($1, $2, $3, 'claimed', $4, $5, $6, $6, $6)
		`, attemptID, jobID, attemptNo, workerID, fenceToken, now); err != nil {
			return fmt.Errorf("insert claimed attempt: %w", err)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO worker_leases (
				id, worker_id, attempt_id, fence_token, expires_at, renewed_at, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $6)
		`, uuid.New(), workerID, attemptID, fenceToken, now.Add(leaseDuration), now); err != nil {
			return fmt.Errorf("insert worker lease: %w", err)
		}

		status.Status = "claimed"
		status.UpdatedAt = now
		status.Error = ""
		status.AttemptID = attemptID.String()
		status.FenceToken = fenceToken
		status.LeaseOwner = stableWorker
		snapshot, digest, err := statusDocument(status)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			UPDATE build_jobs
			SET state = 'claimed', status_snapshot = $2::jsonb, status_digest = $3,
			    updated_at = $4, terminal_reason = '',
			    ledger_revision = ledger_revision + 1
			WHERE id = $1
		`, jobID, string(snapshot), digest, now); err != nil {
			return fmt.Errorf("mark job claimed: %w", err)
		}
		if err := r.insertJobEvent(ctx, q, jobID.String(), "job.claimed", map[string]any{
			"attempt_id": attemptID, "attempt_no": attemptNo, "fence_token": fenceToken,
			"worker": stableWorker, "lease_expires_at": now.Add(leaseDuration),
		}); err != nil {
			return err
		}
		claim = &builder.SchedulerClaim{Request: request, Status: status}
		return nil
	})
	if err != nil || claim != nil {
		r.recordWrite(err)
	}
	return claim, err
}

func (r *JobRepository) schedulerWorker(ctx context.Context, q Querier, stableName string) (uuid.UUID, error) {
	r.workerMu.Lock()
	defer r.workerMu.Unlock()
	now := time.Now().UTC()
	if cached, ok := r.workerIDs[stableName]; ok && now.Sub(cached.Heartbeat) < 15*time.Second {
		return cached.ID, nil
	}
	workerID := uuid.New()
	if cached, ok := r.workerIDs[stableName]; ok {
		workerID = cached.ID
	}
	if err := q.QueryRow(ctx, `
		INSERT INTO workers (id, stable_name, desired_state, capabilities, max_slots, last_seen_at)
		VALUES ($1, $2, 'active', '{"role":"control-plane-executor"}'::jsonb, 1, clock_timestamp())
		ON CONFLICT (stable_name)
		DO UPDATE SET last_seen_at = clock_timestamp(), desired_state = 'active',
		              updated_at = clock_timestamp()
		RETURNING id
	`, workerID, stableName).Scan(&workerID); err != nil {
		return uuid.Nil, fmt.Errorf("register scheduler worker: %w", err)
	}
	r.workerIDs[stableName] = cachedWorker{ID: workerID, Heartbeat: now}
	return workerID, nil
}

func (r *JobRepository) PruneStaleWorkers(ctx context.Context, before time.Time) (int64, error) {
	tag, err := r.db.Pool().Exec(ctx, `
		DELETE FROM workers w
		WHERE (w.last_seen_at IS NULL OR w.last_seen_at < $1)
		  AND NOT EXISTS (SELECT 1 FROM worker_leases l WHERE l.worker_id = w.id)
	`, before)
	if err != nil {
		return 0, fmt.Errorf("prune stale scheduler workers: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *JobRepository) recoverExpiredTx(ctx context.Context, q Querier, limit int) (int, error) {
	rows, err := q.Query(ctx, `
		SELECT l.id, a.id, a.job_id, a.attempt_no, j.max_attempts,
		       j.cancel_requested_at IS NOT NULL, j.status_snapshot
		FROM worker_leases l
		JOIN build_attempts a ON a.id = l.attempt_id
		JOIN build_jobs j ON j.id = a.job_id
		WHERE l.expires_at <= clock_timestamp()
		  AND j.state NOT IN ('completed', 'success', 'failed', 'canceled')
		ORDER BY l.expires_at
		LIMIT $1
		FOR UPDATE OF l, a, j SKIP LOCKED
	`, limit)
	if err != nil {
		return 0, fmt.Errorf("select expired leases: %w", err)
	}
	defer rows.Close()

	type expiredRow struct {
		leaseID, attemptID, jobID uuid.UUID
		attemptNo, maxAttempts    int
		canceled                  bool
		statusJSON                []byte
	}
	var expired []expiredRow
	for rows.Next() {
		var row expiredRow
		if err := rows.Scan(&row.leaseID, &row.attemptID, &row.jobID, &row.attemptNo,
			&row.maxAttempts, &row.canceled, &row.statusJSON); err != nil {
			return 0, err
		}
		expired = append(expired, row)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, row := range expired {
		now := time.Now().UTC()
		state := "queued"
		reason := "previous executor lease expired; job queued for a fenced retry"
		if row.canceled {
			state = "canceled"
			reason = "job canceled while executor lease was active"
		} else if row.attemptNo >= row.maxAttempts {
			state = "failed"
			reason = "executor lease expired and maximum attempts were exhausted"
		}
		status, err := decodeStoredStatus(row.statusJSON, nil)
		if err != nil {
			return 0, err
		}
		status.Status = state
		status.Error = reason
		status.UpdatedAt = now
		status.AttemptID = ""
		status.FenceToken = 0
		status.LeaseOwner = ""
		snapshot, digest, err := statusDocument(status)
		if err != nil {
			return 0, err
		}
		if _, err := q.Exec(ctx, `
			UPDATE build_attempts
			SET state = 'expired', failure_class = 'lease_expired',
			    failure_detail = $2, finished_at = $3, updated_at = $3
			WHERE id = $1
		`, row.attemptID, reason, now); err != nil {
			return 0, err
		}
		if _, err := q.Exec(ctx, `DELETE FROM worker_leases WHERE id = $1`, row.leaseID); err != nil {
			return 0, err
		}
		var completedAt any
		if state == "failed" || state == "canceled" {
			completedAt = now
		}
		if _, err := q.Exec(ctx, `
			UPDATE build_jobs
			SET state = $2, status_snapshot = $3::jsonb, status_digest = $4,
			    terminal_reason = $5, updated_at = $6, completed_at = $7,
			    next_attempt_at = clock_timestamp(), ledger_revision = ledger_revision + 1
			WHERE id = $1
		`, row.jobID, state, string(snapshot), digest, reason, now, completedAt); err != nil {
			return 0, err
		}
		if err := r.insertJobEvent(ctx, q, row.jobID.String(), "job.lease_expired", map[string]any{
			"attempt_id": row.attemptID, "attempt_no": row.attemptNo, "next_state": state,
			"reason": reason,
		}); err != nil {
			return 0, err
		}
	}
	return len(expired), nil
}

// RenewClaim extends only the exact active attempt/fence owned by this slot.
func (r *JobRepository) RenewClaim(ctx context.Context, status *builder.BuildStatus, leaseDuration time.Duration) error {
	if status == nil || status.AttemptID == "" || status.FenceToken <= 0 || status.LeaseOwner == "" {
		return fmt.Errorf("missing durable claim identity")
	}
	tag, err := r.db.pool.Exec(ctx, `
		UPDATE worker_leases l
		SET expires_at = now() + $5::interval, renewed_at = now()
		FROM build_attempts a, workers w
		WHERE l.attempt_id = a.id
		  AND l.worker_id = w.id
		  AND a.id = $1 AND a.job_id = $2
		  AND l.fence_token = $3 AND w.stable_name = $4
		  AND l.expires_at > clock_timestamp()
	`, status.AttemptID, status.JobID, status.FenceToken, status.LeaseOwner,
		leaseDuration.String())
	if err != nil {
		r.recordWrite(err)
		return fmt.Errorf("renew durable claim: %w", err)
	}
	if tag.RowsAffected() != 1 {
		err := fmt.Errorf("durable claim is stale, expired, or no longer owned")
		r.recordWrite(err)
		return err
	}
	r.recordWrite(nil)
	return nil
}

// CheckClaim is the final external-side-effect fence.
func (r *JobRepository) CheckClaim(ctx context.Context, status *builder.BuildStatus) error {
	var valid bool
	err := r.db.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM worker_leases l
			JOIN build_attempts a ON a.id = l.attempt_id
			JOIN workers w ON w.id = l.worker_id
			JOIN build_jobs j ON j.id = a.job_id
			WHERE a.id = $1 AND a.job_id = $2
			  AND l.fence_token = $3 AND w.stable_name = $4
			  AND l.expires_at > clock_timestamp()
			  AND j.state NOT IN ('completed', 'success', 'failed', 'canceled')
		)
	`, status.AttemptID, status.JobID, status.FenceToken, status.LeaseOwner).Scan(&valid)
	if err != nil {
		return fmt.Errorf("check durable claim: %w", err)
	}
	if !valid {
		return fmt.Errorf("durable claim fence is not active")
	}
	return nil
}

// LoadVisible returns the PostgreSQL job projection with any active claim
// identity reattached for the owning executor.
func (r *JobRepository) LoadVisible(ctx context.Context) (map[string]*builder.BuildStatus, error) {
	rows, err := r.db.pool.Query(ctx, `
		SELECT j.id::text, j.request, j.status_snapshot,
		       COALESCE(a.id::text, ''), COALESCE(a.fence_token, 0),
		       COALESCE(w.stable_name, '')
		FROM build_jobs j
		LEFT JOIN LATERAL (
			SELECT a.id, a.fence_token, a.worker_id
			FROM build_attempts a
			JOIN worker_leases l ON l.attempt_id = a.id AND l.expires_at > clock_timestamp()
			WHERE a.job_id = j.id
			ORDER BY a.attempt_no DESC
			LIMIT 1
		) a ON true
		LEFT JOIN workers w ON w.id = a.worker_id
		WHERE j.legacy_visible = true
		ORDER BY j.created_at, j.id
	`)
	if err != nil {
		return nil, fmt.Errorf("load durable job projection: %w", err)
	}
	defer rows.Close()
	result := make(map[string]*builder.BuildStatus)
	for rows.Next() {
		var id, attemptID, owner string
		var requestJSON, statusJSON []byte
		var fence int64
		if err := rows.Scan(&id, &requestJSON, &statusJSON, &attemptID, &fence, &owner); err != nil {
			return nil, err
		}
		request, err := decodeStoredRequest(requestJSON)
		if err != nil {
			return nil, err
		}
		status, err := decodeStoredStatus(statusJSON, request)
		if err != nil {
			return nil, err
		}
		status.JobID = id
		status.AttemptID, status.FenceToken, status.LeaseOwner = attemptID, fence, owner
		result[id] = status
	}
	return result, rows.Err()
}

func (r *JobRepository) CancelJob(ctx context.Context, jobID, reason string) (*builder.BuildStatus, error) {
	var result *builder.BuildStatus
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		var state string
		var statusJSON []byte
		if err := q.QueryRow(ctx, `
			SELECT state, status_snapshot FROM build_jobs
			WHERE id = $1 AND legacy_visible = true
			FOR UPDATE
		`, jobID).Scan(&state, &statusJSON); err != nil {
			return err
		}
		if state == "completed" || state == "success" || state == "failed" || state == "canceled" {
			return fmt.Errorf("job %s is already terminal (%s)", jobID, state)
		}
		status, err := decodeStoredStatus(statusJSON, nil)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		status.Status, status.Error, status.UpdatedAt = "canceled", reason, now
		status.AttemptID, status.FenceToken, status.LeaseOwner = "", 0, ""
		snapshot, digest, err := statusDocument(status)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			UPDATE build_jobs
			SET state = 'canceled', cancel_requested_at = $2, terminal_reason = $3,
			    completed_at = $2, updated_at = $2, status_snapshot = $4::jsonb,
			    status_digest = $5, ledger_revision = ledger_revision + 1
			WHERE id = $1
		`, jobID, now, reason, string(snapshot), digest); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			UPDATE build_attempts SET state = 'canceled', failure_class = 'canceled',
			    failure_detail = $2, finished_at = $3, updated_at = $3
			WHERE job_id = $1 AND finished_at IS NULL
		`, jobID, reason, now); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			DELETE FROM worker_leases WHERE attempt_id IN (
				SELECT id FROM build_attempts WHERE job_id = $1
			)
		`, jobID); err != nil {
			return err
		}
		if err := r.insertJobEvent(ctx, q, jobID, "job.canceled", map[string]any{"reason": reason}); err != nil {
			return err
		}
		result = status
		return nil
	})
	r.recordWrite(err)
	return result, err
}

func (r *JobRepository) RetryJob(ctx context.Context, jobID string) (*builder.BuildStatus, error) {
	var result *builder.BuildStatus
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		var state string
		var requestJSON, statusJSON []byte
		var attempts, maxAttempts int
		if err := q.QueryRow(ctx, `
			SELECT state, request, status_snapshot,
			       (SELECT count(*) FROM build_attempts WHERE job_id = build_jobs.id),
			       max_attempts
			FROM build_jobs
			WHERE id = $1 AND legacy_visible = true
			FOR UPDATE
		`, jobID).Scan(&state, &requestJSON, &statusJSON, &attempts, &maxAttempts); err != nil {
			return err
		}
		if state != "failed" && state != "canceled" {
			return fmt.Errorf("job %s is %s; only failed or canceled jobs can retry", jobID, state)
		}
		if attempts >= maxAttempts {
			return fmt.Errorf("job %s exhausted %d attempts", jobID, maxAttempts)
		}
		request, err := decodeStoredRequest(requestJSON)
		if err != nil {
			return err
		}
		status, err := decodeStoredStatus(statusJSON, request)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		status.Status, status.Error, status.FailedStage, status.UpdatedAt = "queued", "", "", now
		status.AttemptID, status.FenceToken, status.LeaseOwner = "", 0, ""
		snapshot, digest, err := statusDocument(status)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			UPDATE build_jobs
			SET state = 'queued', cancel_requested_at = NULL, terminal_reason = '',
			    completed_at = NULL, next_attempt_at = clock_timestamp(), updated_at = $2,
			    status_snapshot = $3::jsonb, status_digest = $4,
			    ledger_revision = ledger_revision + 1
			WHERE id = $1
		`, jobID, now, string(snapshot), digest); err != nil {
			return err
		}
		if err := r.insertJobEvent(ctx, q, jobID, "job.retry_queued", map[string]any{
			"previous_state": state, "next_attempt": attempts + 1,
		}); err != nil {
			return err
		}
		result = status
		return nil
	})
	r.recordWrite(err)
	return result, err
}

// RuntimeStatus returns scheduler health directly from PostgreSQL. Queue and
// lease counts must not be inferred from one process's in-memory projection
// once more than one control-plane replica is running.
func (r *JobRepository) RuntimeStatus(ctx context.Context) (builder.SchedulerRuntimeStatus, error) {
	status := builder.SchedulerRuntimeStatus{Authority: "postgresql"}
	err := r.db.Pool().QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE j.state = 'queued'),
			count(*) FILTER (
				WHERE j.state IN (
					'claimed','provisioning','forwarding','deploying','building',
					'collecting','verifying','signing','publishing'
				)
			),
			min(j.created_at) FILTER (WHERE j.state = 'queued'),
			(SELECT count(*) FROM worker_leases l WHERE l.expires_at > clock_timestamp()),
			(SELECT count(*) FROM worker_leases l WHERE l.expires_at <= clock_timestamp()),
			(SELECT min(l.expires_at) FROM worker_leases l WHERE l.expires_at > clock_timestamp()),
			(SELECT count(*) FROM workers),
			(SELECT count(*) FROM workers w WHERE w.desired_state = 'active' AND w.last_seen_at > clock_timestamp() - interval '45 seconds'),
			(SELECT count(*) FROM workers w WHERE w.last_seen_at IS NULL OR w.last_seen_at <= clock_timestamp() - interval '45 seconds'),
			(SELECT count(*) FROM build_attempts a WHERE a.created_at > now() - interval '1 hour')
		FROM build_jobs j
		WHERE j.legacy_visible = true
	`).Scan(
		&status.QueuedTasks,
		&status.RunningTasks,
		&status.OldestQueuedAt,
		&status.ActiveLeases,
		&status.ExpiredLeases,
		&status.OldestLeaseExpires,
		&status.RegisteredWorkers,
		&status.ActiveWorkers,
		&status.StaleWorkers,
		&status.AttemptsLastHour,
	)
	if err != nil {
		return status, fmt.Errorf("read scheduler runtime status: %w", err)
	}
	return status, nil
}
