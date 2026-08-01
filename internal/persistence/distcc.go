package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/distcc"
)

const (
	maxCompileLeaseDuration = time.Hour
	maxWorkerFreshness      = 10 * time.Minute
)

// HeartbeatCompileWorker inserts or refreshes one exact compile-worker
// inventory row. Compatibility dimensions cannot drift under a stable name;
// endpoint or capacity changes are accepted only while no live lease exists.
func (r *JobRepository) HeartbeatCompileWorker(
	ctx context.Context,
	heartbeat distcc.WorkerHeartbeat,
) (*distcc.Worker, error) {
	if err := heartbeat.Validate(); err != nil {
		return nil, err
	}
	pool, _ := heartbeat.Pool.Normalize()
	poolID, _ := pool.ID()
	workerID := uuid.New()
	worker := &distcc.Worker{Pool: pool}
	err := r.db.Pool().QueryRow(ctx, `
		INSERT INTO compile_workers (
		  id, stable_name, endpoint, pool_id, architecture, chost,
		  compiler_digest, toolchain_image_generation, cpu_features,
		  network_zone, project_trust_domain, max_slots,
		  last_heartbeat_at, updated_at
		) VALUES (
		  $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
		  clock_timestamp(), clock_timestamp()
		)
		ON CONFLICT (stable_name) DO UPDATE SET
		  endpoint = EXCLUDED.endpoint,
		  max_slots = EXCLUDED.max_slots,
		  last_heartbeat_at = clock_timestamp(),
		  updated_at = clock_timestamp()
		WHERE compile_workers.pool_id = EXCLUDED.pool_id
		  AND compile_workers.architecture = EXCLUDED.architecture
		  AND compile_workers.chost = EXCLUDED.chost
		  AND compile_workers.compiler_digest = EXCLUDED.compiler_digest
		  AND compile_workers.toolchain_image_generation =
		      EXCLUDED.toolchain_image_generation
		  AND compile_workers.cpu_features = EXCLUDED.cpu_features
		  AND compile_workers.network_zone = EXCLUDED.network_zone
		  AND compile_workers.project_trust_domain =
		      EXCLUDED.project_trust_domain
		  AND (
		    (compile_workers.endpoint = EXCLUDED.endpoint
		      AND compile_workers.max_slots = EXCLUDED.max_slots)
		    OR NOT EXISTS (
		      SELECT 1 FROM compile_slot_leases lease
		      WHERE lease.worker_id = compile_workers.id
		        AND lease.state = 'active'
		        AND lease.expires_at > clock_timestamp()
		    )
		  )
		RETURNING id::text, stable_name, endpoint, max_slots, pool_id,
		          last_heartbeat_at, next_fence
	`,
		workerID, heartbeat.StableName, heartbeat.Endpoint, poolID,
		pool.Architecture, pool.CHOST, pool.CompilerDigest,
		pool.ToolchainImageGeneration, pool.CPUFeatures, pool.NetworkZone,
		pool.ProjectTrustDomain, heartbeat.MaxSlots,
	).Scan(
		&worker.ID, &worker.StableName, &worker.Endpoint, &worker.MaxSlots,
		&worker.PoolID, &worker.LastHeartbeat, &worker.NextFence,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("compile worker identity changed or live leases prevent endpoint/capacity mutation")
	}
	if err != nil {
		return nil, fmt.Errorf("heartbeat compile worker: %w", err)
	}
	return worker, nil
}

