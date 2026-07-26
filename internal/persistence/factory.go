package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/imagefactory"
)

// RecordFactoryStatus imports one immutable, digest-bound image-factory
// snapshot. Re-reading the same file is idempotent.
func (r *JobRepository) RecordFactoryStatus(ctx context.Context, source string, status *imagefactory.FactoryStatus) error {
	if status == nil {
		return fmt.Errorf("factory status is nil")
	}
	if err := status.Validate(); err != nil {
		return err
	}
	if source == "" {
		source = "image-factory-status"
	}
	manifest, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal factory status: %w", err)
	}
	sum := sha256.Sum256(manifest)
	digest := hex.EncodeToString(sum[:])

	err = r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		runID := uuid.New()
		err := q.QueryRow(ctx, `
			INSERT INTO factory_runs (
				id, state, input_lock_digest, manifest, source_key,
				source_revision, created_at, updated_at, completed_at
			) VALUES (
				$1, $2::text, $3::text, $4::jsonb, $5::text, $6::text,
				$7::timestamptz, $7::timestamptz,
				CASE WHEN $2::text IN ('passed','failed','blocked') THEN $7::timestamptz ELSE NULL END
			)
			ON CONFLICT (source_key, input_lock_digest) DO NOTHING
			RETURNING id
		`, runID, status.OverallState, digest, string(manifest), source,
			status.UpdatedAt.Format("2006-01-02T15:04:05.999999999Z07:00"), status.UpdatedAt).Scan(&runID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert factory run: %w", err)
		}

		sequence := 0
		for _, milestone := range status.Milestones {
			detail, err := json.Marshal(milestone)
			if err != nil {
				return err
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO factory_steps (
					id, run_id, step_name, state, sequence, detail,
					started_at, completed_at
				) VALUES ($1, $2, $3, $4, $5, $6::jsonb, NULL, $7)
			`, uuid.New(), runID, milestone.ID, milestone.State, sequence,
				string(detail), milestone.CompletedAt); err != nil {
				return fmt.Errorf("insert factory milestone %s: %w", milestone.ID, err)
			}
			sequence++
			for _, step := range milestone.Steps {
				stepDetail, err := json.Marshal(step)
				if err != nil {
					return err
				}
				if _, err := q.Exec(ctx, `
					INSERT INTO factory_steps (
						id, run_id, step_name, state, sequence, detail,
						started_at, completed_at
					) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)
				`, uuid.New(), runID, milestone.ID+"/"+step.ID, step.State,
					sequence, string(stepDetail), step.StartedAt, step.CompletedAt); err != nil {
					return fmt.Errorf("insert factory step %s/%s: %w", milestone.ID, step.ID, err)
				}
				sequence++
				if step.Log != nil {
					if err := insertFactoryEvidence(ctx, q, runID, "step-log", *step.Log,
						map[string]any{"milestone": milestone.ID, "step": step.ID}); err != nil {
						return err
					}
				}
			}
			for _, evidence := range milestone.Evidence {
				if err := insertFactoryEvidence(ctx, q, runID, "milestone-evidence", evidence,
					map[string]any{"milestone": milestone.ID}); err != nil {
					return err
				}
			}
		}
		_, err = q.Exec(ctx, `
			INSERT INTO outbox_events (
				topic, aggregate_type, aggregate_id, payload
			) VALUES (
				'image_factory.status_recorded', 'factory_run', $1,
				jsonb_build_object('digest', $2::text, 'state', $3::text, 'source', $4::text)
			)
		`, runID.String(), digest, status.OverallState, source)
		return err
	})
	r.recordWrite(err)
	return err
}

func insertFactoryEvidence(ctx context.Context, q Querier, runID uuid.UUID, kind string,
	evidence imagefactory.FactoryEvidence, metadata map[string]any) error {
	if evidence.Digest == "" || evidence.Path == "" {
		// The file schema permits human labels without a durable object. DB-3
		// stores only evidence that can actually be verified and retrieved.
		return nil
	}
	metadata["label"] = evidence.Label
	metadata["recorded_at"] = evidence.RecordedAt
	metadata["size_bytes"] = evidence.SizeBytes
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO evidence_refs (
			id, factory_run_id, kind, location, digest, metadata
		) VALUES ($1, $2, $3, $4, $5, $6::jsonb)
	`, uuid.New(), runID, kind, evidence.Path, evidence.Digest, string(data))
	if err != nil {
		return fmt.Errorf("insert factory evidence %s: %w", evidence.Label, err)
	}
	return nil
}
