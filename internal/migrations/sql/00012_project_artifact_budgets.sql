-- +goose Up
-- IAM-1B2a applies a hard, attempt-scoped byte budget to every artifact
-- generation retained in the private quarantine. The limit is snapshotted on
-- first use so lowering a project policy does not preempt an in-flight build.

-- An old executor could otherwise keep writing quarantine bytes without the
-- generation ledger. Require a complete drain at the protocol boundary.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM build_jobs
        WHERE state IN (
            'claimed', 'provisioning', 'forwarding', 'deploying', 'building',
            'collecting', 'verifying', 'signing', 'publishing'
        )
    ) OR EXISTS (
        SELECT 1 FROM worker_leases WHERE expires_at > clock_timestamp()
    ) THEN
        RAISE EXCEPTION
            'schema v12 requires all active build attempts and leases to be drained';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE project_policies
    ADD COLUMN max_artifact_bytes_per_job bigint NOT NULL DEFAULT 34359738368
        CHECK (max_artifact_bytes_per_job > 0);

ALTER TABLE signing_tasks
    ADD COLUMN max_output_bytes bigint NOT NULL DEFAULT 34359738368
        CHECK (max_output_bytes > 0);

CREATE TABLE project_artifact_budgets (
    attempt_id uuid PRIMARY KEY REFERENCES build_attempts(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    limit_bytes bigint NOT NULL CHECK (limit_bytes > 0),
    active_bytes bigint NOT NULL DEFAULT 0 CHECK (active_bytes >= 0),
    peak_bytes bigint NOT NULL DEFAULT 0 CHECK (peak_bytes >= active_bytes),
    state text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'released')),
    reserved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    released_at timestamptz,
    release_reason text NOT NULL DEFAULT '',
    CHECK (
        (state = 'active' AND released_at IS NULL) OR
        (state = 'released' AND released_at IS NOT NULL)
    )
);

CREATE TABLE artifact_generation_reservations (
    attempt_id uuid NOT NULL
        REFERENCES project_artifact_budgets(attempt_id) ON DELETE CASCADE,
    generation text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    state text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'released')),
    reserved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    released_at timestamptz,
    release_reason text NOT NULL DEFAULT '',
    PRIMARY KEY (attempt_id, generation),
    CHECK (length(generation) BETWEEN 1 AND 160),
    CHECK (
        (state = 'active' AND released_at IS NULL) OR
        (state = 'released' AND released_at IS NOT NULL)
    )
);

CREATE INDEX project_artifact_budgets_active_idx
    ON project_artifact_budgets (project_id, reserved_at)
    WHERE state = 'active';

CREATE INDEX project_artifact_budgets_job_idx
    ON project_artifact_budgets (job_id, reserved_at DESC);

-- +goose Down
DROP INDEX IF EXISTS project_artifact_budgets_job_idx;
DROP INDEX IF EXISTS project_artifact_budgets_active_idx;
DROP TABLE IF EXISTS artifact_generation_reservations;
DROP TABLE IF EXISTS project_artifact_budgets;

ALTER TABLE project_policies
    DROP COLUMN IF EXISTS max_artifact_bytes_per_job;

ALTER TABLE signing_tasks
    DROP COLUMN IF EXISTS max_output_bytes;
