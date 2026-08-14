package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
)

const schedulerRecoveryBatch = 32

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func decodeStoredRequest(data []byte) (*builder.BuildRequest, error) {
	var stored storedBuildRequest
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("decode stored build request: %w", err)
	}
	return &builder.BuildRequest{
		ProjectID: stored.ProjectID, RequestedBy: stored.RequestedBy,
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
func (r *JobRepository) ClaimNext(
	ctx context.Context,
	stableWorker string,
	leaseDuration time.Duration,
	executorCapabilities ...[]string,
) (*builder.SchedulerClaim, error) {
	if stableWorker == "" {
		return nil, fmt.Errorf("stable worker name is required")
	}
	if leaseDuration <= 0 {
		return nil, fmt.Errorf("lease duration must be positive")
	}
	capabilities := []string{"phase:provision", "context:legacy"}
	if len(executorCapabilities) > 0 {
		capabilities = executorCapabilities[0]
	}
	capabilities, err := builder.NormalizeExecutorCapabilities(capabilities)
	if err != nil {
		return nil, err
	}
	leaseKind := schedulerLeaseKind(capabilities)
	var claim *builder.SchedulerClaim
	err = r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		workerID, err := r.schedulerWorker(ctx, q, stableWorker, capabilities)
		if err != nil {
			return err
		}

		if _, err := r.recoverExpiredTx(ctx, q, schedulerRecoveryBatch); err != nil {
			return err
		}
		claim, err = r.claimNextTx(
			ctx, q, workerID, stableWorker, leaseKind, leaseDuration,
		)
		return err
	})
	if err != nil || claim != nil {
		r.recordWrite(err)
	}
	return claim, err
}

func schedulerLeaseKind(capabilities []string) string {
	for _, capability := range capabilities {
		if capability == "worker-kind:admission" {
			return "admission"
		}
	}
	return "attempt"
}

type claimCandidate struct {
	jobID                  uuid.UUID
	projectID, requestedBy string
	maxAttempts            int
	createdAt              time.Time
	fairness               fairProjectSelection
	request                *builder.BuildRequest
	status                 *builder.BuildStatus
	workerCandidateCount   int
	workerPressureScore    int
	workerRecentFailures   int
}

func readDatabaseClock(
	ctx context.Context,
	q Querier,
) (time.Time, error) {
	var now time.Time
	if err := q.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	return now.UTC(), nil
}

func selectClaimCandidate(
	ctx context.Context,
	q Querier,
	workerID uuid.UUID,
) (claimCandidate, bool, error) {
	var candidate claimCandidate
	fairness, found, err := selectFairAdmissionProject(ctx, q, workerID)
	if err != nil || !found {
		return candidate, found, err
	}
	candidate.fairness = fairness
	var requestJSON, statusJSON []byte
	err = q.QueryRow(ctx, `
			SELECT j.id, j.project_id::text,
			       COALESCE(j.requested_by_subject_id::text, ''),
			       j.request, j.status_snapshot, j.max_attempts,
			       j.created_at, selected_worker.candidate_count,
			       selected_worker.pressure_score,
			       selected_worker.recent_failures
			FROM build_jobs j
			JOIN project_policies pp ON pp.project_id = j.project_id
			JOIN workers executor ON executor.id = $1
			JOIN LATERAL (
			  SELECT scored.id, scored.pressure_score,
			         scored.recent_failures, scored.candidate_count
			  FROM (
			    SELECT ranked.*,
			           count(*) OVER ()::integer AS candidate_count
			    FROM (
			    SELECT candidate.id,
			      CASE
			        WHEN candidate.capabilities ->> 'pressure_score'
			             ~ '^[0-9]{1,4}$'
			        THEN LEAST(
			          1000,
			          (candidate.capabilities ->> 'pressure_score')::integer
			        )
			        ELSE 500
			      END AS pressure_score,
			      (
			        SELECT count(*)::integer
			        FROM build_attempts recent
			        WHERE recent.worker_id = candidate.id
			          AND recent.state = 'failed'
			          AND recent.updated_at >
			              clock_timestamp() - interval '1 hour'
			      ) AS recent_failures
			    FROM workers candidate
			    WHERE candidate.desired_state = 'active'
			      AND candidate.last_seen_at >
			          clock_timestamp() - interval '5 seconds'
			      AND candidate.executor_protocol >=
			          j.minimum_executor_protocol
			      AND NOT EXISTS (
			        SELECT 1
			        FROM jsonb_array_elements_text(
			          j.required_capabilities
			        ) required(label)
			        WHERE NOT (
			          COALESCE(
			            candidate.capabilities -> 'labels', '[]'::jsonb
			          ) ? required.label
			        )
			      )
			      AND (
			        COALESCE(
			          candidate.capabilities -> 'labels', '[]'::jsonb
			        ) ? 'worker-kind:admission'
			        OR NOT EXISTS (
			          SELECT 1 FROM worker_leases busy
			          WHERE busy.worker_id = candidate.id
			            AND busy.expires_at > clock_timestamp()
			        )
			      )
			      AND (
			        NOT EXISTS (
			          SELECT 1
			          FROM jsonb_array_elements_text(
			            COALESCE(
			              executor.capabilities -> 'labels', '[]'::jsonb
			            )
			          ) current_kind(label)
			          WHERE current_kind.label LIKE 'worker-kind:%'
			        )
			        OR EXISTS (
			          SELECT 1
			          FROM jsonb_array_elements_text(
			            COALESCE(
			              executor.capabilities -> 'labels', '[]'::jsonb
			            )
			          ) current_kind(label)
			          WHERE current_kind.label LIKE 'worker-kind:%'
			            AND COALESCE(
			              candidate.capabilities -> 'labels', '[]'::jsonb
			            ) ? current_kind.label
			        )
			      )
			    ) ranked
			  ) scored
			  WHERE scored.id = executor.id
			) selected_worker ON true
			WHERE j.project_id = $2
			  AND j.state = 'queued'
			  AND j.cancel_requested_at IS NULL
			  AND j.legacy_visible = true
			  AND j.next_attempt_at <= clock_timestamp()
			  AND pp.suspended = false
			  AND (
			    pp.abuse_suspended_until IS NULL
			    OR pp.abuse_suspended_until <= clock_timestamp()
			  )
			  AND (
				SELECT count(*)
				FROM build_jobs active
				WHERE active.project_id = j.project_id
				  AND active.legacy_visible = true
				  AND active.state IN (
					'claimed', 'provisioning', 'forwarding', 'deploying', 'building',
					'collecting', 'verifying', 'signing', 'publishing'
				  )
			  ) < pp.max_active_jobs
			  AND executor.desired_state = 'active'
			  AND executor.last_seen_at >
			      clock_timestamp() - interval '45 seconds'
			  AND executor.executor_protocol >= j.minimum_executor_protocol
			  AND NOT EXISTS (
			    SELECT 1
			    FROM jsonb_array_elements_text(
			      j.required_capabilities
			    ) AS required(label)
			    WHERE NOT (
			      COALESCE(executor.capabilities -> 'labels', '[]'::jsonb)
			      ? required.label
			    )
			  )
			ORDER BY j.priority DESC, j.next_attempt_at, j.created_at, j.id
			LIMIT 1
			FOR UPDATE OF j SKIP LOCKED
		`, workerID, fairness.ProjectID).Scan(
		&candidate.jobID, &candidate.projectID, &candidate.requestedBy,
		&requestJSON, &statusJSON, &candidate.maxAttempts,
		&candidate.createdAt, &candidate.workerCandidateCount,
		&candidate.workerPressureScore, &candidate.workerRecentFailures,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return claimCandidate{}, false, nil
	}
	if err != nil {
		return claimCandidate{}, false, fmt.Errorf("select claimable job: %w", err)
	}
	candidate.request, err = decodeStoredRequest(requestJSON)
	if err != nil {
		return claimCandidate{}, false, err
	}
	candidate.request.ProjectID = candidate.projectID
	candidate.request.RequestedBy = candidate.requestedBy
	candidate.status, err = decodeStoredStatus(statusJSON, candidate.request)
	if err != nil {
		return claimCandidate{}, false, err
	}
	candidate.status.JobID = candidate.jobID.String()
	candidate.status.ProjectID = candidate.projectID
	candidate.status.RequestedBy = candidate.requestedBy
	return candidate, true, nil
}

