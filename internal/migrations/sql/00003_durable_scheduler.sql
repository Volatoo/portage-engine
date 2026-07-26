-- +goose Up
-- DB-2 makes PostgreSQL the queue/claim authority. Fence tokens are scoped to
-- a job (build_attempts already enforces UNIQUE(job_id, fence_token)); making
-- them unique per worker incorrectly prevents one worker from processing
-- fence=1 for two unrelated jobs.

ALTER TABLE worker_leases
    DROP CONSTRAINT IF EXISTS worker_leases_worker_id_fence_token_key;

ALTER TABLE build_jobs
    ADD COLUMN max_attempts integer NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now();

DROP INDEX IF EXISTS build_jobs_claim_idx;
CREATE INDEX build_jobs_claim_idx
    ON build_jobs (priority DESC, next_attempt_at, created_at, id)
    WHERE state = 'queued' AND cancel_requested_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS build_jobs_claim_idx;
CREATE INDEX build_jobs_claim_idx
    ON build_jobs (priority DESC, created_at, id)
    WHERE state = 'queued' AND cancel_requested_at IS NULL;

ALTER TABLE build_jobs
    DROP COLUMN IF EXISTS next_attempt_at,
    DROP COLUMN IF EXISTS max_attempts;

ALTER TABLE worker_leases
    ADD CONSTRAINT worker_leases_worker_id_fence_token_key
    UNIQUE (worker_id, fence_token);
