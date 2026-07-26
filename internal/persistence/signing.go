package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/signing"
)

func (r *JobRepository) EnqueueSigning(ctx context.Context, request signing.Request) (*signing.Task, error) {
	if err := signing.ValidateRequest(request); err != nil {
		return nil, err
	}
	input, err := json.Marshal(request.Artifacts)
	if err != nil {
		return nil, fmt.Errorf("marshal signing input: %w", err)
	}
	var task *signing.Task
	err = r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		var active bool
		err = q.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM build_attempts a
				JOIN worker_leases l ON l.attempt_id = a.id
				JOIN workers w ON w.id = l.worker_id
				JOIN build_jobs j ON j.id = a.job_id
				WHERE a.id = $1 AND a.job_id = $2
				  AND a.fence_token = $3 AND l.fence_token = $3
				  AND w.stable_name = $4
				  AND l.expires_at > clock_timestamp()
				  AND j.state IN ('verifying','signing')
			)
		`, request.AttemptID, request.JobID, request.AttemptFence, request.LeaseOwner).Scan(&active)
		if err != nil {
			return fmt.Errorf("validate signing attempt fence: %w", err)
		}
		if !active {
			return fmt.Errorf("signing request attempt fence is stale or inactive")
		}
		for _, artifact := range request.Artifacts {
			location := "quarantine:" + request.SourceToken + "/" + artifact.RelativePath
			var exists bool
			if err := q.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM artifacts
					WHERE job_id = $1 AND attempt_id = $2
					  AND state = 'verified_unsigned'
					  AND digest_algorithm = 'sha256' AND digest = $3
					  AND size_bytes = $4 AND location = $5
				)
			`, request.JobID, request.AttemptID, artifact.InputDigest,
				artifact.InputSize, location).Scan(&exists); err != nil {
				return fmt.Errorf("validate unsigned artifact metadata: %w", err)
			}
			if !exists {
				return fmt.Errorf("unsigned artifact %q is not verified for this attempt", artifact.RelativePath)
			}
		}

		taskID := uuid.New()
		var row signing.Task
		var inputJSON, outputJSON []byte
		var leaseExpires *time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO signing_tasks (
				id, job_id, attempt_id, attempt_fence, state, source_token,
				architecture, input_manifest
			) VALUES ($1, $2, $3, $4, 'queued', $5, $6, $7::jsonb)
			ON CONFLICT (job_id, attempt_id)
			DO UPDATE SET updated_at = clock_timestamp()
			RETURNING id::text, job_id::text, attempt_id::text, attempt_fence,
			          state, source_token, architecture, input_manifest,
			          COALESCE(output_manifest, '[]'::jsonb), signing_key_id,
			          owner, claim_fence, lease_expires_at, error_detail
		`, taskID, request.JobID, request.AttemptID, request.AttemptFence,
			request.SourceToken, request.Architecture, string(input)).Scan(
			&row.ID, &row.JobID, &row.AttemptID, &row.AttemptFence,
			&row.State, &row.SourceToken, &row.Architecture, &inputJSON,
			&outputJSON, &row.SigningKeyID, &row.Owner, &row.ClaimFence,
			&leaseExpires, &row.Error,
		)
		if err != nil {
			return fmt.Errorf("enqueue signing task: %w", err)
		}
		if err := json.Unmarshal(inputJSON, &row.Artifacts); err != nil {
			return fmt.Errorf("decode signing input: %w", err)
		}
		if row.State == "completed" {
			if err := json.Unmarshal(outputJSON, &row.Artifacts); err != nil {
				return fmt.Errorf("decode signing output: %w", err)
			}
		}
		if row.SourceToken != request.SourceToken || row.Architecture != request.Architecture {
			return fmt.Errorf("existing signing task does not match this immutable request")
		}
		if !sameSigningInputs(row.Artifacts, request.Artifacts) {
			return fmt.Errorf("existing signing task input manifest does not match this immutable request")
		}
		row.LeaseExpiresAt = leaseExpires
		task = &row
		return nil
	})
	r.recordWrite(err)
	return task, err
}

func (r *JobRepository) GetSigningTask(ctx context.Context, id string) (*signing.Task, error) {
	var row signing.Task
	var inputJSON, outputJSON []byte
	err := r.db.Pool().QueryRow(ctx, `
		SELECT id::text, job_id::text, attempt_id::text, attempt_fence,
		       state, source_token, architecture, input_manifest,
		       COALESCE(output_manifest, '[]'::jsonb), signing_key_id,
		       owner, claim_fence, lease_expires_at, error_detail
		FROM signing_tasks WHERE id = $1
	`, id).Scan(
		&row.ID, &row.JobID, &row.AttemptID, &row.AttemptFence,
		&row.State, &row.SourceToken, &row.Architecture, &inputJSON,
		&outputJSON, &row.SigningKeyID, &row.Owner, &row.ClaimFence,
		&row.LeaseExpiresAt, &row.Error,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("signing task %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("read signing task: %w", err)
	}
	payload := inputJSON
	if row.State == "completed" {
		payload = outputJSON
	}
	if err := json.Unmarshal(payload, &row.Artifacts); err != nil {
		return nil, fmt.Errorf("decode signing task manifest: %w", err)
	}
	return &row, nil
}

func (r *JobRepository) SigningRuntimeStatus(ctx context.Context) (signing.RuntimeStatus, error) {
	var status signing.RuntimeStatus
	err := r.db.Pool().QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE state = 'queued'),
			count(*) FILTER (WHERE state = 'claimed'),
			count(*) FILTER (WHERE state = 'completed'),
			count(*) FILTER (WHERE state = 'failed'),
			count(*) FILTER (WHERE state = 'canceled'),
			min(created_at) FILTER (WHERE state = 'queued'),
			min(updated_at) FILTER (WHERE state = 'claimed')
		FROM signing_tasks
	`).Scan(
		&status.Queued, &status.Claimed, &status.Completed,
		&status.Failed, &status.Canceled,
		&status.OldestQueued, &status.OldestClaimed,
	)
	return status, err
}

