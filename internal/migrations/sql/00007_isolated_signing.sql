-- +goose Up
-- P0-B moves release signing out of builders and control-plane replicas.
-- A signer claims digest-bound requests through PostgreSQL and writes only to
-- a job-private quarantine generation. Publication remains a separately
-- fenced control-plane operation.

CREATE TABLE signing_tasks (
    id uuid PRIMARY KEY,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    attempt_id uuid NOT NULL REFERENCES build_attempts(id) ON DELETE CASCADE,
    attempt_fence bigint NOT NULL CHECK (attempt_fence > 0),
    state text NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'claimed', 'completed', 'failed', 'canceled')),
    source_token text NOT NULL CHECK (source_token ~ '^[a-f0-9]{32}$'),
    architecture text NOT NULL,
    input_manifest jsonb NOT NULL,
    output_manifest jsonb,
    signing_key_id text NOT NULL DEFAULT '',
    owner text NOT NULL DEFAULT '',
    claim_fence bigint NOT NULL DEFAULT 0 CHECK (claim_fence >= 0),
    lease_expires_at timestamptz,
    claim_attempts integer NOT NULL DEFAULT 0 CHECK (claim_attempts >= 0),
    error_detail text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    UNIQUE (job_id, attempt_id)
);

CREATE INDEX signing_tasks_claim_idx
    ON signing_tasks (created_at, id)
    WHERE state IN ('queued', 'claimed');

-- Existing artifacts used a digest+location uniqueness key. An isolated
-- signer intentionally records both the verified unsigned object and the
-- signed publication candidate at distinct locations.
CREATE INDEX artifacts_signing_state_idx
    ON artifacts (job_id, attempt_id, state, created_at);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'portage_signer') THEN
        GRANT SELECT, UPDATE ON signing_tasks TO portage_signer;
        GRANT SELECT ON build_jobs, build_attempts, worker_leases, workers,
                        artifacts, goose_db_version TO portage_signer;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX IF EXISTS artifacts_signing_state_idx;
DROP INDEX IF EXISTS signing_tasks_claim_idx;
DROP TABLE IF EXISTS signing_tasks;
