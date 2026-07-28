package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
)

const autoscaleScope = "phase-executor:global"

type autoscaleObservation struct {
	activeSlots, busySlots, backlog, unschedulable int
}

type autoscaleDecisionState struct {
	desired     int
	underTarget *time.Time
	lastChanged *time.Time
}

type autoscaleDecision struct {
	autoscaleDecisionState
	recommendation string
	reason         string
}

func clampSlots(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func readAutoscaleObservation(
	ctx context.Context,
	q Querier,
) (autoscaleObservation, error) {
	var observation autoscaleObservation
	err := q.QueryRow(ctx, `
		SELECT
		  (
		    SELECT count(*) FROM workers worker
		    WHERE worker.desired_state = 'active'
		      AND worker.last_seen_at >
		          clock_timestamp() - interval '45 seconds'
		      AND worker.capabilities ->> 'role' = 'phase-executor'
		  ),
		  (
		    SELECT count(*) FROM phase_work_items work
		    WHERE work.execution_mode = 'active'
		      AND work.state = 'claimed'
		  ),
		  (
		    SELECT count(*) FROM build_jobs job
		    WHERE job.legacy_visible = true AND job.state = 'queued'
		  ) + (
		    SELECT count(*) FROM phase_work_items work
		    WHERE work.execution_mode = 'active' AND work.state = 'ready'
		  ),
		  (
		    SELECT count(*) FROM build_jobs job
		    WHERE job.legacy_visible = true AND job.state = 'queued'
		      AND NOT EXISTS (
		        SELECT 1 FROM workers worker
		        WHERE worker.desired_state = 'active'
		          AND worker.last_seen_at >
		              clock_timestamp() - interval '45 seconds'
		          AND worker.executor_protocol >=
		              job.minimum_executor_protocol
		          AND NOT EXISTS (
		            SELECT 1
		            FROM jsonb_array_elements_text(
		              job.required_capabilities
		            ) AS required(label)
		            WHERE NOT (
		              COALESCE(
		                worker.capabilities -> 'labels', '[]'::jsonb
		              ) ? required.label
		            )
		          )
		      )
		  ) + (
		    SELECT count(*) FROM phase_work_items work
		    JOIN build_jobs job ON job.id = work.job_id
		    WHERE work.execution_mode = 'active' AND work.state = 'ready'
		      AND NOT EXISTS (
		        SELECT 1 FROM workers worker
		        WHERE worker.desired_state = 'active'
		          AND worker.last_seen_at >
		              clock_timestamp() - interval '45 seconds'
		          AND worker.executor_protocol >=
		              job.minimum_executor_protocol
		          AND NOT EXISTS (
		            SELECT 1
		            FROM jsonb_array_elements_text(
		              work.required_capabilities
		            ) AS required(label)
		            WHERE NOT (
		              COALESCE(
		                worker.capabilities -> 'labels', '[]'::jsonb
		              ) ? required.label
		            )
		          )
		      )
		  )
	`).Scan(
		&observation.activeSlots,
		&observation.busySlots,
		&observation.backlog,
		&observation.unschedulable,
	)
	if err != nil {
		return observation, fmt.Errorf("read autoscale demand: %w", err)
	}
	return observation, nil
}

func validateAutoscalePolicy(policy builder.SchedulerAutoscalePolicy) error {
	if policy.Mode != "off" && policy.Mode != "observe" &&
		policy.Mode != "actuate" {
		return fmt.Errorf("autoscale mode must be off, observe, or actuate")
	}
	if policy.MinSlots < 0 || policy.MaxSlots < policy.MinSlots ||
		policy.TargetReady <= 0 || policy.Cooldown < 0 ||
		policy.ScaleDownDelay < 0 {
		return fmt.Errorf("autoscale policy is invalid")
	}
	if len(policy.Pools) > 1024 {
		return fmt.Errorf("autoscale policy contains too many capacity pools")
	}
	seen := make(map[string]struct{}, len(policy.Pools))
	for _, pool := range policy.Pools {
		if strings.TrimSpace(pool.ID) == "" ||
			strings.TrimSpace(pool.Provider) == "" ||
			strings.TrimSpace(pool.ExecutionZone) == "" ||
			strings.TrimSpace(pool.Arch) == "" ||
			strings.TrimSpace(pool.ProfileID) == "" ||
			strings.TrimSpace(pool.ImageID) == "" ||
			strings.TrimSpace(pool.ImageGeneration) == "" {
			return fmt.Errorf("autoscale capacity pool dimensions are incomplete")
		}
		if _, exists := seen[pool.ID]; exists {
			return fmt.Errorf("duplicate autoscale capacity pool %q", pool.ID)
		}
		seen[pool.ID] = struct{}{}
		selector, err := builder.NormalizeExecutorCapabilities(pool.Selector)
		if err != nil {
			return fmt.Errorf("capacity pool %s selector: %w", pool.ID, err)
		}
		foundPoolLabel := false
		for _, label := range selector {
			if label == "capacity-pool:"+pool.ID {
				foundPoolLabel = true
				break
			}
		}
		if !foundPoolLabel {
			return fmt.Errorf(
				"capacity pool %s selector omits its pool label", pool.ID,
			)
		}
		if policy.Mode == "actuate" {
			limit := policy.ProviderMaxSlots[pool.Provider]
			if limit <= 0 || limit > 10000 {
				return fmt.Errorf(
					"actuate mode requires a provider slot limit for %s",
					pool.Provider,
				)
			}
		}
	}
	return nil
}

func decideAutoscale(
	observation autoscaleObservation,
	policy builder.SchedulerAutoscalePolicy,
	now time.Time,
	state autoscaleDecisionState,
) autoscaleDecision {
	schedulableBacklog := observation.backlog - observation.unschedulable
	if schedulableBacklog < 0 {
		schedulableBacklog = 0
	}
	rawDesired := observation.busySlots
	rawDesired += (schedulableBacklog + policy.TargetReady - 1) /
		policy.TargetReady
	rawDesired = clampSlots(rawDesired, policy.MinSlots, policy.MaxSlots)

	decision := autoscaleDecision{
		autoscaleDecisionState: state,
		recommendation:         "hold",
		reason: fmt.Sprintf(
			"busy=%d schedulable_backlog=%d target_ready_per_slot=%d",
			observation.busySlots, schedulableBacklog, policy.TargetReady,
		),
	}
	changed := false
	cooldownReady := state.lastChanged == nil ||
		now.Sub(*state.lastChanged) >= policy.Cooldown
	switch {
	case policy.Mode == "off":
		decision.desired = observation.activeSlots
		decision.recommendation = "off"
		decision.underTarget = nil
	case rawDesired > state.desired:
		decision.underTarget = nil
		if cooldownReady {
			decision.desired, changed = rawDesired, true
		} else {
			decision.reason += "; scale-up cooldown active"
		}
	case rawDesired < state.desired:
		if decision.underTarget == nil {
			started := now
			decision.underTarget = &started
		}
		if cooldownReady &&
			now.Sub(*decision.underTarget) >= policy.ScaleDownDelay {
			decision.desired, changed = rawDesired, true
			decision.underTarget = nil
		} else {
			decision.reason += "; scale-down dwell active"
		}
	default:
		decision.underTarget = nil
	}
	if policy.Mode != "off" {
		switch {
		case decision.desired > observation.activeSlots:
			decision.recommendation = "scale-up"
		case decision.desired < observation.activeSlots:
			decision.recommendation = "scale-down"
		default:
			decision.recommendation = "hold"
		}
	}
	if changed {
		changedAt := now
		decision.lastChanged = &changedAt
	}
	return decision
}

// ReconcileAutoscaling persists a shared recommendation. It deliberately has
// no provider client and therefore cannot create or delete infrastructure.
func (r *JobRepository) ReconcileAutoscaling(
	ctx context.Context,
	policy builder.SchedulerAutoscalePolicy,
) (builder.SchedulerAutoscaleStatus, error) {
	if err := validateAutoscalePolicy(policy); err != nil {
		return builder.SchedulerAutoscaleStatus{}, err
	}
	var result builder.SchedulerAutoscaleStatus
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		observation, err := readAutoscaleObservation(ctx, q)
		if err != nil {
			return err
		}
		now, err := readDatabaseClock(ctx, q)
		if err != nil {
			return err
		}
		initialDesired := clampSlots(
			observation.activeSlots, policy.MinSlots, policy.MaxSlots,
		)
		if _, err := q.Exec(ctx, `
			INSERT INTO scheduler_autoscale_state (
			  scope, mode, active_slots, busy_slots, backlog,
			  unschedulable_backlog, desired_slots, recommendation,
			  reason, last_evaluated_at, updated_at
			) VALUES (
			  $1, $2, $3, $4, $5, $6, $7, 'hold',
			  'initial observation', $8, $8
			)
			ON CONFLICT (scope) DO NOTHING
		`, autoscaleScope, policy.Mode, observation.activeSlots,
			observation.busySlots, observation.backlog,
			observation.unschedulable, initialDesired, now); err != nil {
			return fmt.Errorf("initialize autoscale state: %w", err)
		}

		var currentDesired int
		var underTarget, lastChanged *time.Time
		if err := q.QueryRow(ctx, `
			SELECT desired_slots, under_target_since, last_changed_at
			FROM scheduler_autoscale_state
			WHERE scope = $1
			FOR UPDATE
		`, autoscaleScope).Scan(
			&currentDesired, &underTarget, &lastChanged,
		); err != nil {
			return fmt.Errorf("lock autoscale state: %w", err)
		}

		decision := decideAutoscale(
			observation, policy, now, autoscaleDecisionState{
				desired: currentDesired, underTarget: underTarget,
				lastChanged: lastChanged,
			},
		)

		if _, err := q.Exec(ctx, `
			UPDATE scheduler_autoscale_state
			SET mode = $2, active_slots = $3, busy_slots = $4,
			    backlog = $5, unschedulable_backlog = $6,
			    desired_slots = $7, recommendation = $8, reason = $9,
			    under_target_since = $10, last_changed_at = $11,
			    last_evaluated_at = $12, updated_at = $12
			WHERE scope = $1
		`, autoscaleScope, policy.Mode, observation.activeSlots,
			observation.busySlots, observation.backlog,
			observation.unschedulable, decision.desired,
			decision.recommendation, decision.reason,
			decision.underTarget, decision.lastChanged, now); err != nil {
			return fmt.Errorf("persist autoscale recommendation: %w", err)
		}
		result = builder.SchedulerAutoscaleStatus{
			Scope: autoscaleScope, Mode: policy.Mode,
			ActiveSlots: observation.activeSlots,
			BusySlots:   observation.busySlots, Backlog: observation.backlog,
			UnschedulableBacklog: observation.unschedulable,
			DesiredSlots:         decision.desired,
			Recommendation:       decision.recommendation,
			Reason:               decision.reason, UnderTargetSince: decision.underTarget,
			LastChangedAt: decision.lastChanged, LastEvaluatedAt: &now,
		}
		for _, pool := range policy.Pools {
			poolStatus, err := reconcileCapacityPool(
				ctx, q, pool, policy, now,
			)
			if err != nil {
				return err
			}
			result.Pools = append(result.Pools, poolStatus)
		}
		if err := reconcileCapacityActionsTx(
			ctx, q, policy, result.Pools, observation.activeSlots,
		); err != nil {
			return err
		}
		return nil
	})
	r.recordWrite(err)
	return result, err
}

