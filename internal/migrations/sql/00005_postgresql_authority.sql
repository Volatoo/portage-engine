-- +goose Up
-- DB-4 removes the legacy JSON job snapshot from the live read/write path and
-- stores bounded, redacted build logs in PostgreSQL for cross-replica reads.

CREATE TABLE build_log_chunks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    attempt_id uuid REFERENCES build_attempts(id) ON DELETE SET NULL,
    level text NOT NULL DEFAULT 'info',
    message text NOT NULL CHECK (octet_length(message) <= 16384),
    occurred_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX build_log_chunks_job_cursor_idx
    ON build_log_chunks (job_id, id);

INSERT INTO runtime_settings (
    scope, settings_key, version, value, updated_by
) VALUES (
    'system', 'job_authority', 1,
    '{"authority":"postgresql","legacy_json_reads":false,"legacy_json_writes":false}'::jsonb,
    'migration-00005'
)
ON CONFLICT (scope, settings_key)
DO UPDATE SET
    version = runtime_settings.version + 1,
    value = EXCLUDED.value,
    updated_by = EXCLUDED.updated_by,
    updated_at = clock_timestamp();

-- +goose Down
DELETE FROM runtime_settings
WHERE scope = 'system' AND settings_key = 'job_authority';

DROP INDEX IF EXISTS build_log_chunks_job_cursor_idx;
DROP TABLE IF EXISTS build_log_chunks;
