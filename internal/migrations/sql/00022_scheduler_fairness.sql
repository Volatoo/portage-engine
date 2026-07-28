-- +goose Up
-- Scheduler fairness is PostgreSQL-authoritative. Each project owns one
-- serializable virtual-runtime row so multiple control-plane replicas cannot
-- independently favor the same busy project.

ALTER TABLE project_policies
    ADD COLUMN priority_weight integer NOT NULL DEFAULT 100
        CHECK (priority_weight BETWEEN 1 AND 1000),
    ADD COLUMN starvation_threshold_seconds integer NOT NULL DEFAULT 300
        CHECK (starvation_threshold_seconds BETWEEN 30 AND 86400);

CREATE TABLE project_scheduler_fairness (
    project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    admission_vruntime bigint NOT NULL DEFAULT 0
        CHECK (admission_vruntime >= 0),
    phase_vruntime bigint NOT NULL DEFAULT 0
        CHECK (phase_vruntime >= 0),
    admission_dispatches bigint NOT NULL DEFAULT 0
        CHECK (admission_dispatches >= 0),
    phase_dispatches bigint NOT NULL DEFAULT 0
        CHECK (phase_dispatches >= 0),
    last_admission_wait_seconds bigint NOT NULL DEFAULT 0
        CHECK (last_admission_wait_seconds >= 0),
    last_phase_wait_seconds bigint NOT NULL DEFAULT 0
        CHECK (last_phase_wait_seconds >= 0),
    last_admission_at timestamptz,
    last_phase_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

INSERT INTO project_scheduler_fairness (project_id)
SELECT id FROM projects
ON CONFLICT (project_id) DO NOTHING;

ALTER TABLE phase_work_items
    ADD COLUMN ready_since timestamptz;

UPDATE phase_work_items
SET ready_since = LEAST(available_at, updated_at)
WHERE state IN ('ready', 'claimed');

CREATE INDEX project_scheduler_admission_idx
    ON project_scheduler_fairness (admission_vruntime, project_id);

CREATE INDEX project_scheduler_phase_idx
    ON project_scheduler_fairness (phase_vruntime, project_id);

CREATE INDEX phase_work_items_fair_ready_idx
    ON phase_work_items (project_id, ready_since, available_at, id)
    WHERE execution_mode = 'active' AND state = 'ready';

-- This row stores recommendations only. A later provider-specific actuator
-- may consume desired_slots, but the scheduler never performs cloud side
-- effects while holding queue or fairness locks.
CREATE TABLE scheduler_autoscale_state (
    scope text PRIMARY KEY,
    mode text NOT NULL CHECK (mode IN ('off', 'observe')),
    active_slots integer NOT NULL DEFAULT 0 CHECK (active_slots >= 0),
    busy_slots integer NOT NULL DEFAULT 0 CHECK (busy_slots >= 0),
    backlog integer NOT NULL DEFAULT 0 CHECK (backlog >= 0),
    unschedulable_backlog integer NOT NULL DEFAULT 0
        CHECK (unschedulable_backlog >= 0),
    desired_slots integer NOT NULL DEFAULT 0 CHECK (desired_slots >= 0),
    recommendation text NOT NULL DEFAULT 'hold'
        CHECK (recommendation IN ('off', 'hold', 'scale-up', 'scale-down')),
    reason text NOT NULL DEFAULT '',
    under_target_since timestamptz,
    last_changed_at timestamptz,
    last_evaluated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- +goose Down
DROP TABLE IF EXISTS scheduler_autoscale_state;
DROP INDEX IF EXISTS phase_work_items_fair_ready_idx;
DROP INDEX IF EXISTS project_scheduler_phase_idx;
DROP INDEX IF EXISTS project_scheduler_admission_idx;

ALTER TABLE phase_work_items
    DROP COLUMN IF EXISTS ready_since;

DROP TABLE IF EXISTS project_scheduler_fairness;

ALTER TABLE project_policies
    DROP COLUMN IF EXISTS starvation_threshold_seconds,
    DROP COLUMN IF EXISTS priority_weight;
