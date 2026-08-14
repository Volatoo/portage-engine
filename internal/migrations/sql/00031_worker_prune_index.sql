-- +goose Up
-- Stale worker pruning first removes explainable scoring evidence by worker_id.
-- Without this index both the explicit DELETE and the RESTRICT foreign-key
-- check scan the full decision history during synchronous startup janitorial
-- work.

CREATE INDEX scheduler_worker_decisions_worker_idx
    ON scheduler_worker_decisions(worker_id);

-- +goose Down
DROP INDEX IF EXISTS scheduler_worker_decisions_worker_idx;
