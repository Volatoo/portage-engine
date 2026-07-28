package persistence

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/workergateway"
)

const (
	maxGatewayErrorBytes       = 64 * 1024
	maxGatewayDestinationBytes = 16 * 1024
)

func revokeWorkerGatewayAttempt(
	ctx context.Context,
	q Querier,
	attemptID, reason string,
	revokedAt time.Time,
) error {
	if _, err := q.Exec(ctx, `
		UPDATE workload_certificates
		SET state = 'revoked', revoked_at = $3, revoke_reason = $2
		WHERE attempt_id = $1 AND state = 'active'
	`, attemptID, reason, revokedAt); err != nil {
		return fmt.Errorf("revoke attempt workload certificates: %w", err)
	}
	if _, err := q.Exec(ctx, `
		UPDATE worker_gateway_sessions
		SET state = 'revoked', revoked_at = $3, revoke_reason = $2,
		    updated_at = $3
		WHERE attempt_id = $1 AND state = 'active'
	`, attemptID, reason, revokedAt); err != nil {
		return fmt.Errorf("revoke attempt worker gateway session: %w", err)
	}
	if _, err := q.Exec(ctx, `
		UPDATE worker_gateway_commands
		SET state = 'canceled', completed_at = $3,
		    delivery_lease_expires_at = NULL,
		    error = CASE WHEN error = '' THEN $2 ELSE error END,
		    updated_at = $3
		WHERE attempt_id = $1 AND state IN ('queued', 'delivered')
	`, attemptID, reason, revokedAt); err != nil {
		return fmt.Errorf("cancel attempt worker gateway commands: %w", err)
	}
	if _, err := q.Exec(ctx, `
		UPDATE worker_gateway_uploads
		SET state = 'canceled', upload_lease_expires_at = NULL,
		    updated_at = $2
		WHERE attempt_id = $1 AND state IN ('ready', 'uploading')
	`, attemptID, revokedAt); err != nil {
		return fmt.Errorf("cancel attempt worker gateway uploads: %w", err)
	}
	return nil
}

