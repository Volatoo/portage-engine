package persistence

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
)

const artifactPromotionAdvisoryKey int64 = 0x50454E47494E45 // "PENGINE"

// AcquireArtifactPromotion holds one dedicated PostgreSQL session advisory
// lock across the short filesystem promotion saga. Connection loss releases
// the lock automatically; immutable destinations and reconciliation cover a
// crash between the external move and metadata finalization.
func (r *JobRepository) AcquireArtifactPromotion(ctx context.Context, status *builder.BuildStatus) (func() error, error) {
	conn, err := r.db.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire publication database session: %w", err)
	}
	locked := false
	defer func() {
		if !locked {
			conn.Release()
		}
	}()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, artifactPromotionAdvisoryKey); err != nil {
		return nil, fmt.Errorf("lock artifact publication: %w", err)
	}
	locked = true
	if _, _, err := validateMetadataFence(ctx, conn, status, false); err != nil {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, artifactPromotionAdvisoryKey)
		locked = false
		return nil, err
	}

	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(releaseCtx, `SELECT pg_advisory_unlock($1)`, artifactPromotionAdvisoryKey).Scan(&unlocked); err != nil {
				releaseErr = fmt.Errorf("unlock artifact publication: %w", err)
			} else if !unlocked {
				releaseErr = fmt.Errorf("artifact publication lock was not held")
			}
			conn.Release()
		})
		return releaseErr
	}, nil
}

// ClaimInfraCleanup leases one abandoned Terraform workspace. Eligibility is
// tied to the original attempt state/lease, so a live build is never destroyed
// merely because two control-plane replicas can see the same row.
func (r *JobRepository) ClaimInfraCleanup(ctx context.Context, owner string, leaseDuration time.Duration) (*builder.InfraCleanupClaim, error) {
	if owner == "" {
		return nil, fmt.Errorf("infra cleanup owner is required")
	}
	if leaseDuration <= 0 {
		return nil, fmt.Errorf("infra cleanup lease duration must be positive")
	}
	var claim *builder.InfraCleanupClaim
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		var value builder.InfraCleanupClaim
		err := q.QueryRow(ctx, `
			WITH candidate AS (
				SELECT i.id
				FROM infra_instances i
				LEFT JOIN build_attempts a ON a.id = i.attempt_id
				WHERE i.deleted_at IS NULL
				  AND i.remote_state_ref <> ''
				  AND i.state NOT IN ('destroyed', 'terminated')
				  AND i.next_cleanup_at <= clock_timestamp()
				  AND (
					i.cleanup_lease_expires_at IS NULL
					OR i.cleanup_lease_expires_at <= clock_timestamp()
				  )
				  AND NOT EXISTS (
					SELECT 1 FROM worker_leases live
					WHERE live.attempt_id = i.attempt_id
					  AND live.expires_at > clock_timestamp()
				  )
				  AND (
					i.state = 'destroy_failed'
					OR i.cleanup_after <= clock_timestamp()
					OR a.state IN ('expired', 'failed', 'canceled', 'completed', 'success')
				  )
				ORDER BY i.next_cleanup_at, i.updated_at, i.id
				LIMIT 1
				FOR UPDATE OF i SKIP LOCKED
			)
			UPDATE infra_instances i
			SET cleanup_owner = $1,
			    cleanup_fence = i.cleanup_fence + 1,
			    cleanup_lease_expires_at = clock_timestamp() + $2::interval,
			    cleanup_attempts = i.cleanup_attempts + 1,
			    state = 'cleanup_claimed',
			    updated_at = clock_timestamp()
			FROM candidate c
			WHERE i.id = c.id
			RETURNING i.id::text, i.provider, i.provider_instance_id,
			          i.remote_state_ref, i.cleanup_fence
		`, owner, leaseDuration.String()).Scan(
			&value.ID, &value.Provider, &value.ProviderInstanceID,
			&value.RemoteStateRef, &value.CleanupFence,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim infrastructure cleanup: %w", err)
		}
		claim = &value
		return nil
	})
	r.recordWrite(err)
	return claim, err
}

func (r *JobRepository) CompleteInfraCleanup(ctx context.Context, id, owner string, fence int64) error {
	tag, err := r.db.Pool().Exec(ctx, `
		UPDATE infra_instances
		SET state = 'destroyed', deleted_at = clock_timestamp(),
		    failure_detail = '', cleanup_owner = '',
		    cleanup_lease_expires_at = NULL, updated_at = clock_timestamp()
		WHERE id = $1 AND cleanup_owner = $2 AND cleanup_fence = $3
		  AND cleanup_lease_expires_at > clock_timestamp()
	`, id, owner, fence)
	if err != nil {
		r.recordWrite(err)
		return fmt.Errorf("complete infrastructure cleanup: %w", err)
	}
	if tag.RowsAffected() != 1 {
		err = fmt.Errorf("infrastructure cleanup fence is stale or expired")
		r.recordWrite(err)
		return err
	}
	r.recordWrite(nil)
	return nil
}

func (r *JobRepository) FailInfraCleanup(ctx context.Context, id, owner string, fence int64, detail string) error {
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	tag, err := r.db.Pool().Exec(ctx, `
		UPDATE infra_instances
		SET state = 'destroy_failed', failure_detail = $4,
		    cleanup_owner = '', cleanup_lease_expires_at = NULL,
		    next_cleanup_at = clock_timestamp()
		      + make_interval(secs => LEAST(300, GREATEST(5, cleanup_attempts * 5))),
		    updated_at = clock_timestamp()
		WHERE id = $1 AND cleanup_owner = $2 AND cleanup_fence = $3
		  AND cleanup_lease_expires_at > clock_timestamp()
	`, id, owner, fence, detail)
	if err != nil {
		r.recordWrite(err)
		return fmt.Errorf("fail infrastructure cleanup: %w", err)
	}
	if tag.RowsAffected() != 1 {
		err = fmt.Errorf("infrastructure cleanup fence is stale or expired")
		r.recordWrite(err)
		return err
	}
	r.recordWrite(nil)
	return nil
}
