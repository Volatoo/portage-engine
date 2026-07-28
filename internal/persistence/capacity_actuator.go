package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
)

const capacityActionMaxDetail = 4096

func validateCapacityActionKind(kind string) error {
	if kind != "scale-up" && kind != "scale-down" {
		return fmt.Errorf("capacity action kind must be scale-up or scale-down")
	}
	return nil
}

func scanCapacityAction(row pgx.Row) (*builder.CapacityActionClaim, error) {
	var claim builder.CapacityActionClaim
	var selector []byte
	var leaseExpiresAt *time.Time
	err := row.Scan(
		&claim.ID, &claim.Pool.ID, &claim.Pool.Provider,
		&claim.Pool.ExecutionZone, &claim.Pool.Arch, &claim.Pool.BuildMode,
		&claim.Pool.ProfileID, &claim.Pool.ImageID,
		&claim.Pool.ImageGeneration, &selector, &claim.Kind,
		&claim.RequestedSlots, &claim.ObservedSlots, &claim.DeltaSlots,
		&claim.Reason, &claim.Owner, &claim.Fence, &leaseExpiresAt,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(selector, &claim.Pool.Selector); err != nil {
		return nil, fmt.Errorf("decode capacity action selector: %w", err)
	}
	if leaseExpiresAt != nil {
		claim.LeaseExpiresAt = leaseExpiresAt.UTC()
	}
	return &claim, nil
}

// RequestCapacityAction creates at most one open action per pool. The first
// actuator release intentionally changes one slot at a time so each provider
// instance receives an independent ownership record and heartbeat proof.
func (r *JobRepository) RequestCapacityAction(
	ctx context.Context,
	poolID, kind string,
	requestedSlots, observedSlots int,
	reason string,
) (*builder.CapacityActionClaim, error) {
	if strings.TrimSpace(poolID) == "" {
		return nil, fmt.Errorf("capacity pool id is required")
	}
	if err := validateCapacityActionKind(kind); err != nil {
		return nil, err
	}
	if requestedSlots < 0 || observedSlots < 0 ||
		(kind == "scale-up" && requestedSlots <= observedSlots) ||
		(kind == "scale-down" && requestedSlots >= observedSlots) {
		return nil, fmt.Errorf("capacity action slot transition is invalid")
	}
	if len(reason) > capacityActionMaxDetail {
		reason = reason[:capacityActionMaxDetail]
	}
	var result *builder.CapacityActionClaim
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		actionID := uuid.New()
		row := q.QueryRow(ctx, `
			WITH inserted AS (
			  INSERT INTO scheduler_capacity_actions (
			    id, pool_id, action_kind, state, requested_slots,
			    observed_slots, delta_slots, reason
			  ) VALUES ($1, $2, $3, 'requested', $4, $5, 1, $6)
			  ON CONFLICT (pool_id) WHERE state IN (
			    'requested', 'claimed', 'waiting-for-heartbeat',
			    'waiting-for-drain'
			  ) DO NOTHING
			  RETURNING *
			), action AS (
			  SELECT * FROM inserted
			  UNION ALL
			  SELECT existing.*
			  FROM scheduler_capacity_actions existing
			  WHERE existing.pool_id = $2
			    AND existing.state IN (
			      'requested', 'claimed', 'waiting-for-heartbeat',
			      'waiting-for-drain'
			    )
			    AND NOT EXISTS (SELECT 1 FROM inserted)
			  LIMIT 1
			)
			SELECT action.id::text, pool.pool_id, pool.provider,
			       pool.execution_zone, pool.architecture, pool.build_mode,
			       pool.profile_id, pool.image_id, pool.image_generation,
			       pool.selector, action.action_kind, action.requested_slots,
			       action.observed_slots, action.delta_slots, action.reason,
			       action.claim_owner, action.claim_fence,
			       action.claim_lease_expires_at
			FROM action
			JOIN scheduler_capacity_pool_state pool
			  ON pool.pool_id = action.pool_id
		`, actionID, poolID, kind, requestedSlots, observedSlots, reason)
		claim, err := scanCapacityAction(row)
		if err != nil {
			return fmt.Errorf("request capacity action: %w", err)
		}
		result = claim
		return nil
	})
	r.recordWrite(err)
	return result, err
}