func allocateAttemptFence(
	ctx context.Context,
	q Querier,
	jobID uuid.UUID,
	maxAttempts int,
) (int, int64, error) {
	var attemptNo int
	var fenceToken int64
	if err := q.QueryRow(ctx, `
			SELECT COALESCE(max(attempt_no), 0) + 1,
			       COALESCE(max(fence_token), 0) + 1
			FROM build_attempts
			WHERE job_id = $1
		`, jobID).Scan(&attemptNo, &fenceToken); err != nil {
		return 0, 0, fmt.Errorf("allocate attempt fence: %w", err)
	}
	if attemptNo > maxAttempts {
		return 0, 0, fmt.Errorf("job %s exceeded max attempts", jobID)
	}
	return attemptNo, fenceToken, nil
}

func (r *JobRepository) claimNextTx(
	ctx context.Context,
	q Querier,
	workerID uuid.UUID,
	stableWorker string,
	leaseKind string,
	leaseDuration time.Duration,
) (*builder.SchedulerClaim, error) {
	candidate, found, err := selectClaimCandidate(ctx, q, workerID)
	if err != nil || !found {
		return nil, err
	}
	resources, runtimeBudget, admitted, err := prepareClaimAccounting(
		ctx, q, candidate.projectID, candidate.jobID, candidate.request,
	)
	if err != nil || !admitted {
		return nil, err
	}
	attemptNo, fenceToken, err := allocateAttemptFence(
		ctx, q, candidate.jobID, candidate.maxAttempts,
	)
	if err != nil {
		return nil, err
	}
	claim, err := r.persistClaim(
		ctx, q, candidate, resources, runtimeBudget,
		workerID, stableWorker, leaseKind, leaseDuration, attemptNo, fenceToken,
	)
	if err != nil {
		return nil, err
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO scheduler_worker_decisions (
		  work_kind, job_id, phase, worker_id, candidate_count,
		  pressure_score, recent_failures, reason
		) VALUES (
		  'admission', $1, '', $2, $3, $4, $5,
		  'eligible polling worker; pressure and recent failures observed'
		)
	`, candidate.jobID, workerID, candidate.workerCandidateCount,
		candidate.workerPressureScore, candidate.workerRecentFailures); err != nil {
		return nil, fmt.Errorf("record admission worker decision: %w", err)
	}
	dispatch, err := recordFairDispatch(
		ctx, q, candidate.fairness, "admission", candidate.createdAt,
	)
	if err != nil {
		return nil, err
	}
	if err := r.insertJobEvent(
		ctx, q, candidate.jobID.String(), "scheduler.fair_dispatch",
		map[string]any{
			"kind": "admission", "attempt_id": claim.Status.AttemptID,
			"priority_weight":              candidate.fairness.PriorityWeight,
			"starvation_threshold_seconds": candidate.fairness.StarvationThreshold,
			"starved":                      candidate.fairness.Starved,
			"project_oldest_queued_at":     candidate.fairness.OldestAt,
			"virtual_runtime_before":       candidate.fairness.VRuntimeBefore,
			"virtual_runtime_after":        dispatch.VRuntimeAfter,
			"item_wait_seconds":            dispatch.ItemWaitSecond,
		},
	); err != nil {
		return nil, err
	}
	return claim, nil
}

func (r *JobRepository) persistClaim(
	ctx context.Context,
	q Querier,
	candidate claimCandidate,
	resources resourceVector,
	runtimeBudget runtimeBudgetSpec,
	workerID uuid.UUID,
	stableWorker string,
	leaseKind string,
	leaseDuration time.Duration,
	attemptNo int,
	fenceToken int64,
) (*builder.SchedulerClaim, error) {
	now, err := readDatabaseClock(ctx, q)
	if err != nil {
		return nil, err
	}
	attemptID := uuid.New()
	if _, err := q.Exec(ctx, `
			INSERT INTO build_attempts (
				id, job_id, attempt_no, state, worker_id, fence_token,
				started_at, created_at, updated_at
			) VALUES ($1, $2, $3, 'claimed', $4, $5, $6, $6, $6)
		`, attemptID, candidate.jobID, attemptNo, workerID, fenceToken, now); err != nil {
		return nil, fmt.Errorf("insert claimed attempt: %w", err)
	}
	if _, err := q.Exec(ctx, `
			INSERT INTO project_resource_reservations (
				attempt_id, project_id, job_id, resource_class,
				vcpus, memory_mib, disk_gib, phase, state,
				reserved_at, phase_updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, 'claimed', 'active', $8, $8)
		`, attemptID, candidate.projectID, candidate.jobID, candidate.request.ResourceClass,
		resources.VCPUs, resources.MemoryMiB, resources.DiskGiB, now); err != nil {
		return nil, fmt.Errorf("reserve project build resources: %w", err)
	}
	if err := insertAttemptUsage(
		ctx, q, attemptID, candidate.projectID, candidate.jobID,
		runtimeBudget, now,
	); err != nil {
		return nil, err
	}
	if err := insertPhaseWorkPlan(
		ctx, q, candidate.projectID, candidate.jobID, attemptID,
		candidate.request, now,
	); err != nil {
		return nil, err
	}
	if _, err := q.Exec(ctx, `
			INSERT INTO worker_leases (
				id, worker_id, attempt_id, fence_token, lease_kind,
				expires_at, renewed_at, created_at
			) VALUES (
				$1, $2, $3, $4, $5,
				LEAST($6::timestamptz, $8::timestamptz),
				$7::timestamptz, $7::timestamptz
			)
		`, uuid.New(), workerID, attemptID, fenceToken, leaseKind,
		now.Add(leaseDuration), now,
		now.Add(time.Duration(runtimeBudget.MaxRuntimeSeconds)*time.Second)); err != nil {
		return nil, fmt.Errorf("insert worker lease: %w", err)
	}

	status := candidate.status
	status.Status = "claimed"
	status.UpdatedAt = now
	status.Error = ""
	status.AttemptID = attemptID.String()
	status.FenceToken = fenceToken
	status.LeaseOwner = stableWorker
	snapshot, digest, err := statusDocument(status)
	if err != nil {
		return nil, err
	}
	if _, err := q.Exec(ctx, `
			UPDATE build_jobs
			SET state = 'claimed', status_snapshot = $2::jsonb, status_digest = $3,
			    updated_at = $4, terminal_reason = '',
			    ledger_revision = ledger_revision + 1
			WHERE id = $1
		`, candidate.jobID, string(snapshot), digest, now); err != nil {
		return nil, fmt.Errorf("mark job claimed: %w", err)
	}
	if err := r.insertJobEvent(ctx, q, candidate.jobID.String(), "job.claimed", map[string]any{
		"attempt_id": attemptID, "attempt_no": attemptNo, "fence_token": fenceToken,
		"worker": stableWorker,
		"lease_expires_at": minTime(
			now.Add(leaseDuration),
			now.Add(time.Duration(runtimeBudget.MaxRuntimeSeconds)*time.Second),
		),
		"reserved_build_seconds":         runtimeBudget.MaxRuntimeSeconds,
		"reserved_cloud_cost_microunits": runtimeBudget.ReservedCloudCostMicrounits,
	}); err != nil {
		return nil, err
	}
	return &builder.SchedulerClaim{Request: candidate.request, Status: status}, nil
}

func prepareClaimAccounting(
	ctx context.Context,
	q Querier,
	projectID string,
	jobID uuid.UUID,
	request *builder.BuildRequest,
) (resourceVector, runtimeBudgetSpec, bool, error) {
	resources, err := resourceVectorFromRequest(request)
	if err != nil {
		return resourceVector{}, runtimeBudgetSpec{}, false,
			fmt.Errorf("decode claim resource request: %w", err)
	}
	runtimeBudget, err := runtimeBudgetSpecFromRequest(request)
	if err != nil {
		return resourceVector{}, runtimeBudgetSpec{}, false,
			fmt.Errorf("decode claim runtime budget: %w", err)
	}
	allowed, retryAt, err := lockAndCheckClaimAdmission(
		ctx, q, projectID, jobID.String(), resources, runtimeBudget,
	)
	if err != nil {
		return resourceVector{}, runtimeBudgetSpec{}, false, err
	}
	if allowed {
		return resources, runtimeBudget, true, nil
	}
	// Another worker may have filled the final slot while this transaction
	// waited on the policy lock. Runtime-budget exhaustion backs off to the
	// next UTC day; transient capacity uses a short database-clock delay.
	var retryAtValue any
	if !retryAt.IsZero() {
		retryAtValue = retryAt
	}
	if _, err := q.Exec(ctx, `
		UPDATE build_jobs
		SET next_attempt_at = GREATEST(
		      COALESCE($2::timestamptz, '-infinity'::timestamptz),
		      clock_timestamp() + interval '250 milliseconds'
		    )
		WHERE id = $1 AND state = 'queued'
	`, jobID, retryAtValue); err != nil {
		return resourceVector{}, runtimeBudgetSpec{}, false,
			fmt.Errorf("defer capacity-blocked job: %w", err)
	}
	return resources, runtimeBudget, false, nil
}

func insertPhaseWorkPlan(
	ctx context.Context,
	q Querier,
	projectID string,
	jobID, attemptID uuid.UUID,
	request *builder.BuildRequest,
	createdAt time.Time,
) error {
	requirements := make([]string, 4)
	for index, phase := range []string{"provision", "build", "verify", "publish"} {
		labels, err := builder.PhaseCapabilityRequirements(request, phase)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(labels)
		if err != nil {
			return fmt.Errorf("marshal %s phase requirements: %w", phase, err)
		}
		requirements[index] = string(encoded)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO phase_work_items (
			id, project_id, job_id, attempt_id, phase, sequence,
			state, required_capabilities, available_at, ready_since,
			created_at, updated_at
		) VALUES
			($1, $5, $6, $7, 'provision', 10, 'ready',   $9::jsonb, $8, $8, $8, $8),
			($2, $5, $6, $7, 'build',     20, 'blocked', $10::jsonb, $8, NULL, $8, $8),
			($3, $5, $6, $7, 'verify',    30, 'blocked', $11::jsonb, $8, NULL, $8, $8),
			($4, $5, $6, $7, 'publish',   40, 'blocked', $12::jsonb, $8, NULL, $8, $8)
	`, uuid.New(), uuid.New(), uuid.New(), uuid.New(),
		projectID, jobID, attemptID, createdAt,
		requirements[0], requirements[1], requirements[2], requirements[3],
	); err != nil {
		return fmt.Errorf("create durable phase work plan: %w", err)
	}
	return nil
}