func readCapacityPoolObservation(
	ctx context.Context,
	q Querier,
	poolLabel string,
) (autoscaleObservation, error) {
	var observation autoscaleObservation
	err := q.QueryRow(ctx, `
		SELECT
		  (
		    SELECT count(*) FROM workers worker
		    WHERE worker.desired_state = 'active'
		      AND worker.last_seen_at >
		          clock_timestamp() - interval '45 seconds'
		      AND worker.capabilities ->> 'role' = 'phase-executor'
		      AND COALESCE(
		            worker.capabilities -> 'labels', '[]'::jsonb
		          ) ? $1
		  ),
		  (
		    SELECT count(*) FROM phase_work_items work
		    WHERE work.execution_mode = 'active'
		      AND work.state = 'claimed'
		      AND work.required_capabilities ? $1
		  ),
		  (
		    SELECT count(*) FROM build_jobs job
		    WHERE job.legacy_visible = true AND job.state = 'queued'
		      AND job.required_capabilities ? $1
		  ) + (
		    SELECT count(*) FROM phase_work_items work
		    WHERE work.execution_mode = 'active' AND work.state = 'ready'
		      AND work.required_capabilities ? $1
		  ),
		  (
		    SELECT count(*) FROM build_jobs job
		    WHERE job.legacy_visible = true AND job.state = 'queued'
		      AND job.required_capabilities ? $1
		      AND NOT EXISTS (
		        SELECT 1 FROM workers worker
		        WHERE worker.desired_state = 'active'
		          AND worker.last_seen_at >
		              clock_timestamp() - interval '45 seconds'
		          AND worker.executor_protocol >=
		              job.minimum_executor_protocol
		          AND NOT EXISTS (
		            SELECT 1
		            FROM jsonb_array_elements_text(
		              job.required_capabilities
		            ) AS required(label)
		            WHERE NOT (
		              COALESCE(
		                worker.capabilities -> 'labels', '[]'::jsonb
		              ) ? required.label
		            )
		          )
		      )
		  ) + (
		    SELECT count(*) FROM phase_work_items work
		    JOIN build_jobs job ON job.id = work.job_id
		    WHERE work.execution_mode = 'active' AND work.state = 'ready'
		      AND work.required_capabilities ? $1
		      AND NOT EXISTS (
		        SELECT 1 FROM workers worker
		        WHERE worker.desired_state = 'active'
		          AND worker.last_seen_at >
		              clock_timestamp() - interval '45 seconds'
		          AND worker.executor_protocol >=
		              job.minimum_executor_protocol
		          AND NOT EXISTS (
		            SELECT 1
		            FROM jsonb_array_elements_text(
		              work.required_capabilities
		            ) AS required(label)
		            WHERE NOT (
		              COALESCE(
		                worker.capabilities -> 'labels', '[]'::jsonb
		              ) ? required.label
		            )
		          )
		      )
		  )
	`, poolLabel).Scan(
		&observation.activeSlots,
		&observation.busySlots,
		&observation.backlog,
		&observation.unschedulable,
	)
	if err != nil {
		return observation, fmt.Errorf(
			"read capacity pool %s demand: %w", poolLabel, err,
		)
	}
	return observation, nil
}

