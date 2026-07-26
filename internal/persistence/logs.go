package persistence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
)

const (
	maxLogChunkBytes  = 16 * 1024
	maxLoadedLogBytes = 4 * 1024 * 1024
)

func (r *JobRepository) AppendLogs(ctx context.Context, status *builder.BuildStatus, records []builder.LogRecord) error {
	if status == nil || len(records) == 0 {
		return nil
	}
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		jobID, err := uuid.Parse(status.JobID)
		if err != nil {
			return fmt.Errorf("invalid log job id: %w", err)
		}
		var attemptID any
		if status.AttemptID != "" {
			_, parsedAttempt, err := validateMetadataFence(ctx, q, status, true)
			if err != nil {
				return err
			}
			attemptID = parsedAttempt
		} else {
			var state string
			err := q.QueryRow(ctx, `
				SELECT state FROM build_jobs WHERE id = $1 AND legacy_visible = true
				FOR UPDATE
			`, jobID).Scan(&state)
			if err != nil {
				return fmt.Errorf("validate queued log ownership: %w", err)
			}
			if state != "queued" {
				return fmt.Errorf("job %s has no durable attempt for log state %s", jobID, state)
			}
		}
		for _, record := range records {
			message := record.Message
			if len(message) > maxLogChunkBytes {
				message = message[:maxLogChunkBytes-len("[... truncated ...]")] + "[... truncated ...]"
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO build_log_chunks (job_id, attempt_id, message, occurred_at)
				VALUES ($1, $2, $3, $4)
			`, jobID, attemptID, message, record.OccurredAt); err != nil {
				return fmt.Errorf("append durable build log: %w", err)
			}
		}
		return nil
	})
	r.recordWrite(err)
	return err
}

func (r *JobRepository) LoadLogs(ctx context.Context, jobID string) (string, error) {
	parsed, err := uuid.Parse(jobID)
	if err != nil {
		return "", fmt.Errorf("invalid log job id: %w", err)
	}
	rows, err := r.db.Pool().Query(ctx, `
		SELECT occurred_at, message
		FROM build_log_chunks
		WHERE job_id = $1
		ORDER BY id
	`, parsed)
	if err != nil {
		return "", fmt.Errorf("load durable build logs: %w", err)
	}
	defer rows.Close()
	var result strings.Builder
	for rows.Next() {
		var occurredAt time.Time
		var message string
		if err := rows.Scan(&occurredAt, &message); err != nil {
			return "", err
		}
		line := message + "\n"
		line = occurredAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00") + " " + line
		if result.Len()+len(line) > maxLoadedLogBytes {
			return result.String() + "[... durable log output truncated ...]\n", nil
		}
		result.WriteString(line)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return "", err
	}
	return result.String(), nil
}