func lockAndCheckClaimAdmission(
	ctx context.Context,
	q Querier,
	projectID, excludeJobID string,
	request resourceVector,
	runtimeBudget runtimeBudgetSpec,
) (bool, time.Time, error) {
	var suspended bool
	var abuseUntil *time.Time
	var (
		maxActive, maxVCPUs, maxMemoryMiB, maxDiskGiB, maxClaimed int
		maxBuildSeconds, maxCloudCost                             int64
		now                                                       time.Time
	)
	if err := q.QueryRow(ctx, `
		SELECT suspended, max_active_jobs,
		       max_active_vcpus, max_active_memory_mib, max_active_disk_gib,
		       max_claimed_attempts,
		       max_daily_build_seconds, max_daily_cloud_cost_microunits,
		       abuse_suspended_until, clock_timestamp()
		FROM project_policies
		WHERE project_id = $1
		FOR UPDATE
	`, projectID).Scan(
		&suspended, &maxActive, &maxVCPUs, &maxMemoryMiB, &maxDiskGiB,
		&maxClaimed, &maxBuildSeconds, &maxCloudCost, &abuseUntil, &now,
	); err != nil {
		return false, time.Time{}, fmt.Errorf("lock project claim policy: %w", err)
	}
	if suspended {
		return false, time.Time{}, nil
	}
	if abuseUntil != nil && abuseUntil.After(now) {
		return false, *abuseUntil, nil
	}
	var active int
	if err := q.QueryRow(ctx, `
		SELECT count(*)
		FROM build_jobs
		WHERE project_id = $1
		  AND id <> $2
		  AND legacy_visible = true
		  AND state IN (
			'claimed', 'provisioning', 'forwarding', 'deploying', 'building',
			'collecting', 'verifying', 'signing', 'publishing'
		  )
	`, projectID, excludeJobID).Scan(&active); err != nil {
		return false, time.Time{}, fmt.Errorf("read project active-build usage: %w", err)
	}
	if active >= maxActive {
		return false, time.Time{}, nil
	}
	var used resourceVector
	var claimed int
	if err := q.QueryRow(ctx, `
		SELECT
			COALESCE(sum(vcpus), 0),
			COALESCE(sum(memory_mib), 0),
			COALESCE(sum(disk_gib), 0),
			count(*) FILTER (WHERE phase = 'claimed')
		FROM project_resource_reservations
		WHERE project_id = $1 AND state = 'active'
	`, projectID).Scan(
		&used.VCPUs, &used.MemoryMiB, &used.DiskGiB, &claimed,
	); err != nil {
		return false, time.Time{}, fmt.Errorf("read project resource reservations: %w", err)
	}
	if claimed >= maxClaimed {
		return false, time.Time{}, nil
	}
	resourceAllowed := used.VCPUs+request.VCPUs <= maxVCPUs &&
		used.MemoryMiB+request.MemoryMiB <= maxMemoryMiB &&
		used.DiskGiB+request.DiskGiB <= maxDiskGiB
	if !resourceAllowed {
		return false, time.Time{}, nil
	}
	var usedBuildSeconds, usedCloudCost int64
	if err := q.QueryRow(ctx, `
		SELECT
			COALESCE(sum(
			  CASE WHEN state = 'active'
			       THEN reserved_build_seconds ELSE charged_build_seconds END
			), 0),
			COALESCE(sum(
			  CASE WHEN state = 'active'
			       THEN reserved_cloud_cost_microunits
			       ELSE charged_cloud_cost_microunits END
			), 0)
		FROM project_attempt_usage
		WHERE project_id = $1
		  AND budget_day = ($2::timestamptz AT TIME ZONE 'UTC')::date
	`, projectID, now).Scan(&usedBuildSeconds, &usedCloudCost); err != nil {
		return false, time.Time{}, fmt.Errorf("read project runtime budget usage: %w", err)
	}
	if usedBuildSeconds+runtimeBudget.MaxRuntimeSeconds > maxBuildSeconds ||
		usedCloudCost+runtimeBudget.ReservedCloudCostMicrounits > maxCloudCost {
		dayEnd := time.Date(
			now.UTC().Year(), now.UTC().Month(), now.UTC().Day()+1,
			0, 0, 0, 0, time.UTC,
		)
		return false, dayEnd, nil
	}
	return true, time.Time{}, nil
}