func reconcileCapacityPool(
	ctx context.Context,
	q Querier,
	pool builder.SchedulerCapacityPoolDefinition,
	policy builder.SchedulerAutoscalePolicy,
	now time.Time,
) (builder.SchedulerCapacityPoolStatus, error) {
	status := builder.SchedulerCapacityPoolStatus{
		SchedulerCapacityPoolDefinition: pool,
	}
	observation, err := readCapacityPoolObservation(
		ctx, q, "capacity-pool:"+pool.ID,
	)
	if err != nil {
		return status, err
	}
	selector, err := json.Marshal(pool.Selector)
	if err != nil {
		return status, fmt.Errorf("marshal capacity pool selector: %w", err)
	}
	// The global minimum is a deployment safety floor, not a per-profile warm
	// pool request. Applying it to every catalog entry would multiply idle
	// capacity by the number of profiles. Per-pool warm minima require an
	// explicit future pool policy; the safe default here is zero.
	poolPolicy := policy
	poolPolicy.MinSlots = 0
	providerMaxSlots := policy.ProviderMaxSlots[pool.Provider]
	initialDesired := clampSlots(observation.activeSlots, 0, policy.MaxSlots)
	if _, err := q.Exec(ctx, `
		INSERT INTO scheduler_capacity_pool_state (
		  pool_id, provider, execution_zone, architecture, build_mode,
		  profile_id, image_id, image_generation, selector, mode,
		  provider_max_slots,
		  active_slots, busy_slots, backlog, unschedulable_backlog,
		  desired_slots, recommendation, reason,
		  last_evaluated_at, updated_at
		) VALUES (
		  $1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10,
		  $11, $12, $13, $14, $15, $16, 'hold', 'initial observation',
		  $17, $17
		)
		ON CONFLICT (pool_id) DO NOTHING
	`, pool.ID, pool.Provider, pool.ExecutionZone, pool.Arch, pool.BuildMode,
		pool.ProfileID, pool.ImageID, pool.ImageGeneration, string(selector),
		policy.Mode, providerMaxSlots, observation.activeSlots,
		observation.busySlots, observation.backlog,
		observation.unschedulable, initialDesired, now,
	); err != nil {
		return status, fmt.Errorf(
			"initialize capacity pool %s: %w", pool.ID, err,
		)
	}

	var state autoscaleDecisionState
	if err := q.QueryRow(ctx, `
		SELECT desired_slots, under_target_since, last_changed_at
		FROM scheduler_capacity_pool_state
		WHERE pool_id = $1
		FOR UPDATE
	`, pool.ID).Scan(
		&state.desired, &state.underTarget, &state.lastChanged,
	); err != nil {
		return status, fmt.Errorf("lock capacity pool %s: %w", pool.ID, err)
	}
	decision := decideAutoscale(observation, poolPolicy, now, state)
	if _, err := q.Exec(ctx, `
		UPDATE scheduler_capacity_pool_state
		SET provider = $2, execution_zone = $3, architecture = $4,
		    build_mode = $5, profile_id = $6, image_id = $7,
		    image_generation = $8, selector = $9::jsonb, mode = $10,
		    provider_max_slots = $11,
		    active_slots = $12, busy_slots = $13, backlog = $14,
		    unschedulable_backlog = $15, desired_slots = $16,
		    recommendation = $17, reason = $18,
		    under_target_since = $19, last_changed_at = $20,
		    last_evaluated_at = $21, updated_at = $21
		WHERE pool_id = $1
	`, pool.ID, pool.Provider, pool.ExecutionZone, pool.Arch, pool.BuildMode,
		pool.ProfileID, pool.ImageID, pool.ImageGeneration, string(selector),
		policy.Mode, providerMaxSlots, observation.activeSlots,
		observation.busySlots, observation.backlog,
		observation.unschedulable, decision.desired,
		decision.recommendation, decision.reason, decision.underTarget,
		decision.lastChanged, now,
	); err != nil {
		return status, fmt.Errorf(
			"persist capacity pool %s recommendation: %w", pool.ID, err,
		)
	}
	status.Mode = policy.Mode
	status.ProviderMaxSlots = providerMaxSlots
	status.ActiveSlots = observation.activeSlots
	status.BusySlots = observation.busySlots
	status.Backlog = observation.backlog
	status.UnschedulableBacklog = observation.unschedulable
	status.DesiredSlots = decision.desired
	status.Recommendation = decision.recommendation
	status.Reason = decision.reason
	status.UnderTargetSince = decision.underTarget
	status.LastChangedAt = decision.lastChanged
	status.LastEvaluatedAt = &now
	return status, nil
}

