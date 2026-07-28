-- +goose Up
-- IAM-1B3 reserves the maximum runtime and estimated cloud cost before an
-- attempt can be claimed. A terminal transition settles actual wall time and
-- releases the unused reservation. Failure storms trigger a separate,
-- time-bounded abuse suspension without overwriting an owner's manual
-- suspension decision.

-- An older executor can claim work without opening or settling this ledger.
-- Require all non-terminal work to be drained before crossing the protocol
-- boundary.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM build_jobs
        WHERE state NOT IN ('completed', 'success', 'failed', 'canceled')
    ) OR EXISTS (
        SELECT 1 FROM worker_leases WHERE expires_at > clock_timestamp()
    ) THEN
        RAISE EXCEPTION
            'schema v17 requires queued jobs, active attempts, and worker leases to be drained';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE project_policies
    ADD COLUMN max_daily_build_seconds bigint NOT NULL DEFAULT 86400
        CHECK (max_daily_build_seconds > 0),
    ADD COLUMN max_daily_cloud_cost_microunits bigint NOT NULL DEFAULT 1000000000
        CHECK (max_daily_cloud_cost_microunits > 0),
    ADD COLUMN max_failures_per_hour integer NOT NULL DEFAULT 20
        CHECK (max_failures_per_hour > 0),
    ADD COLUMN abuse_cooldown_seconds integer NOT NULL DEFAULT 3600
        CHECK (abuse_cooldown_seconds BETWEEN 60 AND 604800),
    ADD COLUMN abuse_suspended_until timestamptz,
    ADD COLUMN abuse_reason text NOT NULL DEFAULT '',
    ADD COLUMN abuse_generation bigint NOT NULL DEFAULT 0
        CHECK (abuse_generation >= 0);

CREATE TABLE project_attempt_usage (
    attempt_id uuid PRIMARY KEY REFERENCES build_attempts(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    budget_day date NOT NULL,
    max_runtime_seconds bigint NOT NULL CHECK (max_runtime_seconds > 0),
    cloud_cost_microunits_per_minute bigint NOT NULL
        CHECK (cloud_cost_microunits_per_minute > 0),
    reserved_build_seconds bigint NOT NULL CHECK (reserved_build_seconds > 0),
    reserved_cloud_cost_microunits bigint NOT NULL
        CHECK (reserved_cloud_cost_microunits > 0),
    charged_build_seconds bigint NOT NULL DEFAULT 0
        CHECK (charged_build_seconds >= 0),
    charged_cloud_cost_microunits bigint NOT NULL DEFAULT 0
        CHECK (charged_cloud_cost_microunits >= 0),
    metering_started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    cloud_started_at timestamptz,
    state text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'settled')),
    settled_at timestamptz,
    settlement_reason text NOT NULL DEFAULT '',
    CHECK (
        (state = 'active' AND settled_at IS NULL) OR
        (state = 'settled' AND settled_at IS NOT NULL)
    )
);

CREATE INDEX project_attempt_usage_daily_idx
    ON project_attempt_usage (project_id, budget_day, state);
CREATE INDEX project_attempt_usage_active_deadline_idx
    ON project_attempt_usage (
        metering_started_at,
        max_runtime_seconds
    ) WHERE state = 'active';

-- Existing historical jobs remain readable, but every job accepted after this
-- migration requires the budget-aware executor protocol.
ALTER TABLE build_jobs
    ALTER COLUMN minimum_executor_protocol SET DEFAULT 4;

-- +goose Down
ALTER TABLE build_jobs
    ALTER COLUMN minimum_executor_protocol SET DEFAULT 0;

DROP INDEX IF EXISTS project_attempt_usage_active_deadline_idx;
DROP INDEX IF EXISTS project_attempt_usage_daily_idx;
DROP TABLE IF EXISTS project_attempt_usage;

ALTER TABLE project_policies
    DROP COLUMN IF EXISTS abuse_generation,
    DROP COLUMN IF EXISTS abuse_reason,
    DROP COLUMN IF EXISTS abuse_suspended_until,
    DROP COLUMN IF EXISTS abuse_cooldown_seconds,
    DROP COLUMN IF EXISTS max_failures_per_hour,
    DROP COLUMN IF EXISTS max_daily_cloud_cost_microunits,
    DROP COLUMN IF EXISTS max_daily_build_seconds;