func revokeWorkerGatewayJob(
	ctx context.Context,
	q Querier,
	jobID, reason string,
	revokedAt time.Time,
) error {
	rows, err := q.Query(ctx, `
		SELECT id::text FROM build_attempts WHERE job_id = $1
	`, jobID)
	if err != nil {
		return fmt.Errorf("list job attempts for gateway revocation: %w", err)
	}
	var attempts []string
	for rows.Next() {
		var attemptID string
		if err := rows.Scan(&attemptID); err != nil {
			rows.Close()
			return err
		}
		attempts = append(attempts, attemptID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, attemptID := range attempts {
		if err := revokeWorkerGatewayAttempt(
			ctx, q, attemptID, reason, revokedAt,
		); err != nil {
			return err
		}
	}
	return nil
}

// RegisterWorkerSession durably binds an ephemeral certificate identity to
// the exact live scheduler attempt. Re-registering the same identity after a
// control-plane restart is idempotent; no other identity may take its row.
func (r *JobRepository) RegisterWorkerSession(
	ctx context.Context,
	identity workergateway.Identity,
) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	tag, err := r.db.pool.Exec(ctx, `
		INSERT INTO worker_gateway_sessions (
			worker_id, project_id, job_id, attempt_id, attempt_fence,
			state, created_at, updated_at
		)
		SELECT $1, j.project_id, j.id, a.id, $4, 'active',
		       clock_timestamp(), clock_timestamp()
		FROM build_attempts a
		JOIN build_jobs j ON j.id = a.job_id
		JOIN worker_leases l ON l.attempt_id = a.id
		WHERE j.id = $2 AND a.id = $3
		  AND a.fence_token = $4 AND l.fence_token = $4
		  AND l.expires_at > clock_timestamp()
		  AND j.state NOT IN ('completed', 'success', 'failed', 'canceled')
		ON CONFLICT (worker_id) DO UPDATE
		SET state = 'active', revoked_at = NULL, revoke_reason = '',
		    updated_at = clock_timestamp()
		WHERE worker_gateway_sessions.job_id = EXCLUDED.job_id
		  AND worker_gateway_sessions.attempt_id = EXCLUDED.attempt_id
		  AND worker_gateway_sessions.attempt_fence = EXCLUDED.attempt_fence
	`, identity.WorkerID, identity.JobID, identity.AttemptID, identity.FenceToken)
	if err != nil {
		return fmt.Errorf("register durable worker session: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("worker session identity is stale or conflicts with an existing session")
	}
	return nil
}

// RegisterWorkerCertificate binds one public leaf record to the already
// registered attempt. A newly observed CA fingerprint becomes the active
// generation and the previous active generation starts draining. Revoked or
// draining generations can never silently become active again.
func (r *JobRepository) RegisterWorkerCertificate(
	ctx context.Context,
	identity workergateway.Identity,
	record workergateway.CertificateRecord,
) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	if err := record.Validate(); err != nil {
		return err
	}
	return r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if _, err := q.Exec(ctx, `
			UPDATE workload_issuer_generations
			SET state = 'draining'
			WHERE issuer_id = $1 AND fingerprint <> $2 AND state = 'active'
		`, record.IssuerID, record.IssuerFingerprint); err != nil {
			return fmt.Errorf("drain previous workload issuer generation: %w", err)
		}
		var issuerFingerprint string
		err := q.QueryRow(ctx, `
			INSERT INTO workload_issuer_generations (
				fingerprint, issuer_id, provider, subject, serial_hex,
				not_before, not_after, state, first_seen_at, last_issued_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'active',
			        clock_timestamp(), clock_timestamp())
			ON CONFLICT (fingerprint) DO UPDATE
			SET last_issued_at = clock_timestamp()
			WHERE workload_issuer_generations.issuer_id = EXCLUDED.issuer_id
			  AND workload_issuer_generations.provider = EXCLUDED.provider
			  AND workload_issuer_generations.subject = EXCLUDED.subject
			  AND workload_issuer_generations.serial_hex = EXCLUDED.serial_hex
			  AND workload_issuer_generations.not_before = EXCLUDED.not_before
			  AND workload_issuer_generations.not_after = EXCLUDED.not_after
			  AND workload_issuer_generations.state = 'active'
			RETURNING fingerprint
		`, record.IssuerFingerprint, record.IssuerID, record.IssuerProvider,
			record.IssuerSubject, record.IssuerSerial, record.IssuerNotBefore,
			record.IssuerNotAfter).Scan(&issuerFingerprint)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("workload issuer generation is draining, revoked, or conflicts with immutable metadata")
		}
		if err != nil {
			return fmt.Errorf("register workload issuer generation: %w", err)
		}
		var certificateFingerprint string
		err = q.QueryRow(ctx, `
			INSERT INTO workload_certificates (
				fingerprint, serial_hex, issuer_fingerprint, worker_id,
				job_id, attempt_id, attempt_fence, not_before, not_after,
				state, issued_at
			)
			SELECT $5, $6, $7, s.worker_id, s.job_id, s.attempt_id,
			       s.attempt_fence, $8, $9, 'active', clock_timestamp()
			FROM worker_gateway_sessions s
			WHERE s.worker_id = $1 AND s.job_id = $2 AND s.attempt_id = $3
			  AND s.attempt_fence = $4 AND s.state = 'active'
			ON CONFLICT (fingerprint) DO UPDATE
			SET fingerprint = workload_certificates.fingerprint
			WHERE workload_certificates.serial_hex = EXCLUDED.serial_hex
			  AND workload_certificates.issuer_fingerprint = EXCLUDED.issuer_fingerprint
			  AND workload_certificates.worker_id = EXCLUDED.worker_id
			  AND workload_certificates.job_id = EXCLUDED.job_id
			  AND workload_certificates.attempt_id = EXCLUDED.attempt_id
			  AND workload_certificates.attempt_fence = EXCLUDED.attempt_fence
			  AND workload_certificates.not_before = EXCLUDED.not_before
			  AND workload_certificates.not_after = EXCLUDED.not_after
			  AND workload_certificates.state = 'active'
			RETURNING fingerprint
		`, identity.WorkerID, identity.JobID, identity.AttemptID,
			identity.FenceToken, record.Fingerprint, record.Serial,
			issuerFingerprint, record.NotBefore, record.NotAfter,
		).Scan(&certificateFingerprint)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("workload certificate conflicts with its session or immutable metadata")
		}
		if err != nil {
			return fmt.Errorf("register workload certificate: %w", err)
		}
		tag, err := q.Exec(ctx, `
			UPDATE worker_gateway_sessions
			SET certificate_fingerprint = $5, updated_at = clock_timestamp()
			WHERE worker_id = $1 AND job_id = $2 AND attempt_id = $3
			  AND attempt_fence = $4 AND state = 'active'
			  AND (
			    certificate_fingerprint IS NULL
			    OR certificate_fingerprint = $5
			  )
		`, identity.WorkerID, identity.JobID, identity.AttemptID,
			identity.FenceToken, certificateFingerprint)
		if err != nil {
			return fmt.Errorf("bind workload certificate to session: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("worker session already has a different certificate")
		}
		return nil
	})
}

