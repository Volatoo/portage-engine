-- +goose Up
-- SCHED-2B separates desired capacity from provider side effects. Actions and
-- instances have independent fences so scheduler transactions never call PVE
-- and a stale actuator cannot create, drain, or delete infrastructure.

ALTER TABLE workers
    ADD CONSTRAINT workers_desired_state_known
        CHECK (desired_state IN ('active', 'draining'));

ALTER TABLE scheduler_autoscale_state
    DROP CONSTRAINT scheduler_autoscale_state_mode_check,
    ADD CONSTRAINT scheduler_autoscale_state_mode_check
        CHECK (mode IN ('off', 'observe', 'actuate'));

ALTER TABLE scheduler_capacity_pool_state
    DROP CONSTRAINT scheduler_capacity_pool_state_mode_check,
    ADD CONSTRAINT scheduler_capacity_pool_state_mode_check
        CHECK (mode IN ('off', 'observe', 'actuate'));
ALTER TABLE scheduler_capacity_pool_state
    ADD COLUMN provider_max_slots integer NOT NULL DEFAULT 0
        CHECK (provider_max_slots >= 0);

CREATE TABLE scheduler_capacity_actions (
    id uuid PRIMARY KEY,
    pool_id text NOT NULL
        REFERENCES scheduler_capacity_pool_state(pool_id) ON DELETE RESTRICT,
    action_kind text NOT NULL
        CHECK (action_kind IN ('scale-up', 'scale-down')),
    state text NOT NULL DEFAULT 'requested'
        CHECK (
            state IN (
                'requested', 'claimed', 'waiting-for-heartbeat',
                'waiting-for-drain', 'completed', 'failed', 'canceled'
            )
        ),
    requested_slots integer NOT NULL CHECK (requested_slots >= 0),
    observed_slots integer NOT NULL CHECK (observed_slots >= 0),
    delta_slots integer NOT NULL CHECK (delta_slots > 0),
    reason text NOT NULL DEFAULT '',
    claim_owner text NOT NULL DEFAULT '',
    claim_fence bigint NOT NULL DEFAULT 0 CHECK (claim_fence >= 0),
    claim_lease_expires_at timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    failure_detail text NOT NULL DEFAULT '',
    requested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    claimed_at timestamptz,
    completed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT scheduler_capacity_action_claim_shape CHECK (
        (state = 'claimed' AND claim_owner <> ''
            AND claim_lease_expires_at IS NOT NULL)
        OR state <> 'claimed'
    )
);

CREATE UNIQUE INDEX scheduler_capacity_actions_one_open_pool_idx
    ON scheduler_capacity_actions(pool_id)
    WHERE state IN (
        'requested', 'claimed', 'waiting-for-heartbeat',
        'waiting-for-drain'
    );

CREATE INDEX scheduler_capacity_actions_claim_idx
    ON scheduler_capacity_actions(next_attempt_at, requested_at, id)
    WHERE state IN ('requested', 'claimed');

CREATE TABLE scheduler_capacity_instances (
    id uuid PRIMARY KEY,
    pool_id text NOT NULL
        REFERENCES scheduler_capacity_pool_state(pool_id) ON DELETE RESTRICT,
    create_action_id uuid NOT NULL
        REFERENCES scheduler_capacity_actions(id) ON DELETE RESTRICT UNIQUE,
    delete_action_id uuid
        REFERENCES scheduler_capacity_actions(id) ON DELETE RESTRICT UNIQUE,
    provider text NOT NULL,
    provider_instance_id text NOT NULL,
    owner_token uuid NOT NULL UNIQUE,
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    state text NOT NULL
        CHECK (
            state IN (
                'provisioning', 'active', 'draining', 'deleting',
                'deleted', 'failed'
            )
        ),
    remote_state_ref text NOT NULL DEFAULT '',
    attributes jsonb NOT NULL DEFAULT '{}'::jsonb,
    failure_detail text NOT NULL DEFAULT '',
    heartbeat_observed_at timestamptz,
    drain_requested_at timestamptz,
    deleted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (provider, provider_instance_id)
);

CREATE INDEX scheduler_capacity_instances_pool_state_idx
    ON scheduler_capacity_instances(pool_id, state, created_at);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'portage_actuator') THEN
        GRANT SELECT ON scheduler_capacity_pool_state, workers,
            worker_leases, phase_work_items, goose_db_version
            TO portage_actuator;
        GRANT UPDATE (desired_state, updated_at) ON workers
            TO portage_actuator;
        GRANT SELECT, INSERT, UPDATE, DELETE ON
            scheduler_capacity_actions, scheduler_capacity_instances
            TO portage_actuator;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX IF EXISTS scheduler_capacity_instances_pool_state_idx;
DROP TABLE IF EXISTS scheduler_capacity_instances;
DROP INDEX IF EXISTS scheduler_capacity_actions_claim_idx;
DROP INDEX IF EXISTS scheduler_capacity_actions_one_open_pool_idx;
DROP TABLE IF EXISTS scheduler_capacity_actions;
ALTER TABLE scheduler_capacity_pool_state
    DROP COLUMN IF EXISTS provider_max_slots;
ALTER TABLE scheduler_capacity_pool_state
    DROP CONSTRAINT scheduler_capacity_pool_state_mode_check,
    ADD CONSTRAINT scheduler_capacity_pool_state_mode_check
        CHECK (mode IN ('off', 'observe'));
ALTER TABLE scheduler_autoscale_state
    DROP CONSTRAINT scheduler_autoscale_state_mode_check,
    ADD CONSTRAINT scheduler_autoscale_state_mode_check
        CHECK (mode IN ('off', 'observe'));
ALTER TABLE workers
    DROP CONSTRAINT IF EXISTS workers_desired_state_known;