func (r *JobRepository) schedulerWorker(
	ctx context.Context,
	q Querier,
	stableName string,
	labels []string,
) (uuid.UUID, error) {
	r.workerMu.Lock()
	defer r.workerMu.Unlock()
	telemetry := builder.CaptureExecutorTelemetry(labels)
	capabilities, err := json.Marshal(map[string]any{
		"role":              "control-plane-admission",
		"executor_protocol": builder.ExecutorProtocolVersion,
		"labels":            labels,
		"pressure_score":    telemetry.PressureScore,
		"cache_keys":        telemetry.CacheKeys,
		"telemetry":         telemetry,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal scheduler capabilities: %w", err)
	}
	capabilitiesKey := string(capabilities)
	now := time.Now().UTC()
	if cached, ok := r.workerIDs[stableName]; ok &&
		cached.CapabilitiesKey == capabilitiesKey &&
		now.Sub(cached.Heartbeat) < 15*time.Second {
		return cached.ID, nil
	}
	workerID := uuid.New()
	if cached, ok := r.workerIDs[stableName]; ok {
		workerID = cached.ID
	}
	if err := q.QueryRow(ctx, `
		INSERT INTO workers (
			id, stable_name, desired_state, capabilities, max_slots,
			last_seen_at, executor_protocol
		)
		VALUES (
			$1, $2, 'active',
			$3::jsonb, 1, clock_timestamp(), $4
		)
		ON CONFLICT (stable_name)
		DO UPDATE SET last_seen_at = clock_timestamp(),
		              capabilities = EXCLUDED.capabilities,
		              executor_protocol = EXCLUDED.executor_protocol,
		              generation = workers.generation + CASE
		                WHEN workers.capabilities -> 'labels' IS DISTINCT FROM
		                     EXCLUDED.capabilities -> 'labels'
		                  OR workers.capabilities ->> 'role' IS DISTINCT FROM
		                     EXCLUDED.capabilities ->> 'role'
		                  OR workers.executor_protocol IS DISTINCT FROM
		                     EXCLUDED.executor_protocol
		                THEN 1 ELSE 0 END,
		              updated_at = clock_timestamp()
		RETURNING id
	`, workerID, stableName, capabilitiesKey, builder.ExecutorProtocolVersion).
		Scan(&workerID); err != nil {
		return uuid.Nil, fmt.Errorf("register scheduler worker: %w", err)
	}
	r.workerIDs[stableName] = cachedWorker{
		ID: workerID, Heartbeat: now, CapabilitiesKey: capabilitiesKey,
	}
	return workerID, nil
}

func (r *JobRepository) PruneStaleWorkers(ctx context.Context, before time.Time) (int64, error) {
	var pruned int64
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		rows, err := q.Query(ctx, `
			SELECT w.id
			FROM workers w
			WHERE (w.last_seen_at IS NULL OR w.last_seen_at < $1)
			  AND NOT EXISTS (
			    SELECT 1 FROM worker_leases l WHERE l.worker_id = w.id
			  )
			FOR UPDATE OF w SKIP LOCKED
		`, before)
		if err != nil {
			return err
		}
		var workerIDs []uuid.UUID
		for rows.Next() {
			var workerID uuid.UUID
			if err := rows.Scan(&workerID); err != nil {
				rows.Close()
				return err
			}
			workerIDs = append(workerIDs, workerID)
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		if len(workerIDs) == 0 {
			return nil
		}
		// Worker-scoring evidence deliberately uses ON DELETE RESTRICT so an
		// accidental direct delete cannot erase audit context. Pruning is the
		// authorized lifecycle path: remove that evidence in the same locked
		// transaction before deleting the stale worker rows.
		if _, err := q.Exec(ctx, `
			DELETE FROM scheduler_worker_decisions
			WHERE worker_id = ANY($1::uuid[])
		`, workerIDs); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `
			DELETE FROM workers w
			WHERE w.id = ANY($1::uuid[])
			  AND NOT EXISTS (
			    SELECT 1 FROM worker_leases l WHERE l.worker_id = w.id
			  )
		`, workerIDs)
		if err != nil {
			return err
		}
		pruned = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("prune stale scheduler workers: %w", err)
	}
	return pruned, nil
}

