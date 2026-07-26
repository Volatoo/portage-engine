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

func validateMetadataFence(ctx context.Context, q Querier, status *builder.BuildStatus, allowTerminal bool) (uuid.UUID, uuid.UUID, error) {
	if status == nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("runtime metadata requires a build status")
	}
	jobID, err := uuid.Parse(status.JobID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("invalid metadata job id: %w", err)
	}
	if status.AttemptID == "" || status.FenceToken <= 0 || status.LeaseOwner == "" {
		return uuid.Nil, uuid.Nil, fmt.Errorf("runtime metadata requires an active durable attempt fence")
	}
	attemptID, err := uuid.Parse(status.AttemptID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("invalid metadata attempt id: %w", err)
	}
	var allowed bool
	err = q.QueryRow(ctx, `
		SELECT (
			l.expires_at > clock_timestamp()
			AND l.fence_token = $3
			AND a.fence_token = $3
			AND w.stable_name = $4
		) OR (
			$5
			AND a.fence_token = $3
			AND j.state IN ('completed','success','failed','canceled')
			AND a.state IN ('completed','success','failed','canceled')
		)
		FROM build_attempts a
		JOIN build_jobs j ON j.id = a.job_id
		LEFT JOIN worker_leases l ON l.attempt_id = a.id
		LEFT JOIN workers w ON w.id = l.worker_id
		WHERE a.id = $1 AND a.job_id = $2
		FOR UPDATE OF a
	`, attemptID, jobID, status.FenceToken, status.LeaseOwner, allowTerminal).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) || !allowed {
		return uuid.Nil, uuid.Nil, fmt.Errorf("job %s runtime metadata fence is stale or expired", status.JobID)
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("validate runtime metadata fence: %w", err)
	}
	return jobID, attemptID, nil
}

// RecordInfra upserts the provider resource owned by the exact active attempt.
// Secrets are never accepted by this storage contract.
func (r *JobRepository) RecordInfra(ctx context.Context, status *builder.BuildStatus, record builder.InfraRecord) error {
	if record.Provider == "" || record.ProviderInstanceID == "" || record.State == "" {
		return fmt.Errorf("infra metadata requires provider, provider_instance_id, and state")
	}
	attributes, err := json.Marshal(record.Attributes)
	if err != nil {
		return fmt.Errorf("marshal infra attributes: %w", err)
	}
	err = r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		jobID, attemptID, err := validateMetadataFence(ctx, q, status, true)
		if err != nil {
			return err
		}
		ownerToken := fmt.Sprintf("%s:%d", attemptID, status.FenceToken)
		tag, err := q.Exec(ctx, `
			INSERT INTO infra_instances (
				id, job_id, attempt_id, provider, provider_instance_id, state,
				fence_token, owner_token, remote_state_ref, attributes,
				failure_detail, cleanup_after, deleted_at, created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb,
				$11, $12, $13, clock_timestamp(), clock_timestamp()
			)
			ON CONFLICT (provider, provider_instance_id)
			DO UPDATE SET
				job_id = EXCLUDED.job_id,
				attempt_id = EXCLUDED.attempt_id,
				state = EXCLUDED.state,
				fence_token = EXCLUDED.fence_token,
				owner_token = EXCLUDED.owner_token,
				remote_state_ref = EXCLUDED.remote_state_ref,
				attributes = EXCLUDED.attributes,
				failure_detail = EXCLUDED.failure_detail,
				cleanup_after = EXCLUDED.cleanup_after,
				deleted_at = EXCLUDED.deleted_at,
				updated_at = clock_timestamp()
			WHERE infra_instances.owner_token = EXCLUDED.owner_token
		`, uuid.New(), jobID, attemptID, record.Provider, record.ProviderInstanceID,
			record.State, status.FenceToken, ownerToken, record.RemoteStateRef,
			string(attributes), record.FailureDetail, record.CleanupAfter, record.DeletedAt)
		if err != nil {
			return fmt.Errorf("upsert infra metadata: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("infrastructure %s/%s is already owned by another attempt",
				record.Provider, record.ProviderInstanceID)
		}
		return nil
	})
	r.recordWrite(err)
	return err
}

