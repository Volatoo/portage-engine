-- +goose Up
-- IAM-1A makes project admission a PostgreSQL-authoritative decision. The
-- policy row is also the per-project serialization lock used by submit and
-- scheduler claim transactions, so multiple API replicas/workers cannot
-- oversubscribe a project after checking a stale in-memory counter.

CREATE TABLE project_policies (
    project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    suspended boolean NOT NULL DEFAULT false,
    max_queued_jobs integer NOT NULL DEFAULT 100 CHECK (max_queued_jobs > 0),
    max_active_jobs integer NOT NULL DEFAULT 4 CHECK (max_active_jobs > 0),
    max_daily_submissions integer NOT NULL DEFAULT 500 CHECK (max_daily_submissions > 0),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_by text NOT NULL DEFAULT 'migration',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO project_policies (project_id)
SELECT id FROM projects
ON CONFLICT (project_id) DO NOTHING;

CREATE INDEX build_jobs_project_active_idx
    ON build_jobs (project_id, state)
    WHERE legacy_visible = true
      AND state IN (
          'claimed', 'provisioning', 'forwarding', 'deploying', 'building',
          'collecting', 'verifying', 'signing', 'publishing'
      );

CREATE INDEX build_jobs_project_daily_admission_idx
    ON build_jobs (project_id, created_at)
    WHERE legacy_visible = true;

-- +goose Down
DROP INDEX IF EXISTS build_jobs_project_daily_admission_idx;
DROP INDEX IF EXISTS build_jobs_project_active_idx;
DROP TABLE IF EXISTS project_policies;