// ReserveCompileSlots atomically locks one compatible worker, rechecks live
// lease usage, increments its fence, and inserts the slot lease. It never
// mutates project fairness or project quota state.
func (r *JobRepository) ReserveCompileSlots(
	ctx context.Context,
	request distcc.ReservationRequest,
) (*distcc.Lease, error) {
	started := time.Now()
	if err := validateCompileReservation(request); err != nil {
		return nil, err
	}
	pool, _ := request.Pool.Normalize()
	poolID, _ := pool.ID()
	var lease *distcc.Lease
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		// Serialize the short capacity decision per exact pool. SKIP LOCKED on
		// the only compatible worker can otherwise report no capacity while a
		// concurrent one-slot reservation is merely committing, leaving usable
		// slots idle. The transaction-scoped lock also gives the following
		// statement a fresh READ COMMITTED snapshot of committed leases.
		if _, err := q.Exec(ctx, `
			SELECT pg_advisory_xact_lock(hashtextextended($1, 0))
		`, poolID); err != nil {
			return fmt.Errorf("lock compile pool: %w", err)
		}
		var workerID, endpoint string
		var fence int64
		err := q.QueryRow(ctx, `
			SELECT w.id::text, w.endpoint, w.next_fence
			FROM compile_workers w
			WHERE w.pool_id = $1
			  AND w.architecture = $2
			  AND w.chost = $3
			  AND w.compiler_digest = $4
			  AND w.toolchain_image_generation = $5
			  AND w.cpu_features = $6
			  AND w.network_zone = $7
			  AND w.project_trust_domain = $8
			  AND w.desired_state = 'active'
			  AND w.last_heartbeat_at >
			      clock_timestamp() - ($9 * interval '1 millisecond')
			  AND w.max_slots - COALESCE((
			    SELECT sum(l.slots)
			    FROM compile_slot_leases l
			    WHERE l.worker_id = w.id
			      AND l.state = 'active'
			      AND l.expires_at > clock_timestamp()
			  ), 0) >= $10
			ORDER BY
			  w.max_slots - COALESCE((
			    SELECT sum(l.slots)
			    FROM compile_slot_leases l
			    WHERE l.worker_id = w.id
			      AND l.state = 'active'
			      AND l.expires_at > clock_timestamp()
			  ), 0) DESC,
			  w.stable_name, w.id
			FOR UPDATE
			LIMIT 1
		`, poolID, pool.Architecture, pool.CHOST, pool.CompilerDigest,
			pool.ToolchainImageGeneration, pool.CPUFeatures, pool.NetworkZone,
			pool.ProjectTrustDomain, request.WorkerFreshness.Milliseconds(),
			request.Slots,
		).Scan(&workerID, &endpoint, &fence)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("select compile worker: %w", err)
		}
		if err := q.QueryRow(ctx, `
			UPDATE compile_workers
			SET next_fence = next_fence + 1, updated_at = clock_timestamp()
			WHERE id = $1
			RETURNING next_fence
		`, workerID).Scan(&fence); err != nil {
			return fmt.Errorf("advance compile worker fence: %w", err)
		}
		leaseID := uuid.NewString()
		var expiresAt time.Time
		if err := q.QueryRow(ctx, `
			INSERT INTO compile_slot_leases (
			  id, worker_id, project_id, job_id, attempt_id, attempt_fence,
			  builder_id,
			  builder_network_identity, fence_token, slots, pool_id,
			  fallback_policy, expires_at
			)
			SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			       clock_timestamp() + ($13 * interval '1 millisecond')
			FROM build_attempts attempt
			JOIN build_jobs job ON job.id = attempt.job_id
			JOIN worker_leases admission
			  ON admission.attempt_id = attempt.id
			 AND admission.fence_token = $6
			 AND admission.expires_at > clock_timestamp()
			WHERE attempt.id = $5 AND attempt.job_id = $4
			  AND attempt.fence_token = $6
			  AND attempt.state NOT IN ('completed', 'success', 'failed', 'canceled', 'expired')
			  AND job.id = $4 AND job.project_id = $3
			  AND job.state NOT IN ('completed', 'success', 'failed', 'canceled')
			RETURNING expires_at
		`, leaseID, workerID, request.ProjectID, request.JobID,
			request.AttemptID, request.AttemptFence, request.BuilderID,
			request.BuilderNetworkIdentity, fence, request.Slots, poolID,
			request.FallbackPolicy, request.LeaseDuration.Milliseconds(),
		).Scan(&expiresAt); err != nil {
			return fmt.Errorf("insert compile slot lease: %w", err)
		}
		lease = &distcc.Lease{
			ID: leaseID, WorkerID: workerID, WorkerEndpoint: endpoint,
			BuilderID:              request.BuilderID,
			BuilderNetworkIdentity: request.BuilderNetworkIdentity,
			AttemptID:              request.AttemptID,
			AttemptFence:           request.AttemptFence,
			Fence:                  fence, Slots: request.Slots, PoolID: poolID, Pool: pool,
			FallbackPolicy: request.FallbackPolicy, Pump: false,
			ExpiresAt: expiresAt,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if lease != nil {
		lease.QueueTime = time.Since(started)
	}
	return lease, nil
}