// AuthorizeWorkerCertificate is called for every mTLS request after Go's TLS
// verifier has validated the chain. It applies durable leaf and issuer
// revocation, expiry, session binding, and immutable attempt identity.
func (r *JobRepository) AuthorizeWorkerCertificate(
	ctx context.Context,
	identity workergateway.Identity,
	presented workergateway.PresentedCertificate,
) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	if len(presented.Fingerprint) != 64 ||
		presented.Fingerprint != strings.ToLower(presented.Fingerprint) {
		return fmt.Errorf("invalid presented certificate fingerprint")
	}
	if _, err := hex.DecodeString(presented.Fingerprint); err != nil ||
		presented.Serial == "" {
		return fmt.Errorf("invalid presented certificate metadata")
	}
	tag, err := r.db.pool.Exec(ctx, `
		UPDATE workload_certificates c
		SET first_seen_at = COALESCE(c.first_seen_at, clock_timestamp()),
		    last_seen_at = clock_timestamp()
		FROM workload_issuer_generations i, worker_gateway_sessions s
		WHERE c.fingerprint = $5 AND c.serial_hex = $6
		  AND c.worker_id = $1 AND c.job_id = $2 AND c.attempt_id = $3
		  AND c.attempt_fence = $4 AND c.state = 'active'
		  AND clock_timestamp() BETWEEN c.not_before AND c.not_after
		  AND i.fingerprint = c.issuer_fingerprint
		  AND i.state IN ('active', 'draining')
		  AND clock_timestamp() BETWEEN i.not_before AND i.not_after
		  AND s.worker_id = c.worker_id
		  AND s.certificate_fingerprint = c.fingerprint
		  AND s.state = 'active'
	`, identity.WorkerID, identity.JobID, identity.AttemptID,
		identity.FenceToken, presented.Fingerprint,
		strings.ToLower(presented.Serial))
	if err != nil {
		return fmt.Errorf("authorize workload certificate: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("certificate is unknown, expired, revoked, or not bound to this attempt")
	}
	return nil
}