// RecordArtifacts stores digest-bound staged/published objects. Upserts are
// idempotent, but a different attempt cannot overwrite another attempt's row.
func (r *JobRepository) RecordArtifacts(ctx context.Context, status *builder.BuildStatus, records []builder.ArtifactRecord) error {
	if len(records) == 0 {
		return fmt.Errorf("artifact metadata is empty")
	}
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		jobID, attemptID, err := validateMetadataFence(ctx, q, status, false)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.Kind == "" || record.State == "" || record.Digest == "" ||
				record.SizeBytes < 0 || record.Location == "" {
				return fmt.Errorf("artifact metadata requires kind, state, digest, non-negative size, and location")
			}
			lineage, err := json.Marshal(record.Lineage)
			if err != nil {
				return fmt.Errorf("marshal artifact lineage: %w", err)
			}
			mediaType := record.MediaType
			if mediaType == "" {
				mediaType = "application/octet-stream"
			}
			tag, err := q.Exec(ctx, `
				INSERT INTO artifacts (
					id, job_id, attempt_id, kind, state, digest_algorithm,
					digest, size_bytes, media_type, location, lineage,
					retention_until, created_at, published_at, updated_at
				) VALUES (
					$1, $2, $3, $4, $5, 'sha256',
					$6, $7, $8, $9, $10::jsonb,
					$11, clock_timestamp(), $12, clock_timestamp()
				)
				ON CONFLICT (digest_algorithm, digest, location)
				DO UPDATE SET
					state = EXCLUDED.state,
					published_at = COALESCE(EXCLUDED.published_at, artifacts.published_at),
					retention_until = EXCLUDED.retention_until,
					lineage = EXCLUDED.lineage,
					updated_at = clock_timestamp()
				WHERE artifacts.job_id = EXCLUDED.job_id
				  AND artifacts.attempt_id = EXCLUDED.attempt_id
			`, uuid.New(), jobID, attemptID, record.Kind, record.State,
				record.Digest, record.SizeBytes, mediaType, record.Location,
				string(lineage), record.RetainUntil, record.Published)
			if err != nil {
				return fmt.Errorf("upsert artifact metadata: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("artifact %s is already owned by another attempt", record.Location)
			}
		}
		return nil
	})
	r.recordWrite(err)
	return err
}

// RuntimeMetadataStatus is a low-cardinality reconciliation view.
type RuntimeMetadataStatus struct {
	LiveInfra            int        `json:"live_infra"`
	CleanupFailedInfra   int        `json:"cleanup_failed_infra"`
	PublishedArtifacts   int        `json:"published_artifacts"`
	StagedArtifacts      int        `json:"staged_artifacts"`
	FactoryRuns          int        `json:"factory_runs"`
	MissingArtifacts     int        `json:"missing_artifacts"`
	CorruptArtifacts     int        `json:"corrupt_artifacts"`
	OrphanedArtifacts    int        `json:"orphaned_artifacts"`
	LastMetadataUpdateAt *time.Time `json:"last_metadata_update_at,omitempty"`
}