func validateCompileReservation(request distcc.ReservationRequest) error {
	for name, value := range map[string]string{
		"project": request.ProjectID, "job": request.JobID,
		"attempt": request.AttemptID,
	} {
		if _, err := uuid.Parse(value); err != nil {
			return fmt.Errorf("invalid distcc %s ID", name)
		}
	}
	if request.BuilderID == "" || request.BuilderNetworkIdentity == "" {
		return fmt.Errorf("distcc builder identity is required")
	}
	if request.AttemptFence <= 0 {
		return fmt.Errorf("distcc attempt fence is required")
	}
	if request.Slots < 1 || request.Slots > 256 {
		return fmt.Errorf("distcc requested slots must be between 1 and 256")
	}
	if request.LeaseDuration <= 0 || request.LeaseDuration > maxCompileLeaseDuration {
		return fmt.Errorf("distcc lease duration must be in (0,%s]", maxCompileLeaseDuration)
	}
	if request.WorkerFreshness <= 0 || request.WorkerFreshness > maxWorkerFreshness {
		return fmt.Errorf("distcc worker freshness must be in (0,%s]", maxWorkerFreshness)
	}
	if request.FallbackPolicy != distcc.FallbackLocal &&
		request.FallbackPolicy != distcc.FallbackBlocked {
		return fmt.Errorf("invalid distcc fallback policy %q", request.FallbackPolicy)
	}
	_, err := request.Pool.Normalize()
	return err
}