func requestCapacityActionTx(
	ctx context.Context,
	q Querier,
	poolID, kind string,
	requestedSlots, observedSlots int,
	reason string,
) error {
	if len(reason) > capacityActionMaxDetail {
		reason = reason[:capacityActionMaxDetail]
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO scheduler_capacity_actions (
		  id, pool_id, action_kind, state, requested_slots,
		  observed_slots, delta_slots, reason
		) VALUES ($1, $2, $3, 'requested', $4, $5, 1, $6)
		ON CONFLICT (pool_id) WHERE state IN (
		  'requested', 'claimed', 'waiting-for-heartbeat',
		  'waiting-for-drain'
		) DO UPDATE SET
		  action_kind = EXCLUDED.action_kind,
		  requested_slots = EXCLUDED.requested_slots,
		  observed_slots = EXCLUDED.observed_slots,
		  reason = EXCLUDED.reason,
		  next_attempt_at = clock_timestamp(),
		  failure_detail = '',
		  updated_at = clock_timestamp()
		WHERE scheduler_capacity_actions.state = 'requested'
	`, uuid.New(), poolID, kind, requestedSlots, observedSlots, reason); err != nil {
		return fmt.Errorf("enqueue capacity pool %s action: %w", poolID, err)
	}
	return nil
}

func cancelRequestedCapacityActionTx(
	ctx context.Context,
	q Querier,
	poolID, reason string,
) error {
	if len(reason) > capacityActionMaxDetail {
		reason = reason[:capacityActionMaxDetail]
	}
	if _, err := q.Exec(ctx, `
		UPDATE scheduler_capacity_actions
		SET state = 'canceled', failure_detail = $2,
		    completed_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE pool_id = $1 AND state = 'requested'
	`, poolID, reason); err != nil {
		return fmt.Errorf("cancel stale capacity pool %s action: %w", poolID, err)
	}
	return nil
}

// reconcileCapacityActionsTx turns pool demand into at most one fenced slot
// action per pool while enforcing the deployment-wide active+pending ceiling.
// Claimed actions are never rewritten or canceled underneath an actuator.
func reconcileCapacityActionsTx(
	ctx context.Context,
	q Querier,
	policy builder.SchedulerAutoscalePolicy,
	pools []builder.SchedulerCapacityPoolStatus,
	activeSlots int,
) error {
	if policy.Mode != "actuate" {
		for _, pool := range pools {
			if err := cancelRequestedCapacityActionTx(
				ctx, q, pool.ID, "autoscale actuation is disabled",
			); err != nil {
				return err
			}
		}
		return nil
	}
	var pendingScaleUp int
	if err := q.QueryRow(ctx, `
		SELECT count(*)
		FROM scheduler_capacity_actions
		WHERE action_kind = 'scale-up'
		  AND state IN ('claimed', 'waiting-for-heartbeat')
	`).Scan(&pendingScaleUp); err != nil {
		return fmt.Errorf("read pending scale-up capacity: %w", err)
	}
	scaleUpBudget := policy.MaxSlots - activeSlots - pendingScaleUp
	if scaleUpBudget < 0 {
		scaleUpBudget = 0
	}
	scaleDownBudget := activeSlots - policy.MinSlots
	if scaleDownBudget < 0 {
		scaleDownBudget = 0
	}
	providerBudgets := make(map[string]int, len(policy.ProviderMaxSlots))
	for provider, limit := range policy.ProviderMaxSlots {
		var active, pending int
		if err := q.QueryRow(ctx, `
			SELECT
			  (
			    SELECT count(*)
			    FROM workers
			    WHERE desired_state = 'active'
			      AND last_seen_at >
			          clock_timestamp() - interval '45 seconds'
			      AND COALESCE(
			            capabilities -> 'labels', '[]'::jsonb
			          ) ? $1
			  ),
			  (
			    SELECT count(*)
			    FROM scheduler_capacity_actions action
			    JOIN scheduler_capacity_pool_state pool
			      ON pool.pool_id = action.pool_id
			    WHERE pool.provider = $2
			      AND action.action_kind = 'scale-up'
			      AND action.state IN (
			        'claimed', 'waiting-for-heartbeat'
			      )
			  )
		`, "provider:"+provider, provider).Scan(&active, &pending); err != nil {
			return fmt.Errorf(
				"read %s provider capacity allocation: %w", provider, err,
			)
		}
		budget := limit - active - pending
		if budget < 0 {
			budget = 0
		}
		providerBudgets[provider] = budget
	}
	ordered := append([]builder.SchedulerCapacityPoolStatus(nil), pools...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].UnschedulableBacklog != ordered[j].UnschedulableBacklog {
			return ordered[i].UnschedulableBacklog >
				ordered[j].UnschedulableBacklog
		}
		if ordered[i].Backlog != ordered[j].Backlog {
			return ordered[i].Backlog > ordered[j].Backlog
		}
		return ordered[i].ID < ordered[j].ID
	})
	for _, pool := range ordered {
		switch {
		case pool.Recommendation == "scale-up" &&
			scaleUpBudget > 0 && providerBudgets[pool.Provider] > 0:
			if err := requestCapacityActionTx(
				ctx, q, pool.ID, "scale-up", pool.DesiredSlots,
				pool.ActiveSlots, pool.Reason,
			); err != nil {
				return err
			}
			scaleUpBudget--
			providerBudgets[pool.Provider]--
		case pool.Recommendation == "scale-down" && scaleDownBudget > 0:
			if err := requestCapacityActionTx(
				ctx, q, pool.ID, "scale-down", pool.DesiredSlots,
				pool.ActiveSlots, pool.Reason,
			); err != nil {
				return err
			}
			scaleDownBudget--
		default:
			if err := cancelRequestedCapacityActionTx(
				ctx, q, pool.ID,
				"pool recommendation no longer has global capacity budget",
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func readAutoscaleRuntimeStatus(
	ctx context.Context,
	q Querier,
) (builder.SchedulerAutoscaleStatus, error) {
	var status builder.SchedulerAutoscaleStatus
	err := q.QueryRow(ctx, `
		SELECT scope, mode, active_slots, busy_slots, backlog,
		       unschedulable_backlog, desired_slots, recommendation, reason,
		       under_target_since, last_changed_at, last_evaluated_at
		FROM scheduler_autoscale_state
		WHERE scope = $1
	`, autoscaleScope).Scan(
		&status.Scope, &status.Mode, &status.ActiveSlots, &status.BusySlots,
		&status.Backlog, &status.UnschedulableBacklog, &status.DesiredSlots,
		&status.Recommendation, &status.Reason, &status.UnderTargetSince,
		&status.LastChangedAt, &status.LastEvaluatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return builder.SchedulerAutoscaleStatus{
			Scope: autoscaleScope, Mode: "off", Recommendation: "off",
			Reason: "autoscale observer has not evaluated this deployment",
		}, nil
	}
	if err != nil {
		return status, fmt.Errorf("read autoscale runtime status: %w", err)
	}
	status.Pools, err = readCapacityPoolRuntimeStatus(ctx, q)
	if err != nil {
		return status, err
	}
	status.Actuator, err = readCapacityActuatorRuntimeStatus(ctx, q)
	if err != nil {
		return status, err
	}
	return status, nil
}

func readCapacityActuatorRuntimeStatus(
	ctx context.Context,
	q Querier,
) (builder.CapacityActuatorStatus, error) {
	var status builder.CapacityActuatorStatus
	if err := q.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE state IN (
		    'requested', 'claimed', 'waiting-for-heartbeat',
		    'waiting-for-drain'
		  )),
		  count(*) FILTER (WHERE state = 'failed')
		FROM scheduler_capacity_actions
	`).Scan(&status.OpenActions, &status.FailedActions); err != nil {
		return status, fmt.Errorf("read capacity actuator action counts: %w", err)
	}
	if err := q.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE state = 'provisioning'),
		  count(*) FILTER (WHERE state = 'active'),
		  count(*) FILTER (WHERE state = 'draining'),
		  count(*) FILTER (WHERE state = 'deleting')
		FROM scheduler_capacity_instances
	`).Scan(
		&status.ProvisioningInstances, &status.ActiveInstances,
		&status.DrainingInstances, &status.DeletingInstances,
	); err != nil {
		return status, fmt.Errorf("read capacity actuator instance counts: %w", err)
	}
	actions, err := readCapacityActionRuntimeStatus(ctx, q)
	if err != nil {
		return status, err
	}
	instances, err := readCapacityInstanceRuntimeStatus(ctx, q)
	if err != nil {
		return status, err
	}
	status.Actions, status.Instances = actions, instances
	return status, nil
}

func readCapacityActionRuntimeStatus(
	ctx context.Context,
	q Querier,
) ([]builder.CapacityActionStatus, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, pool_id, action_kind, state, requested_slots,
		       observed_slots, attempts, failure_detail, requested_at,
		       updated_at, completed_at
		FROM scheduler_capacity_actions
		ORDER BY requested_at DESC, id DESC
		LIMIT 50
	`)
	if err != nil {
		return nil, fmt.Errorf("read capacity action status: %w", err)
	}
	defer rows.Close()
	var actions []builder.CapacityActionStatus
	for rows.Next() {
		var action builder.CapacityActionStatus
		if err := rows.Scan(
			&action.ID, &action.PoolID, &action.Kind, &action.State,
			&action.RequestedSlots, &action.ObservedSlots, &action.Attempts,
			&action.FailureDetail, &action.RequestedAt, &action.UpdatedAt,
			&action.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan capacity action status: %w", err)
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate capacity action status: %w", err)
	}
	return actions, nil
}