func (r *JobRepository) RevokeWorkerSession(
	ctx context.Context,
	identity workergateway.Identity,
	reason string,
) error {
	if len(reason) > maxGatewayErrorBytes {
		reason = reason[:maxGatewayErrorBytes]
	}
	return r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		tag, err := q.Exec(ctx, `
			UPDATE worker_gateway_sessions
			SET state = 'revoked', revoked_at = clock_timestamp(),
			    revoke_reason = $5, updated_at = clock_timestamp()
			WHERE worker_id = $1 AND job_id = $2 AND attempt_id = $3
			  AND attempt_fence = $4 AND state = 'active'
		`, identity.WorkerID, identity.JobID, identity.AttemptID,
			identity.FenceToken, reason)
		if err != nil {
			return fmt.Errorf("revoke durable worker session: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		if _, err := q.Exec(ctx, `
			UPDATE workload_certificates
			SET state = 'revoked', revoked_at = clock_timestamp(),
			    revoke_reason = $2
			WHERE worker_id = $1 AND state = 'active'
		`, identity.WorkerID, reason); err != nil {
			return fmt.Errorf("revoke worker certificate: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE worker_gateway_commands
			SET state = 'canceled', completed_at = clock_timestamp(),
			    delivery_lease_expires_at = NULL,
			    error = CASE WHEN error = '' THEN $2 ELSE error END,
			    updated_at = clock_timestamp()
			WHERE worker_id = $1 AND state IN ('queued', 'delivered')
		`, identity.WorkerID, reason); err != nil {
			return fmt.Errorf("cancel durable worker commands: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE worker_gateway_uploads
			SET state = 'canceled', upload_lease_expires_at = NULL,
			    updated_at = clock_timestamp()
			WHERE worker_id = $1 AND state IN ('ready', 'uploading')
		`, identity.WorkerID); err != nil {
			return fmt.Errorf("cancel durable worker uploads: %w", err)
		}
		return nil
	})
}

func (r *JobRepository) TouchWorkerSession(
	ctx context.Context,
	identity workergateway.Identity,
) error {
	tag, err := r.db.pool.Exec(ctx, `
		UPDATE worker_gateway_sessions s
		SET connected_at = COALESCE(connected_at, clock_timestamp()),
		    last_seen_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE worker_id = $1 AND job_id = $2 AND attempt_id = $3
		  AND attempt_fence = $4 AND state = 'active'
		  AND EXISTS (
		      SELECT 1
		      FROM worker_leases l
		      JOIN build_attempts a ON a.id = l.attempt_id
		      JOIN build_jobs j ON j.id = a.job_id
		      WHERE a.id = s.attempt_id AND j.id = s.job_id
		        AND a.fence_token = s.attempt_fence
		        AND l.fence_token = s.attempt_fence
		        AND l.expires_at > clock_timestamp()
		        AND j.state NOT IN ('completed', 'success', 'failed', 'canceled')
		  )
	`, identity.WorkerID, identity.JobID, identity.AttemptID, identity.FenceToken)
	if err != nil {
		return fmt.Errorf("touch durable worker session: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("durable worker session is stale or revoked")
	}
	return nil
}

func (r *JobRepository) WorkerSessionConnected(
	ctx context.Context,
	identity workergateway.Identity,
) (bool, error) {
	var connected bool
	err := r.db.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM worker_gateway_sessions
			WHERE worker_id = $1 AND job_id = $2 AND attempt_id = $3
			  AND attempt_fence = $4 AND state = 'active'
			  AND connected_at IS NOT NULL
		)
	`, identity.WorkerID, identity.JobID, identity.AttemptID,
		identity.FenceToken).Scan(&connected)
	if err != nil {
		return false, fmt.Errorf("read durable worker session: %w", err)
	}
	return connected, nil
}

func (r *JobRepository) EnqueueWorkerCommand(
	ctx context.Context,
	identity workergateway.Identity,
	task workergateway.Task,
) error {
	if _, err := uuid.Parse(task.ID); err != nil {
		return fmt.Errorf("invalid worker command id: %w", err)
	}
	if task.Action != workergateway.ActionBuild &&
		task.Action != workergateway.ActionVerify &&
		task.Action != workergateway.ActionCollect {
		return fmt.Errorf("unsupported worker action %q", task.Action)
	}
	if !json.Valid(task.Payload) {
		return fmt.Errorf("worker command request is not valid JSON")
	}
	tag, err := r.db.pool.Exec(ctx, `
		INSERT INTO worker_gateway_commands (
			id, worker_id, project_id, job_id, attempt_id, action, request,
			state, created_at, updated_at
		)
		SELECT $5, s.worker_id, s.project_id, s.job_id, s.attempt_id,
		       $6, $7::jsonb, 'queued', clock_timestamp(), clock_timestamp()
		FROM worker_gateway_sessions s
		WHERE s.worker_id = $1 AND s.job_id = $2 AND s.attempt_id = $3
		  AND s.attempt_fence = $4 AND s.state = 'active'
		  AND EXISTS (
		      SELECT 1 FROM worker_leases l
		      WHERE l.attempt_id = s.attempt_id
		        AND l.fence_token = s.attempt_fence
		        AND l.expires_at > clock_timestamp()
		  )
		ON CONFLICT (id) DO UPDATE
		SET updated_at = worker_gateway_commands.updated_at
		WHERE worker_gateway_commands.worker_id = EXCLUDED.worker_id
		  AND worker_gateway_commands.job_id = EXCLUDED.job_id
		  AND worker_gateway_commands.attempt_id = EXCLUDED.attempt_id
		  AND worker_gateway_commands.action = EXCLUDED.action
		  AND worker_gateway_commands.request = EXCLUDED.request
	`, identity.WorkerID, identity.JobID, identity.AttemptID,
		identity.FenceToken, task.ID, task.Action, string(task.Payload))
	if err != nil {
		return fmt.Errorf("enqueue durable worker command: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("durable worker session is stale or command id conflicts with another immutable request")
	}
	return nil
}

func (r *JobRepository) ClaimWorkerCommand(
	ctx context.Context,
	identity workergateway.Identity,
	lease time.Duration,
) (*workergateway.Task, error) {
	if lease <= 0 {
		return nil, fmt.Errorf("worker command lease must be positive")
	}
	var task *workergateway.Task
	noWork := false
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := touchWorkerSessionTx(ctx, q, identity); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			UPDATE worker_gateway_commands
			SET state = 'queued', delivery_lease_expires_at = NULL,
			    updated_at = clock_timestamp()
			WHERE worker_id = $1 AND attempt_id = $2
			  AND state = 'delivered'
			  AND delivery_lease_expires_at <= clock_timestamp()
		`, identity.WorkerID, identity.AttemptID); err != nil {
			return fmt.Errorf("reclaim expired worker command: %w", err)
		}
		var result workergateway.Task
		var payload []byte
		err := q.QueryRow(ctx, `
			WITH candidate AS (
			    SELECT id
			    FROM worker_gateway_commands
			    WHERE worker_id = $1 AND attempt_id = $2 AND state = 'queued'
			    ORDER BY created_at, id
			    FOR UPDATE SKIP LOCKED
			    LIMIT 1
			)
			UPDATE worker_gateway_commands c
			SET state = 'delivered',
			    delivery_fence = c.delivery_fence + 1,
			    delivery_lease_expires_at = clock_timestamp() + $3::interval,
			    delivered_at = clock_timestamp(),
			    updated_at = clock_timestamp()
			FROM candidate
			WHERE c.id = candidate.id
			RETURNING c.id::text, c.action, c.delivery_fence, c.request
		`, identity.WorkerID, identity.AttemptID, lease.String()).Scan(
			&result.ID, &result.Action, &result.DeliveryFence, &payload,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			// This is a successful empty poll. Commit the session heartbeat so
			// a provision phase waiting for the first connection can observe
			// the worker before any command exists.
			noWork = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim durable worker command: %w", err)
		}
		result.Payload = append([]byte(nil), payload...)
		task = &result
		return nil
	})
	if err == nil && noWork {
		return nil, workergateway.ErrNoWork
	}
	return task, err
}

func touchWorkerSessionTx(
	ctx context.Context,
	q Querier,
	identity workergateway.Identity,
) error {
	tag, err := q.Exec(ctx, `
		UPDATE worker_gateway_sessions s
		SET connected_at = COALESCE(connected_at, clock_timestamp()),
		    last_seen_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE worker_id = $1 AND job_id = $2 AND attempt_id = $3
		  AND attempt_fence = $4 AND state = 'active'
		  AND EXISTS (
		      SELECT 1 FROM worker_leases l
		      WHERE l.attempt_id = s.attempt_id
		        AND l.fence_token = s.attempt_fence
		        AND l.expires_at > clock_timestamp()
		  )
	`, identity.WorkerID, identity.JobID, identity.AttemptID, identity.FenceToken)
	if err != nil {
		return fmt.Errorf("touch durable worker session: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("durable worker session is stale or revoked")
	}
	return nil
}

func (r *JobRepository) CompleteWorkerCommand(
	ctx context.Context,
	identity workergateway.Identity,
	completion workergateway.Completion,
) error {
	if completion.DeliveryFence <= 0 {
		return fmt.Errorf("worker completion requires a positive delivery fence")
	}
	if len(completion.Error) > maxGatewayErrorBytes {
		return fmt.Errorf("worker completion error exceeds %d bytes", maxGatewayErrorBytes)
	}
	if len(completion.Payload) > 0 && !json.Valid(completion.Payload) {
		return fmt.Errorf("worker completion payload is not valid JSON")
	}
	state := "completed"
	if completion.Error != "" {
		state = "failed"
	}
	var response any
	if len(completion.Payload) > 0 {
		response = string(completion.Payload)
	}
	tag, err := r.db.pool.Exec(ctx, `
		UPDATE worker_gateway_commands c
		SET state = $6, response = $7::jsonb, error = $8,
		    delivery_lease_expires_at = NULL,
		    completed_at = COALESCE(c.completed_at, clock_timestamp()),
		    updated_at = clock_timestamp()
		FROM worker_gateway_sessions s
		WHERE c.worker_id = s.worker_id
		  AND c.id = $5 AND c.worker_id = $1
		  AND c.job_id = $2 AND c.attempt_id = $3
		  AND s.attempt_fence = $4 AND s.state = 'active'
		  AND c.delivery_fence = $9
		  AND (
		    c.state = 'delivered'
		    OR (
		      c.state = $6 AND c.error = $8
		      AND c.response IS NOT DISTINCT FROM $7::jsonb
		    )
		  )
		  AND EXISTS (
		      SELECT 1
		      FROM worker_leases l
		      JOIN build_jobs j ON j.id = c.job_id
		      WHERE l.attempt_id = c.attempt_id
		        AND l.fence_token = s.attempt_fence
		        AND l.expires_at > clock_timestamp()
		        AND j.state NOT IN ('completed', 'success', 'failed', 'canceled')
		  )
	`, identity.WorkerID, identity.JobID, identity.AttemptID,
		identity.FenceToken, completion.TaskID, state, response,
		completion.Error, completion.DeliveryFence)
	if err != nil {
		return fmt.Errorf("complete durable worker command: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return workergateway.ErrStaleFence
	}
	return nil
}

func (r *JobRepository) WorkerCommandResult(
	ctx context.Context,
	identity workergateway.Identity,
	taskID string,
) (*workergateway.Completion, error) {
	var state, commandError string
	var response []byte
	var fence int64
	err := r.db.pool.QueryRow(ctx, `
		SELECT c.state, c.delivery_fence, c.response, c.error
		FROM worker_gateway_commands c
		JOIN worker_gateway_sessions s ON s.worker_id = c.worker_id
		WHERE c.id = $5 AND c.worker_id = $1
		  AND c.job_id = $2 AND c.attempt_id = $3
		  AND s.attempt_fence = $4
	`, identity.WorkerID, identity.JobID, identity.AttemptID,
		identity.FenceToken, taskID).Scan(&state, &fence, &response, &commandError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("durable worker command is unknown")
	}
	if err != nil {
		return nil, fmt.Errorf("read durable worker command result: %w", err)
	}
	if state != "completed" && state != "failed" && state != "canceled" {
		return nil, workergateway.ErrResultPending
	}
	return &workergateway.Completion{
		TaskID: taskID, DeliveryFence: fence,
		Payload: append([]byte(nil), response...), Error: commandError,
	}, nil
}

func (r *JobRepository) PrepareWorkerUpload(
	ctx context.Context,
	identity workergateway.Identity,
	uploadID, destination string,
	maxBytes int64,
) error {
	if _, err := uuid.Parse(uploadID); err != nil {
		return fmt.Errorf("invalid worker upload id: %w", err)
	}
	if destination == "" || len(destination) > maxGatewayDestinationBytes || maxBytes <= 0 {
		return fmt.Errorf("invalid durable worker upload capability")
	}
	tag, err := r.db.pool.Exec(ctx, `
		INSERT INTO worker_gateway_uploads (
			id, worker_id, project_id, job_id, attempt_id,
			destination, max_bytes, state, created_at, updated_at
		)
		SELECT $5, s.worker_id, s.project_id, s.job_id, s.attempt_id,
		       $6, $7, 'ready', clock_timestamp(), clock_timestamp()
		FROM worker_gateway_sessions s
		WHERE s.worker_id = $1 AND s.job_id = $2 AND s.attempt_id = $3
		  AND s.attempt_fence = $4 AND s.state = 'active'
		ON CONFLICT (id) DO UPDATE
		SET updated_at = worker_gateway_uploads.updated_at
		WHERE worker_gateway_uploads.worker_id = EXCLUDED.worker_id
		  AND worker_gateway_uploads.job_id = EXCLUDED.job_id
		  AND worker_gateway_uploads.attempt_id = EXCLUDED.attempt_id
		  AND worker_gateway_uploads.destination = EXCLUDED.destination
		  AND worker_gateway_uploads.max_bytes = EXCLUDED.max_bytes
	`, identity.WorkerID, identity.JobID, identity.AttemptID,
		identity.FenceToken, uploadID, destination, maxBytes)
	if err != nil {
		return fmt.Errorf("prepare durable worker upload: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("durable worker session is stale")
	}
	return nil
}

func (r *JobRepository) ClaimWorkerUpload(
	ctx context.Context,
	identity workergateway.Identity,
	uploadID string,
	lease time.Duration,
) (workergateway.UploadClaim, error) {
	var claim workergateway.UploadClaim
	if lease <= 0 {
		return claim, fmt.Errorf("worker upload lease must be positive")
	}
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := touchWorkerSessionTx(ctx, q, identity); err != nil {
			return err
		}
		var state, destination, digest string
		var maxBytes, fence int64
		var size *int64
		var expires *time.Time
		var leaseActive bool
		err := q.QueryRow(ctx, `
			SELECT state, destination, max_bytes, upload_fence,
			       upload_lease_expires_at, digest, size_bytes,
			       COALESCE(upload_lease_expires_at > clock_timestamp(), false)
			FROM worker_gateway_uploads
			WHERE id = $5 AND worker_id = $1 AND job_id = $2
			  AND attempt_id = $3
			  AND EXISTS (
			      SELECT 1 FROM worker_gateway_sessions s
			      WHERE s.worker_id = $1 AND s.attempt_fence = $4
			  )
			FOR UPDATE
		`, identity.WorkerID, identity.JobID, identity.AttemptID,
			identity.FenceToken, uploadID).Scan(
			&state, &destination, &maxBytes, &fence, &expires, &digest, &size,
			&leaseActive,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("durable worker upload is unknown")
		}
		if err != nil {
			return fmt.Errorf("lock durable worker upload: %w", err)
		}
		claim = workergateway.UploadClaim{
			ID: uploadID, Destination: destination, MaxBytes: maxBytes,
			Fence: fence, Digest: digest,
		}
		if size != nil {
			claim.Size = *size
		}
		if state == "completed" {
			claim.Completed = true
			return nil
		}
		if state == "canceled" {
			return workergateway.ErrStaleFence
		}
		if state == "uploading" && leaseActive {
			return workergateway.ErrStaleFence
		}
		err = q.QueryRow(ctx, `
			UPDATE worker_gateway_uploads
			SET state = 'uploading', upload_fence = upload_fence + 1,
			    upload_lease_expires_at = clock_timestamp() + $2::interval,
			    updated_at = clock_timestamp()
			WHERE id = $1
			RETURNING upload_fence
		`, uploadID, lease.String()).Scan(&claim.Fence)
		return err
	})
	return claim, err
}

func (r *JobRepository) CompleteWorkerUpload(
	ctx context.Context,
	identity workergateway.Identity,
	uploadID string,
	fence int64,
	digest string,
	size int64,
) error {
	digest = strings.ToLower(strings.TrimSpace(digest))
	if len(digest) != 64 || size < 0 {
		return fmt.Errorf("invalid worker upload result")
	}
	tag, err := r.db.pool.Exec(ctx, `
		UPDATE worker_gateway_uploads u
		SET state = 'completed', digest = $7, size_bytes = $8,
		    upload_lease_expires_at = NULL,
		    completed_at = clock_timestamp(), updated_at = clock_timestamp()
		FROM worker_gateway_sessions s
		WHERE u.worker_id = s.worker_id
		  AND u.id = $5 AND u.worker_id = $1
		  AND u.job_id = $2 AND u.attempt_id = $3
		  AND s.attempt_fence = $4 AND s.state = 'active'
		  AND u.state = 'uploading' AND u.upload_fence = $6
		  AND EXISTS (
		      SELECT 1
		      FROM worker_leases l
		      JOIN build_jobs j ON j.id = u.job_id
		      WHERE l.attempt_id = u.attempt_id
		        AND l.fence_token = s.attempt_fence
		        AND l.expires_at > clock_timestamp()
		        AND j.state NOT IN ('completed', 'success', 'failed', 'canceled')
		  )
	`, identity.WorkerID, identity.JobID, identity.AttemptID,
		identity.FenceToken, uploadID, fence, digest, size)
	if err != nil {
		return fmt.Errorf("complete durable worker upload: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return workergateway.ErrStaleFence
	}
	return nil
}

func (r *JobRepository) CancelWorkerUpload(
	ctx context.Context,
	identity workergateway.Identity,
	uploadID string,
	fence int64,
) error {
	tag, err := r.db.pool.Exec(ctx, `
		UPDATE worker_gateway_uploads u
		SET state = 'canceled', upload_lease_expires_at = NULL,
		    updated_at = clock_timestamp()
		FROM worker_gateway_sessions s
		WHERE u.worker_id = s.worker_id
		  AND u.id = $5 AND u.worker_id = $1
		  AND u.job_id = $2 AND u.attempt_id = $3
		  AND s.attempt_fence = $4
		  AND u.state = 'uploading' AND u.upload_fence = $6
	`, identity.WorkerID, identity.JobID, identity.AttemptID,
		identity.FenceToken, uploadID, fence)
	if err != nil {
		return fmt.Errorf("cancel durable worker upload: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return workergateway.ErrStaleFence
	}
	return nil
}

func (r *JobRepository) WorkloadIdentityInventory(
	ctx context.Context,
) ([]workergateway.IssuerGenerationStatus, []workergateway.CertificateStatus, error) {
	issuerRows, err := r.db.pool.Query(ctx, `
		SELECT fingerprint, issuer_id, provider, subject, serial_hex,
		       not_before, not_after, state, last_issued_at, revoked_at,
		       revoke_reason,
		       (
		         SELECT count(*) FROM workload_certificates c
		         WHERE c.issuer_fingerprint = i.fingerprint
		           AND c.state = 'active'
		           AND c.not_after > clock_timestamp()
		       )
		FROM workload_issuer_generations i
		ORDER BY last_issued_at DESC, fingerprint
	`)
	if err != nil {
		return nil, nil, fmt.Errorf("list workload issuer generations: %w", err)
	}
	issuers := make([]workergateway.IssuerGenerationStatus, 0)
	for issuerRows.Next() {
		var item workergateway.IssuerGenerationStatus
		if err := issuerRows.Scan(
			&item.Fingerprint, &item.IssuerID, &item.Provider, &item.Subject,
			&item.Serial, &item.NotBefore, &item.NotAfter, &item.State,
			&item.LastIssuedAt, &item.RevokedAt, &item.RevokeReason,
			&item.ActiveCertificates,
		); err != nil {
			issuerRows.Close()
			return nil, nil, err
		}
		issuers = append(issuers, item)
	}
	if err := issuerRows.Err(); err != nil {
		issuerRows.Close()
		return nil, nil, err
	}
	issuerRows.Close()

	certificateRows, err := r.db.pool.Query(ctx, `
		SELECT fingerprint, serial_hex, issuer_fingerprint, worker_id,
		       job_id::text, attempt_id::text, attempt_fence, not_before,
		       not_after, state, issued_at, last_seen_at, revoked_at,
		       revoke_reason
		FROM workload_certificates
		ORDER BY issued_at DESC, fingerprint
		LIMIT 100
	`)
	if err != nil {
		return nil, nil, fmt.Errorf("list workload certificates: %w", err)
	}
	certificates := make([]workergateway.CertificateStatus, 0)
	for certificateRows.Next() {
		var item workergateway.CertificateStatus
		if err := certificateRows.Scan(
			&item.Fingerprint, &item.Serial, &item.IssuerFingerprint,
			&item.WorkerID, &item.JobID, &item.AttemptID,
			&item.AttemptFence, &item.NotBefore, &item.NotAfter, &item.State,
			&item.IssuedAt, &item.LastSeenAt, &item.RevokedAt,
			&item.RevokeReason,
		); err != nil {
			certificateRows.Close()
			return nil, nil, err
		}
		certificates = append(certificates, item)
	}
	if err := certificateRows.Err(); err != nil {
		certificateRows.Close()
		return nil, nil, err
	}
	certificateRows.Close()
	return issuers, certificates, nil
}

func (r *JobRepository) RevokeWorkloadCertificate(
	ctx context.Context,
	fingerprint, reason string,
) error {
	fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	reason = strings.TrimSpace(reason)
	if len(fingerprint) != 64 || reason == "" || len(reason) > maxGatewayErrorBytes {
		return fmt.Errorf("valid certificate fingerprint and revoke reason are required")
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return fmt.Errorf("invalid certificate fingerprint")
	}
	return r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		var attemptID string
		err := q.QueryRow(ctx, `
			SELECT attempt_id::text
			FROM workload_certificates
			WHERE fingerprint = $1
			FOR UPDATE
		`, fingerprint).Scan(&attemptID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("workload certificate not found")
		}
		if err != nil {
			return fmt.Errorf("lock workload certificate: %w", err)
		}
		return revokeWorkerGatewayAttempt(
			ctx, q, attemptID, reason, time.Now().UTC(),
		)
	})
}

func (r *JobRepository) RevokeWorkloadIssuer(
	ctx context.Context,
	fingerprint, reason string,
) error {
	fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	reason = strings.TrimSpace(reason)
	if len(fingerprint) != 64 || reason == "" || len(reason) > maxGatewayErrorBytes {
		return fmt.Errorf("valid issuer fingerprint and revoke reason are required")
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return fmt.Errorf("invalid issuer fingerprint")
	}
	return r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		now := time.Now().UTC()
		tag, err := q.Exec(ctx, `
			UPDATE workload_issuer_generations
			SET state = 'revoked', revoked_at = $3, revoke_reason = $2
			WHERE fingerprint = $1 AND state <> 'revoked'
		`, fingerprint, reason, now)
		if err != nil {
			return fmt.Errorf("revoke workload issuer: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := q.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM workload_issuer_generations
					WHERE fingerprint = $1
				)
			`, fingerprint).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("workload issuer not found")
			}
			return nil
		}
		if _, err := q.Exec(ctx, `
			UPDATE workload_certificates
			SET state = 'revoked', revoked_at = $3, revoke_reason = $2
			WHERE issuer_fingerprint = $1 AND state = 'active'
		`, fingerprint, reason, now); err != nil {
			return fmt.Errorf("revoke issuer workload certificates: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE worker_gateway_sessions s
			SET state = 'revoked', revoked_at = $3, revoke_reason = $2,
			    updated_at = $3
			FROM workload_certificates c
			WHERE s.certificate_fingerprint = c.fingerprint
			  AND c.issuer_fingerprint = $1 AND s.state = 'active'
		`, fingerprint, reason, now); err != nil {
			return fmt.Errorf("revoke issuer worker sessions: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE worker_gateway_commands c
			SET state = 'canceled', completed_at = $3,
			    delivery_lease_expires_at = NULL,
			    error = CASE WHEN c.error = '' THEN $2 ELSE c.error END,
			    updated_at = $3
			FROM workload_certificates cert
			WHERE c.worker_id = cert.worker_id
			  AND cert.issuer_fingerprint = $1
			  AND c.state IN ('queued', 'delivered')
		`, fingerprint, reason, now); err != nil {
			return fmt.Errorf("cancel issuer worker commands: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE worker_gateway_uploads u
			SET state = 'canceled', upload_lease_expires_at = NULL,
			    updated_at = $2
			FROM workload_certificates cert
			WHERE u.worker_id = cert.worker_id
			  AND cert.issuer_fingerprint = $1
			  AND u.state IN ('ready', 'uploading')
		`, fingerprint, now); err != nil {
			return fmt.Errorf("cancel issuer worker uploads: %w", err)
		}
		return nil
	})
}

func (r *JobRepository) WorkerGatewayStatus(
	ctx context.Context,
) (workergateway.RuntimeStatus, error) {
	var status workergateway.RuntimeStatus
	err := r.db.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM worker_gateway_sessions WHERE state = 'active'),
		  (SELECT count(*) FROM worker_gateway_sessions
		   WHERE state = 'active' AND connected_at IS NOT NULL),
		  (SELECT count(*) FROM worker_gateway_commands
		   WHERE state IN ('queued', 'delivered')),
		  (SELECT count(*) FROM worker_gateway_uploads
		   WHERE state IN ('ready', 'uploading')),
		  (SELECT count(*) FROM workload_issuer_generations
		   WHERE state = 'active' AND not_after > clock_timestamp()),
		  (SELECT count(*) FROM workload_issuer_generations
		   WHERE state = 'draining'),
		  (SELECT count(*) FROM workload_issuer_generations
		   WHERE state = 'revoked'),
		  (SELECT count(*) FROM workload_certificates
		   WHERE state = 'active' AND not_after > clock_timestamp()),
		  (SELECT count(*) FROM workload_certificates
		   WHERE state = 'revoked'),
		  (SELECT count(*) FROM workload_certificates
		   WHERE state = 'active'
		     AND not_after > clock_timestamp()
		     AND not_after <= clock_timestamp() + interval '30 minutes')
	`).Scan(
		&status.RegisteredSessions, &status.ConnectedSessions,
		&status.PendingTasks, &status.PendingUploads,
		&status.ActiveIssuers, &status.DrainingIssuers,
		&status.RevokedIssuers, &status.ActiveCertificates,
		&status.RevokedCertificates, &status.ExpiringCertificates,
	)
	if err != nil {
		return status, fmt.Errorf("read durable worker gateway status: %w", err)
	}
	status.Authority = "postgresql"
	return status, nil
}
