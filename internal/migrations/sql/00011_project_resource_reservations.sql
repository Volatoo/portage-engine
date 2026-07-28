-- +goose Up
-- IAM-1B1 accounts for the machine capacity held by each active attempt.
-- The project policy row remains the serialization lock for claim admission,
-- while this ledger is the authoritative usage source exposed to operators.

-- An online old executor could otherwise continue an attempt without creating
-- the new reservation. Require an explicit drain before this migration.
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
            'schema v11 requires all active build attempts and leases to be drained';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE project_policies
    ADD COLUMN max_active_vcpus integer NOT NULL DEFAULT 32
        CHECK (max_active_vcpus > 0),
    ADD COLUMN max_active_memory_mib integer NOT NULL DEFAULT 131072
        CHECK (max_active_memory_mib > 0),
    ADD COLUMN max_active_disk_gib integer NOT NULL DEFAULT 1000
        CHECK (max_active_disk_gib > 0);

CREATE TABLE project_resource_reservations (
    attempt_id uuid PRIMARY KEY REFERENCES build_attempts(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    resource_class text NOT NULL DEFAULT '',
    vcpus integer NOT NULL CHECK (vcpus > 0),
    memory_mib integer NOT NULL CHECK (memory_mib > 0),
    disk_gib integer NOT NULL CHECK (disk_gib > 0),
    phase text NOT NULL DEFAULT 'claimed'
        CHECK (phase IN ('claimed', 'provision', 'build', 'verify', 'publish')),
    state text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'released')),
    reserved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    phase_updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    released_at timestamptz,
    release_reason text NOT NULL DEFAULT '',
    CHECK (
        (state = 'active' AND released_at IS NULL) OR
        (state = 'released' AND released_at IS NOT NULL)
    )
);

CREATE INDEX project_resource_reservations_active_idx
    ON project_resource_reservations (project_id, phase, reserved_at)
    WHERE state = 'active';

CREATE INDEX project_resource_reservations_job_idx
    ON project_resource_reservations (job_id, reserved_at DESC);

-- +goose Down
DROP INDEX IF EXISTS project_resource_reservations_job_idx;
DROP INDEX IF EXISTS project_resource_reservations_active_idx;
DROP TABLE IF EXISTS project_resource_reservations;

ALTER TABLE project_policies
    DROP COLUMN IF EXISTS max_active_disk_gib,
    DROP COLUMN IF EXISTS max_active_memory_mib,
    DROP COLUMN IF EXISTS max_active_vcpus;
