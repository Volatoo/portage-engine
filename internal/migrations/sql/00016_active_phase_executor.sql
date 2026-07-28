-- +goose Up
-- IAM-1B2b2c persists the non-secret hand-off state between independently
-- leased provision/build/verify/publish executors.
--
-- v15 executors cannot resume this context and must be drained before the
-- active cutover. Shadow plans are safe to retain.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM phase_work_items
        WHERE execution_mode = 'active'
          AND state IN ('ready', 'claimed', 'blocked')
    ) OR EXISTS (
        SELECT 1 FROM worker_leases WHERE expires_at > clock_timestamp()
    ) THEN
        RAISE EXCEPTION
            'schema v16 requires active phase plans and worker leases to be drained';
    END IF;
END
$$;
-- +goose StatementEnd

CREATE TABLE phase_execution_contexts (
    attempt_id uuid PRIMARY KEY REFERENCES build_attempts(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    attempt_fence bigint NOT NULL CHECK (attempt_fence > 0),
    context_sequence smallint NOT NULL DEFAULT 0 CHECK (context_sequence >= 0),
    context jsonb NOT NULL DEFAULT '{}'::jsonb,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX phase_execution_contexts_job_idx
    ON phase_execution_contexts (job_id, attempt_id);

-- +goose Down
DROP INDEX IF EXISTS phase_execution_contexts_job_idx;
DROP TABLE IF EXISTS phase_execution_contexts;
