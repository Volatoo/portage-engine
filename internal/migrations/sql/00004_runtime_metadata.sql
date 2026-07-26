-- +goose Up
-- DB-3 binds infrastructure and artifacts to the exact fenced build attempt,
-- and makes image-factory snapshots durable instead of WebUI-only files.

ALTER TABLE infra_instances
    ADD COLUMN job_id uuid REFERENCES build_jobs(id) ON DELETE SET NULL,
    ADD COLUMN fence_token bigint CHECK (fence_token > 0),
    ADD COLUMN failure_detail text NOT NULL DEFAULT '',
    ADD COLUMN deleted_at timestamptz;

CREATE INDEX infra_instances_job_idx
    ON infra_instances (job_id, created_at DESC);
CREATE INDEX infra_instances_live_idx
    ON infra_instances (state, updated_at)
    WHERE deleted_at IS NULL;

ALTER TABLE artifacts
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

ALTER TABLE factory_runs
    ADD COLUMN source_key text NOT NULL DEFAULT 'image-factory-status',
    ADD COLUMN source_revision text NOT NULL DEFAULT '';

CREATE UNIQUE INDEX factory_runs_source_digest_idx
    ON factory_runs (source_key, input_lock_digest);

-- +goose Down
DROP INDEX IF EXISTS factory_runs_source_digest_idx;
ALTER TABLE factory_runs
    DROP COLUMN IF EXISTS source_revision,
    DROP COLUMN IF EXISTS source_key;

ALTER TABLE artifacts
    DROP COLUMN IF EXISTS updated_at;

DROP INDEX IF EXISTS infra_instances_live_idx;
DROP INDEX IF EXISTS infra_instances_job_idx;
ALTER TABLE infra_instances
    DROP COLUMN IF EXISTS deleted_at,
    DROP COLUMN IF EXISTS failure_detail,
    DROP COLUMN IF EXISTS fence_token,
    DROP COLUMN IF EXISTS job_id;