type expiredLeaseRow struct {
	leaseID, attemptID, jobID uuid.UUID
	attemptNo, maxAttempts    int
	canceled                  bool
	leaseKind                 string
	statusJSON                []byte
}

func loadExpiredLeases(ctx context.Context, q Querier, limit int) ([]expiredLeaseRow, error) {
	rows, err := q.Query(ctx, `
		SELECT l.id, a.id, a.job_id, a.attempt_no, j.max_attempts,
		       j.cancel_requested_at IS NOT NULL, l.lease_kind,
		       j.status_snapshot
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
		return nil, fmt.Errorf("select expired leases: %w", err)
	}
	defer rows.Close()

	var expired []expiredLeaseRow
	for rows.Next() {
		var row expiredLeaseRow
		if err := rows.Scan(&row.leaseID, &row.attemptID, &row.jobID, &row.attemptNo,
			&row.maxAttempts, &row.canceled, &row.leaseKind, &row.statusJSON); err != nil {
			return nil, err
		}
		expired = append(expired, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return expired, nil
}

func leaseExpiryResult(state string) string {
	switch state {
	case "queued":
		return "requeued"
	case "failed", "canceled":
		return state
	default:
		return ""
	}
}

// leaseExpiryKey identifies one row of scheduler_lease_expiry_counters. The
// set is fixed and small — seven rows — which is what makes a global lock order
// over them cheap to state and cheap to keep.
type leaseExpiryKey struct{ leaseKind, result string }

// flushLeaseExpiries applies a whole recovery batch's counter updates in one
// fixed order.
//
// Every replica runs the same recovery loop, and each used to update a counter
// row per recovered lease, in whatever order loadExpiredLeases happened to
// return them — which is by expires_at, so two replicas recovering overlapping
// kinds routinely took the same two rows in opposite orders. That is a lock
// cycle: PostgreSQL breaks it with 40P01 and rolls back a whole transaction,
// and this transaction is the one that requeues expired work, so a deadlock
// costs an entire round of recovery rather than one counter increment.
//
// Sorting gives the rows a global order, so two batches can only ever queue
// behind each other. Folding the batch into one UPDATE per row is what makes
// the ordered section short: the locks are taken once, at the end, and held for
// the remainder of a transaction that has already done all its other work.
func flushLeaseExpiries(
	ctx context.Context,
	q Querier,
	tally map[leaseExpiryKey]int64,
) error {
	ordered := make([]leaseExpiryKey, 0, len(tally))
	for key := range tally {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].leaseKind != ordered[right].leaseKind {
			return ordered[left].leaseKind < ordered[right].leaseKind
		}
		return ordered[left].result < ordered[right].result
	})
	for _, key := range ordered {
		if err := recordLeaseExpiry(ctx, q, key.leaseKind, key.result, tally[key]); err != nil {
			return err
		}
	}
	return nil
}

// withDeferredLeaseExpiries adapts a transaction body that tallies lease
// expiries into one that applies them last.
//
// The order only holds if every writer keeps it. ClaimPhaseWork used to reclaim
// expired phase leases and update the counter at the top of its transaction,
// before it locked any phase_work_items row, while the recovery loop locks
// phase_work_items first — the cancel path rewrites them — and the counters at
// the end. That is the same lock cycle stated above with the rows swapped, so
// the two paths could deadlock on the pair even though each was internally
// ordered. Routing both through this wrapper is what makes "counters last" a
// property of the package rather than of one function.
func withDeferredLeaseExpiries(
	ctx context.Context,
	body func(q Querier, tally map[leaseExpiryKey]int64) error,
) func(Querier) error {
	return func(q Querier) error {
		tally := make(map[leaseExpiryKey]int64, 1)
		if err := body(q, tally); err != nil {
			return err
		}
		return flushLeaseExpiries(ctx, q, tally)
	}
}

// validLeaseExpiryCounter reports whether the pair names one of the fixed rows.
func validLeaseExpiryCounter(leaseKind, result string) bool {
	if leaseKind == "phase" {
		return result == "reclaimed"
	}
	return (leaseKind == "attempt" || leaseKind == "admission") &&
		(result == "requeued" || result == "failed" || result == "canceled")
}

func recordLeaseExpiry(
	ctx context.Context,
	q Querier,
	leaseKind, result string,
	count int64,
) error {
	if !validLeaseExpiryCounter(leaseKind, result) || count <= 0 {
		return fmt.Errorf(
			"invalid lease expiry counter update kind=%q result=%q count=%d",
			leaseKind, result, count,
		)
	}
	tag, err := q.Exec(ctx, `
		UPDATE scheduler_lease_expiry_counters
		SET total = total + $3,
		    last_occurred_at = clock_timestamp()
		WHERE lease_kind = $1 AND result = $2
	`, leaseKind, result, count)
	if err != nil {
		return fmt.Errorf("record lease expiry counter: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf(
			"lease expiry counter is missing for kind=%q result=%q",
			leaseKind, result,
		)
	}
	return nil
}

func expiredLeaseOutcome(row expiredLeaseRow) (string, string) {
	if row.canceled {
		return "canceled", "job canceled while executor lease was active"
	}
	if row.attemptNo >= row.maxAttempts {
		return "failed", "executor lease expired and maximum attempts were exhausted"
	}
	return "queued", "previous executor lease expired; job queued for a fenced retry"
}

// recoverExpiredLease requeues, fails or cancels one expired lease and tallies
// the counter row it belongs to. The counter is written by flushLeaseExpiries
// once the whole batch is recovered, so this function takes no lock that
// another replica's batch could be holding in the other direction.
func (r *JobRepository) recoverExpiredLease(
	ctx context.Context,
	q Querier,
	row expiredLeaseRow,
	tally map[leaseExpiryKey]int64,
) error {
	now, err := readDatabaseClock(ctx, q)
	if err != nil {
		return err
	}
	state, reason := expiredLeaseOutcome(row)
	status, err := decodeStoredStatus(row.statusJSON, nil)
	if err != nil {
		return err
	}
	status.Status = state
	status.Error = reason
	status.UpdatedAt = now
	status.AttemptID = ""
	status.FenceToken = 0
	status.LeaseOwner = ""
	snapshot, digest, err := statusDocument(status)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `
			UPDATE build_attempts
			SET state = 'expired', failure_class = 'lease_expired',
			    failure_detail = $2, finished_at = $3, updated_at = $3
			WHERE id = $1
		`, row.attemptID, reason, now); err != nil {
		return err
	}
	if err := settleAttemptUsage(
		ctx, q, row.attemptID.String(), "lease_expired", "expired", now,
	); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `DELETE FROM worker_leases WHERE id = $1`, row.leaseID); err != nil {
		return err
	}
	if err := releaseResourceReservation(
		ctx, q, row.attemptID.String(), "lease_expired", now,
	); err != nil {
		return err
	}
	if err := releaseArtifactBudgetReservation(
		ctx, q, row.attemptID.String(), "lease_expired", now,
	); err != nil {
		return err
	}
	if err := cancelPhaseWorkItems(
		ctx, q, row.attemptID.String(), "lease_expired", now,
	); err != nil {
		return err
	}
	if err := revokeWorkerGatewayAttempt(
		ctx, q, row.attemptID.String(), "lease_expired", now,
	); err != nil {
		return err
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
		return err
	}
	if err := r.insertJobEvent(ctx, q, row.jobID.String(), "job.lease_expired", map[string]any{
		"attempt_id": row.attemptID, "attempt_no": row.attemptNo, "next_state": state,
		"lease_kind": row.leaseKind, "reason": reason,
	}); err != nil {
		return err
	}
	result := leaseExpiryResult(state)
	if !validLeaseExpiryCounter(row.leaseKind, result) {
		return fmt.Errorf(
			"invalid lease expiry counter update kind=%q result=%q",
			row.leaseKind, result,
		)
	}
	tally[leaseExpiryKey{leaseKind: row.leaseKind, result: result}]++
	return nil
}

func (r *JobRepository) recoverExpiredTx(ctx context.Context, q Querier, limit int) (int, error) {
	expired, err := loadExpiredLeases(ctx, q, limit)
	if err != nil {
		return 0, err
	}
	tally := make(map[leaseExpiryKey]int64, len(expired))
	for _, row := range expired {
		if err := r.recoverExpiredLease(ctx, q, row, tally); err != nil {
			return 0, err
		}
	}
	if err := flushLeaseExpiries(ctx, q, tally); err != nil {
		return 0, err
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
		SET expires_at = LEAST(
		      u.metering_started_at
		        + make_interval(secs => u.max_runtime_seconds::double precision),
		      now() + $5::interval
		    ),
		    renewed_at = now()
		FROM build_attempts a, workers w, project_attempt_usage u
		WHERE l.attempt_id = a.id
		  AND l.worker_id = w.id
		  AND u.attempt_id = a.id
		  AND a.id = $1 AND a.job_id = $2
		  AND l.fence_token = $3 AND w.stable_name = $4
		  AND l.expires_at > clock_timestamp()
		  AND u.state = 'active'
		  AND u.metering_started_at
		      + make_interval(secs => u.max_runtime_seconds::double precision)
		      > clock_timestamp()
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
			JOIN project_attempt_usage u ON u.attempt_id = a.id
			WHERE a.id = $1 AND a.job_id = $2
			  AND l.fence_token = $3 AND w.stable_name = $4
			  AND l.expires_at > clock_timestamp()
			  AND u.state = 'active'
			  AND u.metering_started_at
			      + make_interval(secs => u.max_runtime_seconds::double precision)
			      > clock_timestamp()
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
		SELECT j.id::text, j.project_id::text,
		       COALESCE(j.requested_by_subject_id::text, ''),
		       j.request, j.status_snapshot,
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
		var id, projectID, requestedBy, attemptID, owner string
		var requestJSON, statusJSON []byte
		var fence int64
		if err := rows.Scan(
			&id, &projectID, &requestedBy, &requestJSON, &statusJSON,
			&attemptID, &fence, &owner,
		); err != nil {
			return nil, err
		}
		request, err := decodeStoredRequest(requestJSON)
		if err != nil {
			return nil, err
		}
		request.ProjectID, request.RequestedBy = projectID, requestedBy
		status, err := decodeStoredStatus(statusJSON, request)
		if err != nil {
			return nil, err
		}
		status.JobID = id
		status.ProjectID, status.RequestedBy = projectID, requestedBy
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
		now, err := readDatabaseClock(ctx, q)
		if err != nil {
			return err
		}
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
		if err := settleJobAttemptUsage(
			ctx, q, jobID, "job_canceled", "canceled", now,
		); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			DELETE FROM worker_leases WHERE attempt_id IN (
				SELECT id FROM build_attempts WHERE job_id = $1
			)
		`, jobID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			UPDATE project_resource_reservations
			SET state = 'released', released_at = $2, release_reason = 'job_canceled'
			WHERE job_id = $1 AND state = 'active'
		`, jobID, now); err != nil {
			return fmt.Errorf("release canceled job resources: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE phase_work_items
			SET state = 'canceled', lease_expires_at = NULL,
			    finished_at = $2, updated_at = $2, error = 'job_canceled'
			WHERE job_id = $1
			  AND state IN ('blocked', 'ready', 'claimed')
		`, jobID, now); err != nil {
			return fmt.Errorf("cancel job phase work: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE artifact_generation_reservations
			SET state = 'released', released_at = $2, updated_at = $2,
			    release_reason = 'job_canceled'
			WHERE attempt_id IN (
				SELECT attempt_id FROM project_artifact_budgets WHERE job_id = $1
			) AND state = 'active'
		`, jobID, now); err != nil {
			return fmt.Errorf("release canceled artifact generations: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE project_artifact_budgets
			SET state = 'released', active_bytes = 0, released_at = $2,
			    updated_at = $2, release_reason = 'job_canceled'
			WHERE job_id = $1 AND state = 'active'
		`, jobID, now); err != nil {
			return fmt.Errorf("release canceled artifact budgets: %w", err)
		}
		if err := revokeWorkerGatewayJob(
			ctx, q, jobID, "job_canceled", now,
		); err != nil {
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
		var projectID string
		var requestJSON, statusJSON []byte
		var attempts, maxAttempts int
		if err := q.QueryRow(ctx, `
			SELECT state, project_id::text, request, status_snapshot,
			       (SELECT count(*) FROM build_attempts WHERE job_id = build_jobs.id),
			       max_attempts
			FROM build_jobs
			WHERE id = $1 AND legacy_visible = true
			FOR UPDATE
		`, jobID).Scan(&state, &projectID, &requestJSON, &statusJSON, &attempts, &maxAttempts); err != nil {
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
		if err := lockAndCheckRetryAdmission(ctx, q, projectID, jobID, request); err != nil {
			return err
		}
		status, err := decodeStoredStatus(statusJSON, request)
		if err != nil {
			return err
		}
		now, err := readDatabaseClock(ctx, q)
		if err != nil {
			return err
		}
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

func lockAndCheckRetryAdmission(
	ctx context.Context,
	q Querier,
	projectID, jobID string,
	request *builder.BuildRequest,
) error {
	var suspended bool
	var abuseUntil *time.Time
	var now time.Time
	var maxQueued, maxVCPUs, maxMemoryMiB, maxDiskGiB int
	var maxBuildSeconds, maxCloudCost int64
	if err := q.QueryRow(ctx, `
		SELECT suspended, max_queued_jobs,
		       max_active_vcpus, max_active_memory_mib, max_active_disk_gib,
		       max_daily_build_seconds, max_daily_cloud_cost_microunits,
		       abuse_suspended_until, clock_timestamp()
		FROM project_policies
		WHERE project_id = $1
		FOR UPDATE
	`, projectID).Scan(
		&suspended, &maxQueued, &maxVCPUs, &maxMemoryMiB, &maxDiskGiB,
		&maxBuildSeconds, &maxCloudCost, &abuseUntil, &now,
	); err != nil {
		return fmt.Errorf("lock project retry policy: %w", err)
	}
	if suspended {
		return builder.NewAdmissionError(
			"project_suspended", 0, 0, 0,
			fmt.Errorf("project is suspended; new builds and retries are disabled"),
		)
	}
	if abuseUntil != nil && abuseUntil.After(now) {
		return builder.NewAdmissionError(
			"project_abuse_suspended", 0, 0, abuseUntil.Sub(now),
			fmt.Errorf("project is temporarily suspended by the failure-storm gate until %s",
				abuseUntil.UTC().Format(time.RFC3339)),
		)
	}
	if err := checkResourceRequestLimits(
		request, maxVCPUs, maxMemoryMiB, maxDiskGiB,
	); err != nil {
		return err
	}
	if err := checkRuntimeRequestLimits(
		request, maxBuildSeconds, maxCloudCost,
	); err != nil {
		return err
	}
	var queued int
	if err := q.QueryRow(ctx, `
		SELECT count(*)
		FROM build_jobs
		WHERE project_id = $1 AND id <> $2
		  AND legacy_visible = true AND state = 'queued'
	`, projectID, jobID).Scan(&queued); err != nil {
		return fmt.Errorf("read project retry usage: %w", err)
	}
	if queued >= maxQueued {
		return builder.NewAdmissionError(
			"queued_limit_reached", maxQueued, queued, 30*time.Second,
			fmt.Errorf("project queued-job limit reached (%d/%d)", queued, maxQueued),
		)
	}
	return nil
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
			  WHERE j.state = 'queued' AND NOT EXISTS (
			    SELECT 1 FROM workers capability_worker
			    WHERE capability_worker.desired_state = 'active'
			      AND capability_worker.last_seen_at >
			          clock_timestamp() - interval '45 seconds'
			      AND capability_worker.executor_protocol >=
			          j.minimum_executor_protocol
			      AND NOT EXISTS (
			        SELECT 1
			        FROM jsonb_array_elements_text(
			          j.required_capabilities
			        ) AS required(label)
			        WHERE NOT (
			          COALESCE(
			            capability_worker.capabilities -> 'labels',
			            '[]'::jsonb
			          ) ? required.label
			        )
			      )
			  )
			),
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
			(SELECT count(*) FROM workers w WHERE w.desired_state = 'active' AND w.last_seen_at > clock_timestamp() - interval '45 seconds' AND jsonb_array_length(COALESCE(w.capabilities -> 'labels', '[]'::jsonb)) > 0),
			(SELECT count(*) FROM workers w WHERE w.last_seen_at IS NULL OR w.last_seen_at <= clock_timestamp() - interval '45 seconds'),
			(SELECT count(*) FROM build_attempts a WHERE a.created_at > now() - interval '1 hour')
		FROM build_jobs j
		WHERE j.legacy_visible = true
	`).Scan(
		&status.QueuedTasks,
		&status.UnschedulableTasks,
		&status.RunningTasks,
		&status.OldestQueuedAt,
		&status.ActiveLeases,
		&status.ExpiredLeases,
		&status.OldestLeaseExpires,
		&status.RegisteredWorkers,
		&status.ActiveWorkers,
		&status.CapabilityWorkers,
		&status.StaleWorkers,
		&status.AttemptsLastHour,
	)
	if err != nil {
		return status, fmt.Errorf("read scheduler runtime status: %w", err)
	}
	status.LeaseExpiries, err = readLeaseExpiryStatus(ctx, r.db.Pool())
	if err != nil {
		return status, err
	}
	status.Fairness, err = readFairnessRuntimeStatus(ctx, r.db.Pool())
	if err != nil {
		return status, err
	}
	status.WorkerScoring, err = readWorkerScoringStatus(ctx, r.db.Pool())
	if err != nil {
		return status, err
	}
	status.TargetHistory, err = r.targetHistoryStatus(ctx)
	if err != nil {
		return status, err
	}
	status.Autoscaler, err = readAutoscaleRuntimeStatus(ctx, r.db.Pool())
	if err != nil {
		return status, err
	}
	return status, nil
}

func readLeaseExpiryStatus(
	ctx context.Context,
	q Querier,
) (builder.LeaseExpiryStatus, error) {
	var status builder.LeaseExpiryStatus
	err := q.QueryRow(ctx, `
		SELECT
		  COALESCE(sum(total) FILTER (
		    WHERE lease_kind = 'attempt' AND result = 'requeued'
		  ), 0),
		  COALESCE(sum(total) FILTER (
		    WHERE lease_kind = 'attempt' AND result = 'failed'
		  ), 0),
		  COALESCE(sum(total) FILTER (
		    WHERE lease_kind = 'attempt' AND result = 'canceled'
		  ), 0),
		  COALESCE(sum(total) FILTER (
		    WHERE lease_kind = 'admission' AND result = 'requeued'
		  ), 0),
		  COALESCE(sum(total) FILTER (
		    WHERE lease_kind = 'admission' AND result = 'failed'
		  ), 0),
		  COALESCE(sum(total) FILTER (
		    WHERE lease_kind = 'admission' AND result = 'canceled'
		  ), 0),
		  COALESCE(sum(total) FILTER (
		    WHERE lease_kind = 'phase' AND result = 'reclaimed'
		  ), 0)
		FROM scheduler_lease_expiry_counters
	`).Scan(
		&status.AttemptRequeued,
		&status.AttemptFailed,
		&status.AttemptCanceled,
		&status.AdmissionRequeued,
		&status.AdmissionFailed,
		&status.AdmissionCanceled,
		&status.PhaseReclaimed,
	)
	if err != nil {
		return status, fmt.Errorf("read lease expiry counters: %w", err)
	}
	return status, nil
}

func readWorkerScoringStatus(
	ctx context.Context,
	q Querier,
) (builder.WorkerScoringStatus, error) {
	var status builder.WorkerScoringStatus
	if err := q.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE candidate_count > 1)
		FROM scheduler_worker_decisions
		WHERE selected_at > clock_timestamp() - interval '1 hour'
	`).Scan(
		&status.DecisionsLastHour,
		&status.MultiCandidateLastHour,
	); err != nil {
		return status, fmt.Errorf("read worker-scoring counters: %w", err)
	}
	rows, err := q.Query(ctx, `
		SELECT d.work_kind, d.phase, w.stable_name,
		       d.candidate_count, d.pressure_score, d.recent_failures,
		       d.reason, d.selected_at
		FROM scheduler_worker_decisions d
		JOIN workers w ON w.id = d.worker_id
		ORDER BY d.selected_at DESC
		LIMIT 10
	`)
	if err != nil {
		return status, fmt.Errorf("read worker-scoring decisions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var decision builder.WorkerDecisionStatus
		if err := rows.Scan(
			&decision.WorkKind, &decision.Phase, &decision.Worker,
			&decision.CandidateCount, &decision.PressureScore,
			&decision.RecentFailures, &decision.Reason, &decision.SelectedAt,
		); err != nil {
			return status, fmt.Errorf("scan worker-scoring decision: %w", err)
		}
		status.Recent = append(status.Recent, decision)
	}
	if err := rows.Err(); err != nil {
		return status, fmt.Errorf("iterate worker-scoring decisions: %w", err)
	}
	return status, nil
}