func readCapacityInstanceRuntimeStatus(
	ctx context.Context,
	q Querier,
) ([]builder.CapacityInstanceStatus, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, pool_id, provider, provider_instance_id, generation,
		       state, attributes, heartbeat_observed_at, drain_requested_at,
		       created_at, updated_at
		FROM scheduler_capacity_instances
		WHERE state <> 'deleted'
		   OR deleted_at > clock_timestamp() - interval '24 hours'
		ORDER BY created_at DESC, id DESC
		LIMIT 100
	`)
	if err != nil {
		return nil, fmt.Errorf("read capacity instance status: %w", err)
	}
	defer rows.Close()
	var instances []builder.CapacityInstanceStatus
	for rows.Next() {
		var instance builder.CapacityInstanceStatus
		var attributes []byte
		if err := rows.Scan(
			&instance.ID, &instance.PoolID, &instance.Provider,
			&instance.ProviderInstanceID, &instance.Generation,
			&instance.State, &attributes, &instance.HeartbeatObservedAt,
			&instance.DrainRequestedAt, &instance.CreatedAt,
			&instance.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan capacity instance status: %w", err)
		}
		if err := json.Unmarshal(attributes, &instance.Attributes); err != nil {
			return nil, fmt.Errorf(
				"decode capacity instance %s attributes: %w",
				instance.ID, err,
			)
		}
		instances = append(instances, instance)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate capacity instance status: %w", err)
	}
	return instances, nil
}

func readCapacityPoolRuntimeStatus(
	ctx context.Context,
	q Querier,
) ([]builder.SchedulerCapacityPoolStatus, error) {
	rows, err := q.Query(ctx, `
		SELECT pool_id, provider, execution_zone, architecture, build_mode,
		       profile_id, image_id, image_generation, selector,
		       mode, provider_max_slots, active_slots, busy_slots, backlog,
		       unschedulable_backlog, desired_slots, recommendation, reason,
		       under_target_since, last_changed_at, last_evaluated_at
		FROM scheduler_capacity_pool_state
		ORDER BY provider, execution_zone, architecture, profile_id, pool_id
	`)
	if err != nil {
		return nil, fmt.Errorf("read capacity pool runtime status: %w", err)
	}
	defer rows.Close()
	var pools []builder.SchedulerCapacityPoolStatus
	for rows.Next() {
		var pool builder.SchedulerCapacityPoolStatus
		var selector []byte
		if err := rows.Scan(
			&pool.ID, &pool.Provider, &pool.ExecutionZone, &pool.Arch,
			&pool.BuildMode, &pool.ProfileID, &pool.ImageID,
			&pool.ImageGeneration, &selector, &pool.Mode,
			&pool.ProviderMaxSlots, &pool.ActiveSlots,
			&pool.BusySlots, &pool.Backlog, &pool.UnschedulableBacklog,
			&pool.DesiredSlots, &pool.Recommendation, &pool.Reason,
			&pool.UnderTargetSince, &pool.LastChangedAt,
			&pool.LastEvaluatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan capacity pool runtime status: %w", err)
		}
		if err := json.Unmarshal(selector, &pool.Selector); err != nil {
			return nil, fmt.Errorf(
				"decode capacity pool %s selector: %w", pool.ID, err,
			)
		}
		pools = append(pools, pool)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate capacity pool runtime status: %w", err)
	}
	return pools, nil
}