// ClaimCapacityAction leases one requested or expired action with a new fence.
func (r *JobRepository) ClaimCapacityAction(
	ctx context.Context,
	owner string,
	lease time.Duration,
) (*builder.CapacityActionClaim, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > 256 || lease <= 0 {
		return nil, fmt.Errorf("capacity action owner and positive lease are required")
	}
	var result *builder.CapacityActionClaim
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		row := q.QueryRow(ctx, `
			WITH candidate AS (
			  SELECT action.id
			  FROM scheduler_capacity_actions action
			  WHERE action.next_attempt_at <= clock_timestamp()
			    AND (
			      action.state = 'requested'
			      OR (
			        action.state = 'claimed'
			        AND action.claim_lease_expires_at <= clock_timestamp()
			      )
			    )
			  ORDER BY action.requested_at, action.id
			  LIMIT 1
			  FOR UPDATE OF action SKIP LOCKED
			), action AS (
			  UPDATE scheduler_capacity_actions current
			  SET state = 'claimed', claim_owner = $1,
			      claim_fence = current.claim_fence + 1,
			      claim_lease_expires_at =
			        clock_timestamp() + $2::interval,
			      attempts = current.attempts + 1,
			      claimed_at = clock_timestamp(),
			      updated_at = clock_timestamp()
			  FROM candidate
			  WHERE current.id = candidate.id
			  RETURNING current.*
			)
			SELECT action.id::text, pool.pool_id, pool.provider,
			       pool.execution_zone, pool.architecture, pool.build_mode,
			       pool.profile_id, pool.image_id, pool.image_generation,
			       pool.selector, action.action_kind, action.requested_slots,
			       action.observed_slots, action.delta_slots, action.reason,
			       action.claim_owner, action.claim_fence,
			       action.claim_lease_expires_at
			FROM action
			JOIN scheduler_capacity_pool_state pool
			  ON pool.pool_id = action.pool_id
		`, owner, lease.String())
		claim, err := scanCapacityAction(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim capacity action: %w", err)
		}
		result = claim
		return nil
	})
	r.recordWrite(err)
	return result, err
}

func validateCapacityActionFence(
	ctx context.Context,
	q Querier,
	claim *builder.CapacityActionClaim,
) error {
	if claim == nil || claim.ID == "" || claim.Owner == "" || claim.Fence <= 0 {
		return fmt.Errorf("capacity action claim is incomplete")
	}
	var valid bool
	if err := q.QueryRow(ctx, `
		SELECT state = 'claimed'
		  AND claim_owner = $2
		  AND claim_fence = $3
		  AND claim_lease_expires_at > clock_timestamp()
		FROM scheduler_capacity_actions
		WHERE id = $1
		FOR UPDATE
	`, claim.ID, claim.Owner, claim.Fence).Scan(&valid); err != nil {
		return fmt.Errorf("validate capacity action fence: %w", err)
	}
	if !valid {
		return fmt.Errorf("capacity action fence is stale or expired")
	}
	return nil
}

func (r *JobRepository) RenewCapacityAction(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
	lease time.Duration,
) error {
	if lease <= 0 {
		return fmt.Errorf("capacity action renewal lease must be positive")
	}
	tag, err := r.db.Pool().Exec(ctx, `
		UPDATE scheduler_capacity_actions
		SET claim_lease_expires_at = clock_timestamp() + $4::interval,
		    updated_at = clock_timestamp()
		WHERE id = $1 AND state = 'claimed' AND claim_owner = $2
		  AND claim_fence = $3
		  AND claim_lease_expires_at > clock_timestamp()
	`, claim.ID, claim.Owner, claim.Fence, lease.String())
	if err == nil && tag.RowsAffected() != 1 {
		err = fmt.Errorf("capacity action fence is stale or expired")
	}
	r.recordWrite(err)
	return err
}

func scanCapacityInstance(row pgx.Row) (*builder.CapacityInstance, error) {
	var instance builder.CapacityInstance
	var attributes []byte
	var deleteActionID *string
	err := row.Scan(
		&instance.ID, &instance.PoolID, &instance.CreateActionID,
		&deleteActionID,
		&instance.Provider, &instance.ProviderInstanceID,
		&instance.OwnerToken, &instance.Generation, &instance.State,
		&instance.RemoteStateRef, &attributes,
	)
	if err != nil {
		return nil, err
	}
	if deleteActionID != nil {
		instance.DeleteActionID = *deleteActionID
	}
	if err := json.Unmarshal(attributes, &instance.Attributes); err != nil {
		return nil, fmt.Errorf("decode capacity instance attributes: %w", err)
	}
	return &instance, nil
}

