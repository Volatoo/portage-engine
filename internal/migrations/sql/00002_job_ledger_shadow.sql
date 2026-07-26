-- +goose Up
-- DB-1 keeps the legacy memory queue authoritative while PostgreSQL receives
-- a complete, comparable shadow ledger.

ALTER TABLE build_jobs
    ADD COLUMN status_snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN status_digest text NOT NULL DEFAULT '',
    ADD COLUMN ledger_revision bigint NOT NULL DEFAULT 1 CHECK (ledger_revision > 0),
    ADD COLUMN legacy_visible boolean NOT NULL DEFAULT true,
    ADD COLUMN source text NOT NULL DEFAULT 'api',
    ADD COLUMN deleted_at timestamptz;

CREATE UNIQUE INDEX build_jobs_global_idempotency_idx
    ON build_jobs (idempotency_key)
    WHERE project_id IS NULL
      AND idempotency_key IS NOT NULL;

CREATE INDEX build_jobs_legacy_visible_idx
    ON build_jobs (created_at DESC, id)
    WHERE legacy_visible = true;

-- +goose Down
DROP INDEX IF EXISTS build_jobs_legacy_visible_idx;
DROP INDEX IF EXISTS build_jobs_global_idempotency_idx;

ALTER TABLE build_jobs
    DROP COLUMN IF EXISTS deleted_at,
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS legacy_visible,
    DROP COLUMN IF EXISTS ledger_revision,
    DROP COLUMN IF EXISTS status_digest,
    DROP COLUMN IF EXISTS status_snapshot;