func (r *JobRepository) ClaimSigning(ctx context.Context, owner string, lease time.Duration) (*signing.Task, error) {
	if owner == "" || lease <= 0 {
		return nil, fmt.Errorf("signer owner and positive lease are required")
	}
	var task *signing.Task
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if _, err := q.Exec(ctx, `
			UPDATE signing_tasks s
			SET state = 'canceled', owner = '', lease_expires_at = NULL,
			    error_detail = 'originating build attempt is no longer active',
			    updated_at = clock_timestamp()
			WHERE s.state IN ('queued','claimed')
			  AND NOT EXISTS (
				SELECT 1
				FROM worker_leases l
				JOIN build_attempts a ON a.id = l.attempt_id
				WHERE a.id = s.attempt_id
				  AND a.fence_token = s.attempt_fence
				  AND l.fence_token = s.attempt_fence
				  AND l.expires_at > clock_timestamp()
			  )
		`); err != nil {
			return fmt.Errorf("cancel stale signing tasks: %w", err)
		}

		var row signing.Task
		var inputJSON []byte
		err := q.QueryRow(ctx, `
			WITH candidate AS (
				SELECT s.id
				FROM signing_tasks s
				JOIN worker_leases l ON l.attempt_id = s.attempt_id
				JOIN build_attempts a ON a.id = s.attempt_id
				WHERE s.state IN ('queued','claimed')
				  AND (s.state = 'queued' OR s.lease_expires_at <= clock_timestamp())
				  AND a.fence_token = s.attempt_fence
				  AND l.fence_token = s.attempt_fence
				  AND l.expires_at > clock_timestamp()
				ORDER BY s.created_at, s.id
				LIMIT 1
				FOR UPDATE OF s SKIP LOCKED
			)
			UPDATE signing_tasks s
			SET state = 'claimed', owner = $1,
			    claim_fence = s.claim_fence + 1,
			    lease_expires_at = clock_timestamp() + $2::interval,
			    claim_attempts = s.claim_attempts + 1,
			    error_detail = '', updated_at = clock_timestamp()
			FROM candidate c
			WHERE s.id = c.id
			RETURNING s.id::text, s.job_id::text, s.attempt_id::text,
			          s.attempt_fence, s.state, s.source_token, s.architecture,
			          s.input_manifest, s.owner, s.claim_fence,
			          s.lease_expires_at
		`, owner, lease.String()).Scan(
			&row.ID, &row.JobID, &row.AttemptID, &row.AttemptFence,
			&row.State, &row.SourceToken, &row.Architecture, &inputJSON,
			&row.Owner, &row.ClaimFence, &row.LeaseExpiresAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim signing task: %w", err)
		}
		if err := json.Unmarshal(inputJSON, &row.Artifacts); err != nil {
			return fmt.Errorf("decode claimed signing task: %w", err)
		}
		task = &row
		return nil
	})
	r.recordWrite(err)
	return task, err
}