func (r *JobRepository) ReserveCapacityInstance(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
) (*builder.CapacityInstance, error) {
	if claim == nil || claim.Kind != "scale-up" || claim.DeltaSlots != 1 {
		return nil, fmt.Errorf("one-slot scale-up claim is required")
	}
	var result *builder.CapacityInstance
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := validateCapacityActionFence(ctx, q, claim); err != nil {
			return err
		}
		instanceID, ownerToken := uuid.New(), uuid.New()
		providerID := "portage-capacity-" + instanceID.String()
		row := q.QueryRow(ctx, `
			INSERT INTO scheduler_capacity_instances (
			  id, pool_id, create_action_id, provider,
			  provider_instance_id, owner_token, state, attributes
			) VALUES (
			  $1, $2, $3, $4, $5, $6, 'provisioning', '{}'::jsonb
			)
			ON CONFLICT (create_action_id) DO UPDATE
			SET updated_at = scheduler_capacity_instances.updated_at
			RETURNING id::text, pool_id, create_action_id::text,
			          delete_action_id::text, provider,
			          provider_instance_id, owner_token::text, generation,
			          state, remote_state_ref, attributes
		`, instanceID, claim.Pool.ID, claim.ID, claim.Pool.Provider,
			providerID, ownerToken)
		instance, err := scanCapacityInstance(row)
		if err != nil {
			return fmt.Errorf("reserve capacity instance: %w", err)
		}
		result = instance
		return nil
	})
	r.recordWrite(err)
	return result, err
}

func encodeCapacityAttributes(attributes map[string]string) ([]byte, error) {
	if attributes == nil {
		attributes = map[string]string{}
	}
	data, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("encode capacity instance attributes: %w", err)
	}
	if len(data) > 64<<10 {
		return nil, fmt.Errorf("capacity instance attributes exceed 64 KiB")
	}
	return data, nil
}

func (r *JobRepository) RecordCapacityInstanceProvisioned(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
	instance *builder.CapacityInstance,
	remoteStateRef string,
	attributes map[string]string,
) error {
	data, err := encodeCapacityAttributes(attributes)
	if err != nil {
		return err
	}
	err = r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := validateCapacityActionFence(ctx, q, claim); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `
			UPDATE scheduler_capacity_instances
			SET remote_state_ref = $4, attributes = $5::jsonb,
			    updated_at = clock_timestamp()
			WHERE id = $1 AND owner_token = $2 AND generation = $3
			  AND pool_id = $6 AND state = 'provisioning'
		`, instance.ID, instance.OwnerToken, instance.Generation,
			remoteStateRef, string(data), claim.Pool.ID)
		if err != nil {
			return fmt.Errorf("record provisioned capacity instance: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("capacity instance ownership fence is stale")
		}
		return nil
	})
	r.recordWrite(err)
	return err
}

func capacityInstanceLabel(instanceID string) string {
	return "capacity-instance:" + instanceID
}

func (r *JobRepository) ConfirmCapacityInstanceHeartbeat(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
	instance *builder.CapacityInstance,
) (bool, error) {
	confirmed := false
	selector, err := json.Marshal(claim.Pool.Selector)
	if err != nil {
		return false, fmt.Errorf("encode capacity heartbeat selector: %w", err)
	}
	err = r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := validateCapacityActionFence(ctx, q, claim); err != nil {
			return err
		}
		var workers int
		if err := q.QueryRow(ctx, `
			SELECT count(*)
			FROM workers
			WHERE desired_state = 'active'
			  AND last_seen_at > clock_timestamp() - interval '45 seconds'
			  AND COALESCE(capabilities -> 'labels', '[]'::jsonb) ? $1
			  AND NOT EXISTS (
			    SELECT 1
			    FROM jsonb_array_elements_text($2::jsonb) required(label)
			    WHERE NOT (
			      COALESCE(capabilities -> 'labels', '[]'::jsonb)
			        ? required.label
			    )
			  )
		`, capacityInstanceLabel(instance.ID), string(selector)).Scan(&workers); err != nil {
			return fmt.Errorf("observe capacity instance heartbeat: %w", err)
		}
		if workers == 0 {
			return nil
		}
		tag, err := q.Exec(ctx, `
			UPDATE scheduler_capacity_instances
			SET state = 'active', heartbeat_observed_at = clock_timestamp(),
			    updated_at = clock_timestamp()
			WHERE id = $1 AND owner_token = $2 AND generation = $3
			  AND pool_id = $4 AND state = 'provisioning'
		`, instance.ID, instance.OwnerToken, instance.Generation, claim.Pool.ID)
		if err != nil {
			return fmt.Errorf("confirm capacity instance heartbeat: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("capacity instance heartbeat ownership fence is stale")
		}
		if err := completeCapacityActionTx(ctx, q, claim); err != nil {
			return err
		}
		confirmed = true
		return nil
	})
	r.recordWrite(err)
	return confirmed, err
}

