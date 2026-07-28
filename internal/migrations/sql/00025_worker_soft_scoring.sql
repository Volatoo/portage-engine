-- +goose Up
-- SCHED-3 records explainable, pull-aware worker scoring without turning
-- pressure or recent failures into hard scheduling constraints.

CREATE TABLE scheduler_worker_decisions (
    id bigserial PRIMARY KEY,
    work_kind text NOT NULL CHECK (work_kind IN ('admission', 'phase')),
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    phase text NOT NULL DEFAULT '',
    worker_id uuid NOT NULL REFERENCES workers(id) ON DELETE RESTRICT,
    candidate_count integer NOT NULL CHECK (candidate_count > 0),
    pressure_score integer NOT NULL CHECK (pressure_score BETWEEN 0 AND 1000),
    recent_failures integer NOT NULL CHECK (recent_failures >= 0),
    reason text NOT NULL,
    selected_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX scheduler_worker_decisions_job_idx
    ON scheduler_worker_decisions(job_id, selected_at DESC);
CREATE INDEX scheduler_worker_decisions_recent_idx
    ON scheduler_worker_decisions(selected_at DESC);

-- +goose Down
DROP INDEX IF EXISTS scheduler_worker_decisions_recent_idx;
DROP INDEX IF EXISTS scheduler_worker_decisions_job_idx;
DROP TABLE IF EXISTS scheduler_worker_decisions;
