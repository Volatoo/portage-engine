-- +goose Up
-- IAM-1B2b2a introduces a durable, independently leased phase-work queue.
-- The existing executor continues to drive the pipeline until the worker
-- command spool is durable; this table is the authority required for that
-- later cutover, not an in-memory projection.

-- Executors before v14 cannot create or fence phase work. Drain so a rolling
-- deployment cannot mix whole-pipeline and phase-work ownership.
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
            'schema v14 requires all active build attempts and leases to be drained';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE project_resource_reservations
    DROP CONSTRAINT project_resource_reservations_phase_check;

ALTER TABLE project_resource_reservations
    ADD CONSTRAINT project_resource_reservations_phase_check
    CHECK (phase IN ('claimed', 'waiting', 'provision', 'build', 'verify', 'publish'));

CREATE TABLE phase_work_items (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    attempt_id uuid NOT NULL REFERENCES build_attempts(id) ON DELETE CASCADE,
    phase text NOT NULL
        CHECK (phase IN ('provision', 'build', 'verify', 'publish')),
    sequence smallint NOT NULL CHECK (sequence > 0),
    execution_mode text NOT NULL DEFAULT 'shadow'
        CHECK (execution_mode IN ('shadow', 'active')),
    state text NOT NULL DEFAULT 'blocked'
        CHECK (state IN (
            'blocked', 'ready', 'claimed', 'completed', 'failed', 'canceled'
        )),
    available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    claim_owner text NOT NULL DEFAULT '',
    claim_fence bigint NOT NULL DEFAULT 0 CHECK (claim_fence >= 0),
    lease_expires_at timestamptz,
    started_at timestamptz,
    finished_at timestamptz,
    error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (attempt_id, phase),
    UNIQUE (attempt_id, sequence),
    CHECK (
        (state = 'claimed'
            AND claim_owner <> ''
            AND claim_fence > 0
            AND lease_expires_at IS NOT NULL
            AND started_at IS NOT NULL
            AND finished_at IS NULL)
        OR
        (state <> 'claimed' AND lease_expires_at IS NULL)
    ),
    CHECK (
        (state IN ('completed', 'failed', 'canceled') AND finished_at IS NOT NULL)
        OR
        (state NOT IN ('completed', 'failed', 'canceled') AND finished_at IS NULL)
    )
);

CREATE INDEX phase_work_items_ready_idx
    ON phase_work_items (available_at, created_at, id)
    WHERE state = 'ready';

CREATE INDEX phase_work_items_attempt_idx
    ON phase_work_items (attempt_id, sequence);

CREATE INDEX phase_work_items_lease_idx
    ON phase_work_items (lease_expires_at)
    WHERE state = 'claimed';

-- +goose Down
DROP INDEX IF EXISTS phase_work_items_lease_idx;
DROP INDEX IF EXISTS phase_work_items_attempt_idx;
DROP INDEX IF EXISTS phase_work_items_ready_idx;
DROP TABLE IF EXISTS phase_work_items;

ALTER TABLE project_resource_reservations
    DROP CONSTRAINT project_resource_reservations_phase_check;

ALTER TABLE project_resource_reservations
    ADD CONSTRAINT project_resource_reservations_phase_check
    CHECK (phase IN ('claimed', 'provision', 'build', 'verify', 'publish'));