func (r *JobRepository) SelectCapacityInstanceForDrain(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
) (*builder.CapacityInstance, error) {
	if claim == nil || claim.Kind != "scale-down" || claim.DeltaSlots != 1 {
		return nil, fmt.Errorf("one-slot scale-down claim is required")
	}
	var result *builder.CapacityInstance
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := validateCapacityActionFence(ctx, q, claim); err != nil {
			return err
		}
		row := q.QueryRow(ctx, `
			WITH existing AS (
			  SELECT instance.id
			  FROM scheduler_capacity_instances instance
			  WHERE instance.delete_action_id = $2
			    AND instance.pool_id = $1
			    AND instance.state IN ('draining', 'deleting')
			  FOR UPDATE OF instance
			), candidate AS (
			  SELECT instance.id
			  FROM scheduler_capacity_instances instance
			  WHERE instance.pool_id = $1 AND instance.state = 'active'
			    AND NOT EXISTS (SELECT 1 FROM existing)
			  ORDER BY instance.heartbeat_observed_at, instance.created_at,
			           instance.id
			  LIMIT 1
			  FOR UPDATE OF instance SKIP LOCKED
			), target AS (
			  SELECT id FROM existing
			  UNION ALL
			  SELECT id FROM candidate
			), selected AS (
			  UPDATE scheduler_capacity_instances instance
			  SET state = CASE
			        WHEN instance.state = 'deleting' THEN 'deleting'
			        ELSE 'draining'
			      END,
			      delete_action_id = $2,
			      drain_requested_at = COALESCE(
			        instance.drain_requested_at, clock_timestamp()
			      ),
			      updated_at = clock_timestamp()
			  FROM target
			  WHERE instance.id = target.id
			  RETURNING instance.*
			)
			SELECT id::text, pool_id, create_action_id::text,
			       delete_action_id::text, provider,
			       provider_instance_id, owner_token::text, generation,
			       state, remote_state_ref, attributes
			FROM selected
		`, claim.Pool.ID, claim.ID)
		instance, err := scanCapacityInstance(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("select capacity instance for drain: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE workers
			SET desired_state = 'draining', updated_at = clock_timestamp()
			WHERE COALESCE(capabilities -> 'labels', '[]'::jsonb) ? $1
		`, capacityInstanceLabel(instance.ID)); err != nil {
			return fmt.Errorf("drain capacity instance workers: %w", err)
		}
		result = instance
		return nil
	})
	r.recordWrite(err)
	return result, err
}

func (r *JobRepository) CapacityInstanceDrained(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
	instance *builder.CapacityInstance,
) (bool, error) {
	drained := false
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := validateCapacityActionFence(ctx, q, claim); err != nil {
			return err
		}
		return q.QueryRow(ctx, `
			SELECT NOT EXISTS (
			  SELECT 1
			  FROM workers worker
			  JOIN worker_leases lease ON lease.worker_id = worker.id
			  WHERE lease.expires_at > clock_timestamp()
			    AND COALESCE(
			          worker.capabilities -> 'labels', '[]'::jsonb
			        ) ? $1
			) AND NOT EXISTS (
			  SELECT 1
			  FROM phase_work_items work
			  JOIN workers worker ON worker.stable_name = work.claim_owner
			  WHERE work.state = 'claimed'
			    AND work.lease_expires_at > clock_timestamp()
			    AND COALESCE(
			          worker.capabilities -> 'labels', '[]'::jsonb
			        ) ? $1
			) AND EXISTS (
			  SELECT 1
			  FROM scheduler_capacity_instances instance
			  WHERE instance.id = $2 AND instance.owner_token = $3
			    AND instance.generation = $4 AND instance.pool_id = $5
			    AND instance.state = 'draining'
			)
		`, capacityInstanceLabel(instance.ID), instance.ID,
			instance.OwnerToken, instance.Generation, claim.Pool.ID).Scan(&drained)
	})
	r.recordWrite(err)
	return drained, err
}

func (r *JobRepository) BeginCapacityInstanceDelete(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
	instance *builder.CapacityInstance,
) error {
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := validateCapacityActionFence(ctx, q, claim); err != nil {
			return err
		}
		drained, err := capacityInstanceDrainedTx(ctx, q, claim, instance)
		if err != nil {
			return err
		}
		if !drained {
			return fmt.Errorf("capacity instance still owns live work")
		}
		tag, err := q.Exec(ctx, `
			UPDATE scheduler_capacity_instances
			SET state = 'deleting', updated_at = clock_timestamp()
			WHERE id = $1 AND owner_token = $2 AND generation = $3
			  AND pool_id = $4 AND state = 'draining'
		`, instance.ID, instance.OwnerToken, instance.Generation, claim.Pool.ID)
		if err != nil {
			return fmt.Errorf("begin capacity instance delete: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("capacity instance delete ownership fence is stale")
		}
		return nil
	})
	r.recordWrite(err)
	return err
}

func capacityInstanceDrainedTx(
	ctx context.Context,
	q Querier,
	claim *builder.CapacityActionClaim,
	instance *builder.CapacityInstance,
) (bool, error) {
	var drained bool
	err := q.QueryRow(ctx, `
		SELECT NOT EXISTS (
		  SELECT 1
		  FROM workers worker
		  JOIN worker_leases lease ON lease.worker_id = worker.id
		  WHERE lease.expires_at > clock_timestamp()
		    AND COALESCE(worker.capabilities -> 'labels', '[]'::jsonb) ? $1
		) AND NOT EXISTS (
		  SELECT 1
		  FROM phase_work_items work
		  JOIN workers worker ON worker.stable_name = work.claim_owner
		  WHERE work.state = 'claimed'
		    AND work.lease_expires_at > clock_timestamp()
		    AND COALESCE(worker.capabilities -> 'labels', '[]'::jsonb) ? $1
		) AND EXISTS (
		  SELECT 1 FROM scheduler_capacity_instances current
		  WHERE current.id = $2 AND current.owner_token = $3
		    AND current.generation = $4 AND current.pool_id = $5
		    AND current.state = 'draining'
		)
	`, capacityInstanceLabel(instance.ID), instance.ID, instance.OwnerToken,
		instance.Generation, claim.Pool.ID).Scan(&drained)
	if err != nil {
		return false, fmt.Errorf("verify capacity instance drain: %w", err)
	}
	return drained, nil
}

func completeCapacityActionTx(
	ctx context.Context,
	q Querier,
	claim *builder.CapacityActionClaim,
) error {
	tag, err := q.Exec(ctx, `
		UPDATE scheduler_capacity_actions
		SET state = 'completed', claim_owner = '',
		    claim_lease_expires_at = NULL, failure_detail = '',
		    completed_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE id = $1 AND state = 'claimed' AND claim_owner = $2
		  AND claim_fence = $3
		  AND claim_lease_expires_at > clock_timestamp()
	`, claim.ID, claim.Owner, claim.Fence)
	if err != nil {
		return fmt.Errorf("complete capacity action: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("capacity action completion fence is stale")
	}
	return nil
}

func (r *JobRepository) CompleteCapacityInstanceDelete(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
	instance *builder.CapacityInstance,
) error {
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := validateCapacityActionFence(ctx, q, claim); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `
			UPDATE scheduler_capacity_instances
			SET state = 'deleted', deleted_at = clock_timestamp(),
			    updated_at = clock_timestamp()
			WHERE id = $1 AND owner_token = $2 AND generation = $3
			  AND pool_id = $4 AND state = 'deleting'
		`, instance.ID, instance.OwnerToken, instance.Generation, claim.Pool.ID)
		if err != nil {
			return fmt.Errorf("complete capacity instance delete: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("capacity instance delete ownership fence is stale")
		}
		return completeCapacityActionTx(ctx, q, claim)
	})
	r.recordWrite(err)
	return err
}

// RetryCapacityAction releases a fenced claim back to the queue with bounded
// exponential backoff. Persistent instance bindings make a retry resume the
// same provider identity rather than allocating a second VM.
func (r *JobRepository) RetryCapacityAction(
	ctx context.Context,
	claim *builder.CapacityActionClaim,
	detail string,
) error {
	if len(detail) > capacityActionMaxDetail {
		detail = detail[:capacityActionMaxDetail]
	}
	tag, err := r.db.Pool().Exec(ctx, `
		UPDATE scheduler_capacity_actions
		SET state = 'requested', failure_detail = $4, claim_owner = '',
		    claim_lease_expires_at = NULL,
		    next_attempt_at = clock_timestamp() + make_interval(
		      secs => LEAST(300, power(2, LEAST(attempts, 8))::integer)
		    ),
		    updated_at = clock_timestamp()
		WHERE id = $1 AND state = 'claimed' AND claim_owner = $2
		  AND claim_fence = $3
		  AND claim_lease_expires_at > clock_timestamp()
	`, claim.ID, claim.Owner, claim.Fence, detail)
	if err == nil && tag.RowsAffected() != 1 {
		err = fmt.Errorf("capacity action failure fence is stale")
	}
	r.recordWrite(err)
	return err
}
