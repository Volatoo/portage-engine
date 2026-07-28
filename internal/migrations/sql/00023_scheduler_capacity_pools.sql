-- +goose Up
-- SCHED-2A makes autoscale demand provider/capability-pool specific. A pool is
-- an executor environment from the immutable catalog; it is deliberately not
-- the disposable PVE builder VM created for one build attempt.

CREATE TABLE scheduler_capacity_pool_state (
    pool_id text PRIMARY KEY,
    provider text NOT NULL,
    execution_zone text NOT NULL,
    architecture text NOT NULL,
    build_mode text NOT NULL,
    profile_id text NOT NULL,
    image_id text NOT NULL,
    image_generation text NOT NULL,
    selector jsonb NOT NULL,
    mode text NOT NULL CHECK (mode IN ('off', 'observe')),
    active_slots integer NOT NULL DEFAULT 0 CHECK (active_slots >= 0),
    busy_slots integer NOT NULL DEFAULT 0 CHECK (busy_slots >= 0),
    backlog integer NOT NULL DEFAULT 0 CHECK (backlog >= 0),
    unschedulable_backlog integer NOT NULL DEFAULT 0
        CHECK (unschedulable_backlog >= 0),
    desired_slots integer NOT NULL DEFAULT 0 CHECK (desired_slots >= 0),
    recommendation text NOT NULL DEFAULT 'hold'
        CHECK (recommendation IN ('off', 'hold', 'scale-up', 'scale-down')),
    reason text NOT NULL DEFAULT '',
    under_target_since timestamptz,
    last_changed_at timestamptz,
    last_evaluated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT scheduler_capacity_pool_selector_array CHECK (
        jsonb_typeof(selector) = 'array'
        AND jsonb_array_length(selector) BETWEEN 1 AND 64
    )
);

CREATE INDEX scheduler_capacity_pool_provider_idx
    ON scheduler_capacity_pool_state (
        provider, execution_zone, architecture, profile_id
    );

-- +goose Down
DROP INDEX IF EXISTS scheduler_capacity_pool_provider_idx;
DROP TABLE IF EXISTS scheduler_capacity_pool_state;