// HeartbeatCompileLease renews a live lease only when the caller's complete
// builder identity and fence still match and the compile worker is fresh.
func (r *JobRepository) HeartbeatCompileLease(
	ctx context.Context,
	lease distcc.Lease,
	duration, workerFreshness time.Duration,
) error {
	if duration <= 0 || duration > maxCompileLeaseDuration ||
		workerFreshness <= 0 || workerFreshness > maxWorkerFreshness {
		return fmt.Errorf("invalid distcc lease heartbeat duration")
	}
	tag, err := r.db.Pool().Exec(ctx, `
		UPDATE compile_slot_leases lease
		SET expires_at = clock_timestamp() + ($1 * interval '1 millisecond'),
		    renewed_at = clock_timestamp()
		FROM compile_workers worker
		WHERE lease.worker_id = worker.id
		  AND lease.id = $2
		  AND lease.worker_id = $3
		  AND lease.builder_id = $4
		  AND lease.builder_network_identity = $5
		  AND lease.attempt_id = $6
		  AND lease.attempt_fence = $7
		  AND lease.fence_token = $8
		  AND lease.pool_id = $9
		  AND lease.state = 'active'
		  AND lease.expires_at > clock_timestamp()
		  AND EXISTS (
		    SELECT 1 FROM worker_leases admission
		    WHERE admission.attempt_id = lease.attempt_id
		      AND admission.fence_token = lease.attempt_fence
		      AND admission.expires_at > clock_timestamp()
		  )
		  AND worker.desired_state = 'active'
		  AND worker.last_heartbeat_at >
		      clock_timestamp() - ($10 * interval '1 millisecond')
	`, duration.Milliseconds(), lease.ID, lease.WorkerID, lease.BuilderID,
		lease.BuilderNetworkIdentity, lease.AttemptID, lease.AttemptFence,
		lease.Fence, lease.PoolID,
		workerFreshness.Milliseconds())
	if err != nil {
		return fmt.Errorf("heartbeat compile lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("compile lease heartbeat rejected by expiry or fence")
	}
	return nil
}

// CheckCompileLease is the authorization-plane read used immediately before a
// compiler connection and again before accepting remote-derived output.
func (r *JobRepository) CheckCompileLease(
	ctx context.Context,
	lease distcc.Lease,
	workerFreshness time.Duration,
) error {
	if workerFreshness <= 0 || workerFreshness > maxWorkerFreshness {
		return fmt.Errorf("invalid distcc worker freshness")
	}
	var valid bool
	if err := r.db.Pool().QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1
		  FROM compile_slot_leases lease
		  JOIN compile_workers worker ON worker.id = lease.worker_id
		  WHERE lease.id = $1 AND lease.worker_id = $2
		    AND lease.builder_id = $3
		    AND lease.builder_network_identity = $4
		    AND lease.attempt_id = $5 AND lease.attempt_fence = $6
		    AND lease.fence_token = $7 AND lease.pool_id = $8
		    AND lease.state = 'active'
		    AND lease.expires_at > clock_timestamp()
		    AND EXISTS (
		      SELECT 1 FROM worker_leases admission
		      WHERE admission.attempt_id = lease.attempt_id
		        AND admission.fence_token = lease.attempt_fence
		        AND admission.expires_at > clock_timestamp()
		    )
		    AND worker.desired_state = 'active'
		    AND worker.last_heartbeat_at >
		        clock_timestamp() - ($9 * interval '1 millisecond')
		)
	`, lease.ID, lease.WorkerID, lease.BuilderID,
		lease.BuilderNetworkIdentity, lease.AttemptID, lease.AttemptFence,
		lease.Fence, lease.PoolID,
		workerFreshness.Milliseconds()).Scan(&valid); err != nil {
		return fmt.Errorf("check compile lease: %w", err)
	}
	if !valid {
		return fmt.Errorf("compile lease is expired, stale, or fenced")
	}
	return nil
}

// ReleaseCompileLease terminally returns capacity. A stale fence cannot
// release a newer builder's slots.
func (r *JobRepository) ReleaseCompileLease(
	ctx context.Context,
	lease distcc.Lease,
	failureReason string,
) error {
	probe := distcc.Observation{Outcome: distcc.OutcomeLocal, Count: 1,
		FailureReason: failureReason}
	if err := probe.Validate(); err != nil {
		return err
	}
	tag, err := r.db.Pool().Exec(ctx, `
		UPDATE compile_slot_leases
		SET state = CASE
		      WHEN expires_at <= clock_timestamp() THEN 'expired'
		      ELSE 'released' END,
		    released_at = clock_timestamp(), failure_reason = $1
		WHERE id = $2 AND worker_id = $3 AND builder_id = $4
		  AND builder_network_identity = $5 AND attempt_id = $6
		  AND attempt_fence = $7 AND fence_token = $8 AND pool_id = $9
		  AND state = 'active'
	`, failureReason, lease.ID, lease.WorkerID, lease.BuilderID,
		lease.BuilderNetworkIdentity, lease.AttemptID, lease.AttemptFence,
		lease.Fence, lease.PoolID)
	if err != nil {
		return fmt.Errorf("release compile lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("compile lease release rejected by state or fence")
	}
	return nil
}

// RecordCompileObservation persists compiler-wrapper facts without accepting
// arbitrary metric labels.
func (r *JobRepository) RecordCompileObservation(
	ctx context.Context,
	lease distcc.Lease,
	observation distcc.Observation,
) error {
	if err := observation.Validate(); err != nil {
		return err
	}
	tag, err := r.db.Pool().Exec(ctx, `
		INSERT INTO compile_observations (
		  lease_id, outcome, failure_reason, compile_count,
		  network_bytes, queue_millis
		)
		SELECT id, $1, $2, $3, $4, $5
		FROM compile_slot_leases
		WHERE id = $6 AND worker_id = $7 AND builder_id = $8
		  AND builder_network_identity = $9 AND attempt_id = $10
		  AND attempt_fence = $11 AND fence_token = $12
		  AND pool_id = $13 AND state = 'active'
		  AND expires_at > clock_timestamp()
	`, observation.Outcome, observation.FailureReason, observation.Count,
		observation.NetworkBytes, observation.QueueMillis, lease.ID,
		lease.WorkerID, lease.BuilderID, lease.BuilderNetworkIdentity,
		lease.AttemptID, lease.AttemptFence, lease.Fence, lease.PoolID)
	if err != nil {
		return fmt.Errorf("record compile observation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("compile observation rejected by lease fence")
	}
	return nil
}

// CompileMetrics returns a bounded aggregate suitable for Prometheus. The
// caller selects the freshness and observation window; no identity is exposed.
func (r *JobRepository) CompileMetrics(
	ctx context.Context,
	workerFreshness, observationWindow time.Duration,
) (distcc.MetricsSnapshot, error) {
	if workerFreshness <= 0 || workerFreshness > maxWorkerFreshness ||
		observationWindow <= 0 || observationWindow > 30*24*time.Hour {
		return distcc.MetricsSnapshot{}, fmt.Errorf("invalid distcc metrics window")
	}
	snapshot := distcc.MetricsSnapshot{FailuresByReason: make(map[string]int64)}
	if err := r.db.Pool().QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(max_slots), 0), COALESCE((
		  SELECT sum(lease.slots)
		  FROM compile_slot_leases lease
		  JOIN compile_workers leased_worker
		    ON leased_worker.id = lease.worker_id
		  WHERE lease.state = 'active'
		    AND lease.expires_at > clock_timestamp()
		    AND leased_worker.desired_state = 'active'
		    AND leased_worker.last_heartbeat_at >
		        clock_timestamp() - ($1 * interval '1 millisecond')
		), 0)
		FROM compile_workers
		WHERE desired_state = 'active'
		  AND last_heartbeat_at >
		      clock_timestamp() - ($1 * interval '1 millisecond')
	`, workerFreshness.Milliseconds()).Scan(
		&snapshot.WorkersFresh, &snapshot.SlotsTotal, &snapshot.SlotsLeased,
	); err != nil {
		return distcc.MetricsSnapshot{}, fmt.Errorf("read compile inventory metrics: %w", err)
	}
	rows, err := r.db.Pool().Query(ctx, `
		SELECT outcome, failure_reason, sum(compile_count),
		       sum(network_bytes), sum(queue_millis)
		FROM compile_observations
		WHERE observed_at >
		      clock_timestamp() - ($1 * interval '1 millisecond')
		GROUP BY outcome, failure_reason
	`, observationWindow.Milliseconds())
	if err != nil {
		return distcc.MetricsSnapshot{}, fmt.Errorf("read compile observation metrics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var outcome, reason string
		var count, bytes, queue int64
		if err := rows.Scan(&outcome, &reason, &count, &bytes, &queue); err != nil {
			return distcc.MetricsSnapshot{}, fmt.Errorf("scan compile observation metrics: %w", err)
		}
		switch outcome {
		case distcc.OutcomeLocal:
			snapshot.LocalCompiles += count
		case distcc.OutcomeRemote:
			snapshot.RemoteCompiles += count
		case distcc.OutcomeHit:
			snapshot.Hits += count
		case distcc.OutcomeFallback:
			snapshot.Fallbacks += count
		}
		snapshot.NetworkBytes += bytes
		snapshot.QueueMillis += queue
		if reason != "" {
			snapshot.FailuresByReason[reason] += count
		}
	}
	if err := rows.Err(); err != nil {
		return distcc.MetricsSnapshot{}, fmt.Errorf("iterate compile observation metrics: %w", err)
	}
	return snapshot, nil
}
