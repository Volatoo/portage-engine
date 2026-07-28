-- +goose Up
-- IAM-0 makes project ownership part of the durable job identity and adds the
-- external-subject/member vocabulary required by OIDC-backed authorization.

INSERT INTO projects (id, name, description)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    'default',
    'Compatibility project for pre-IAM jobs and trusted-alpha operators'
)
ON CONFLICT (name) DO NOTHING;

CREATE TABLE iam_subjects (
    id uuid PRIMARY KEY,
    issuer text NOT NULL,
    subject text NOT NULL,
    preferred_username text NOT NULL DEFAULT '',
    display_name text NOT NULL DEFAULT '',
    email text NOT NULL DEFAULT '',
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject)
);

CREATE TABLE project_memberships (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    subject_id uuid NOT NULL REFERENCES iam_subjects(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('viewer', 'developer', 'maintainer', 'owner')),
    granted_by_subject_id uuid REFERENCES iam_subjects(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, subject_id)
);

CREATE INDEX project_memberships_subject_idx
    ON project_memberships (subject_id, project_id);

ALTER TABLE build_jobs
    ADD COLUMN requested_by_subject_id uuid REFERENCES iam_subjects(id) ON DELETE SET NULL;

UPDATE build_jobs
SET project_id = (
    SELECT id
    FROM projects
    WHERE name = 'default'
)
WHERE project_id IS NULL;

ALTER TABLE build_jobs
    ALTER COLUMN project_id SET NOT NULL;

ALTER TABLE audit_events
    ADD COLUMN actor_subject_id uuid REFERENCES iam_subjects(id) ON DELETE SET NULL,
    ADD COLUMN project_id uuid REFERENCES projects(id) ON DELETE SET NULL,
    ADD COLUMN outcome text NOT NULL DEFAULT 'success'
        CHECK (outcome IN ('success', 'denied', 'failure'));

CREATE INDEX audit_events_actor_cursor_idx
    ON audit_events (actor_subject_id, id DESC);
CREATE INDEX audit_events_project_cursor_idx
    ON audit_events (project_id, id DESC);

-- +goose Down
DROP INDEX IF EXISTS audit_events_project_cursor_idx;
DROP INDEX IF EXISTS audit_events_actor_cursor_idx;
ALTER TABLE audit_events
    DROP COLUMN IF EXISTS outcome,
    DROP COLUMN IF EXISTS project_id,
    DROP COLUMN IF EXISTS actor_subject_id;
ALTER TABLE build_jobs
    ALTER COLUMN project_id DROP NOT NULL,
    DROP COLUMN IF EXISTS requested_by_subject_id;
DROP INDEX IF EXISTS project_memberships_subject_idx;
DROP TABLE IF EXISTS project_memberships;
DROP TABLE IF EXISTS iam_subjects;