func (r *JobRepository) RenewSigning(ctx context.Context, task *signing.Task, lease time.Duration) error {
	if task == nil || task.ID == "" || task.Owner == "" || task.ClaimFence <= 0 || lease <= 0 {
		return fmt.Errorf("renew signing requires task, owner, fence, and positive lease")
	}
	tag, err := r.db.Pool().Exec(ctx, `
		UPDATE signing_tasks s
		SET lease_expires_at = clock_timestamp() + $4::interval,
		    updated_at = clock_timestamp()
		WHERE s.id = $1 AND s.owner = $2 AND s.claim_fence = $3
		  AND s.state = 'claimed' AND s.lease_expires_at > clock_timestamp()
		  AND EXISTS (
			SELECT 1 FROM worker_leases l
			WHERE l.attempt_id = s.attempt_id
			  AND l.fence_token = s.attempt_fence
			  AND l.expires_at > clock_timestamp()
		  )
	`, task.ID, task.Owner, task.ClaimFence, lease.String())
	if err != nil {
		return fmt.Errorf("renew signing lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("signing task lease or originating attempt fence is stale")
	}
	return nil
}

func (r *JobRepository) CompleteSigning(ctx context.Context, task *signing.Task, keyID string, outputs []signing.Artifact) error {
	if task == nil || task.ID == "" || task.Owner == "" || task.ClaimFence <= 0 || keyID == "" {
		return fmt.Errorf("complete signing requires task, owner, fence, and key ID")
	}
	if len(outputs) != len(task.Artifacts) {
		return fmt.Errorf("signed output count does not match input")
	}
	for _, artifact := range outputs {
		if err := signing.ValidateArtifact(artifact, true); err != nil {
			return err
		}
	}
	if !sameSigningOutputs(task.Artifacts, outputs) {
		return fmt.Errorf("signed outputs do not preserve the input artifact set")
	}
	outputJSON, err := json.Marshal(outputs)
	if err != nil {
		return err
	}
	tag, err := r.db.Pool().Exec(ctx, `
		UPDATE signing_tasks s
		SET state = 'completed', output_manifest = $5::jsonb,
		    signing_key_id = $6, owner = '', lease_expires_at = NULL,
		    error_detail = '', completed_at = clock_timestamp(),
		    updated_at = clock_timestamp()
		WHERE s.id = $1 AND s.owner = $2 AND s.claim_fence = $3
		  AND s.attempt_fence = $4 AND s.state = 'claimed'
		  AND s.lease_expires_at > clock_timestamp()
		  AND EXISTS (
			SELECT 1 FROM worker_leases l
			WHERE l.attempt_id = s.attempt_id
			  AND l.fence_token = s.attempt_fence
			  AND l.expires_at > clock_timestamp()
		  )
	`, task.ID, task.Owner, task.ClaimFence, task.AttemptFence, string(outputJSON), keyID)
	if err != nil {
		r.recordWrite(err)
		return fmt.Errorf("complete signing task: %w", err)
	}
	if tag.RowsAffected() != 1 {
		err = fmt.Errorf("signing task lease or originating attempt fence is stale")
		r.recordWrite(err)
		return err
	}
	r.recordWrite(nil)
	return nil
}

func sameSigningInputs(left, right []signing.Artifact) bool {
	if len(left) != len(right) {
		return false
	}
	expected := make(map[string]signing.Artifact, len(left))
	for _, artifact := range left {
		expected[artifact.RelativePath] = artifact
	}
	for _, artifact := range right {
		match, ok := expected[artifact.RelativePath]
		if !ok || match.InputDigest != artifact.InputDigest || match.InputSize != artifact.InputSize {
			return false
		}
		delete(expected, artifact.RelativePath)
	}
	return len(expected) == 0
}

func sameSigningOutputs(inputs, outputs []signing.Artifact) bool {
	if len(inputs) != len(outputs) {
		return false
	}
	expected := make(map[string]signing.Artifact, len(inputs))
	for _, artifact := range inputs {
		expected[artifact.RelativePath] = artifact
	}
	for _, artifact := range outputs {
		match, ok := expected[artifact.RelativePath]
		if !ok || match.InputDigest != artifact.InputDigest || match.InputSize != artifact.InputSize {
			return false
		}
		delete(expected, artifact.RelativePath)
	}
	return len(expected) == 0
}

func (r *JobRepository) FailSigning(ctx context.Context, task *signing.Task, detail string) error {
	if task == nil || task.ID == "" || task.Owner == "" || task.ClaimFence <= 0 {
		return fmt.Errorf("fail signing requires task, owner, and fence")
	}
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	tag, err := r.db.Pool().Exec(ctx, `
		UPDATE signing_tasks
		SET state = 'failed', error_detail = $4, owner = '',
		    lease_expires_at = NULL, updated_at = clock_timestamp()
		WHERE id = $1 AND owner = $2 AND claim_fence = $3
		  AND state = 'claimed' AND lease_expires_at > clock_timestamp()
	`, task.ID, task.Owner, task.ClaimFence, detail)
	if err != nil {
		r.recordWrite(err)
		return fmt.Errorf("fail signing task: %w", err)
	}
	if tag.RowsAffected() != 1 {
		err = fmt.Errorf("signing task lease is stale")
		r.recordWrite(err)
		return err
	}
	r.recordWrite(nil)
	return nil
}