func (r *JobRepository) RuntimeMetadataStatus(ctx context.Context) (RuntimeMetadataStatus, error) {
	var status RuntimeMetadataStatus
	err := r.db.Pool().QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM infra_instances WHERE deleted_at IS NULL AND state NOT IN ('destroyed','terminated')),
			(SELECT count(*) FROM infra_instances WHERE deleted_at IS NULL AND state = 'destroy_failed'),
			(SELECT count(*) FROM artifacts WHERE state = 'published'),
			(SELECT count(*) FROM artifacts WHERE state = 'staged'),
			(SELECT count(*) FROM factory_runs),
			(SELECT count(*) FROM artifacts WHERE state = 'missing'),
			(SELECT count(*) FROM artifacts WHERE state = 'corrupt'),
			(SELECT count(*) FROM artifacts WHERE state = 'orphaned'),
			(SELECT max(updated_at) FROM (
				SELECT updated_at FROM infra_instances
				UNION ALL SELECT updated_at FROM artifacts
				UNION ALL SELECT updated_at FROM factory_runs
			) metadata_updates)
	`).Scan(&status.LiveInfra, &status.CleanupFailedInfra, &status.PublishedArtifacts,
		&status.StagedArtifacts, &status.FactoryRuns, &status.MissingArtifacts,
		&status.CorruptArtifacts, &status.OrphanedArtifacts, &status.LastMetadataUpdateAt)
	if err != nil {
		return status, fmt.Errorf("read runtime metadata status: %w", err)
	}
	return status, nil
}

type ArtifactInventory struct {
	Location  string
	Digest    string
	SizeBytes int64
}

type ArtifactReconcileReport struct {
	Published int `json:"published"`
	Matched   int `json:"matched"`
	Missing   int `json:"missing"`
	Corrupt   int `json:"corrupt"`
	Orphaned  int `json:"orphaned"`
}

// ReconcileArtifacts compares external binpkg bytes with PostgreSQL metadata.
// Unknown files are quarantined logically as "orphaned"; they are never
// silently promoted into a job lineage.
func (r *JobRepository) ReconcileArtifacts(ctx context.Context, inventory []ArtifactInventory) (ArtifactReconcileReport, error) {
	report := ArtifactReconcileReport{}
	available := make(map[string]ArtifactInventory, len(inventory))
	for _, item := range inventory {
		available[item.Location] = item
	}
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		rows, err := q.Query(ctx, `
			SELECT id, location, digest, size_bytes, state
			FROM artifacts
			WHERE state IN ('published','missing','corrupt','staged')
			ORDER BY CASE WHEN state = 'staged' THEN 1 ELSE 0 END, created_at
			FOR UPDATE
		`)
		if err != nil {
			return err
		}
		type durableArtifact struct {
			ID                      uuid.UUID
			Location, Digest, State string
			Size                    int64
		}
		var durable []durableArtifact
		for rows.Next() {
			var item durableArtifact
			if err := rows.Scan(&item.ID, &item.Location, &item.Digest, &item.Size, &item.State); err != nil {
				rows.Close()
				return err
			}
			durable = append(durable, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, expected := range durable {
			actual, exists := available[expected.Location]
			if expected.State == "staged" && !exists {
				// The normal pre-promotion record points at the future public
				// location. Absence is expected until the external atomic move.
				continue
			}
			if expected.State != "staged" {
				report.Published++
			}
			state := "published"
			switch {
			case !exists:
				state = "missing"
				report.Missing++
			case actual.Digest != expected.Digest || actual.SizeBytes != expected.Size:
				state = "corrupt"
				report.Corrupt++
			default:
				report.Matched++
				if expected.State == "staged" {
					report.Published++
				}
			}
			delete(available, expected.Location)
			if _, err := q.Exec(ctx, `
				UPDATE artifacts
				SET state = $2,
				    published_at = CASE
				      WHEN $2 = 'published' THEN COALESCE(published_at, clock_timestamp())
				      ELSE published_at
				    END,
				    updated_at = clock_timestamp()
				WHERE id = $1
			`, expected.ID, state); err != nil {
				return err
			}
		}
		orphanRows, err := q.Query(ctx, `
			SELECT id, location, digest, size_bytes, state
			FROM artifacts
			WHERE state = 'orphaned'
			FOR UPDATE
		`)
		if err != nil {
			return err
		}
		var knownOrphans []durableArtifact
		for orphanRows.Next() {
			var item durableArtifact
			if err := orphanRows.Scan(&item.ID, &item.Location, &item.Digest, &item.Size, &item.State); err != nil {
				orphanRows.Close()
				return err
			}
			knownOrphans = append(knownOrphans, item)
		}
		if err := orphanRows.Err(); err != nil {
			orphanRows.Close()
			return err
		}
		orphanRows.Close()
		for _, orphan := range knownOrphans {
			actual, exists := available[orphan.Location]
			if exists && actual.Digest == orphan.Digest && actual.SizeBytes == orphan.Size {
				delete(available, orphan.Location)
				report.Orphaned++
				continue
			}
			// Orphan rows have no build lineage. Remove stale observations so
			// a deleted or replaced file does not permanently poison status.
			if _, err := q.Exec(ctx, `DELETE FROM artifacts WHERE id = $1`, orphan.ID); err != nil {
				return err
			}
		}
		for _, item := range available {
			if item.Location == "" || item.Digest == "" || item.SizeBytes < 0 {
				continue
			}
			tag, err := q.Exec(ctx, `
				INSERT INTO artifacts (
					id, kind, state, digest_algorithm, digest, size_bytes,
					media_type, location, lineage, created_at, updated_at
				) VALUES (
					$1, 'gentoo-binpkg', 'orphaned', 'sha256', $2, $3,
					'application/vnd.gentoo.gpkg', $4,
					'{"reason":"discovered_without_job_lineage"}'::jsonb,
					clock_timestamp(), clock_timestamp()
				)
				ON CONFLICT (digest_algorithm, digest, location) DO NOTHING
			`, uuid.New(), item.Digest, item.SizeBytes, item.Location)
			if err != nil {
				return err
			}
			report.Orphaned += int(tag.RowsAffected())
		}
		return nil
	})
	r.recordWrite(err)
	return report, err
}
