-- +goose Up
-- DB-0 creates the durable control-plane vocabulary. The server does not yet
-- use these tables as its primary read/write path; that cutover is DB-1/DB-2.

CREATE TABLE projects (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE catalog_versions (
    id uuid PRIMARY KEY,
    project_id uuid REFERENCES projects(id) ON DELETE CASCADE,
    revision text NOT NULL,
    digest text NOT NULL,
    source_uri text NOT NULL DEFAULT '',
    manifest jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, revision)
);

CREATE TABLE targets (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name text NOT NULL,
    architecture text NOT NULL,
    profile text NOT NULL,
    service_manager text NOT NULL DEFAULT '',
    repository_revision text NOT NULL,
    image_revision text NOT NULL DEFAULT '',
    configuration jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

CREATE TABLE workers (
    id uuid PRIMARY KEY,
    stable_name text NOT NULL UNIQUE,
    desired_state text NOT NULL DEFAULT 'active',
    capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
    max_slots integer NOT NULL CHECK (max_slots > 0),
    last_seen_at timestamptz,
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE build_jobs (
    id uuid PRIMARY KEY,
    project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
    target_id uuid REFERENCES targets(id) ON DELETE RESTRICT,
    idempotency_key text,
    package_atom text NOT NULL,
    state text NOT NULL,
    priority integer NOT NULL DEFAULT 0,
    request jsonb NOT NULL,
    request_digest text NOT NULL,
    cancel_requested_at timestamptz,
    terminal_reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    UNIQUE (project_id, idempotency_key)
);

CREATE INDEX build_jobs_claim_idx
    ON build_jobs (priority DESC, created_at, id)
    WHERE state = 'queued' AND cancel_requested_at IS NULL;
CREATE INDEX build_jobs_project_created_idx
    ON build_jobs (project_id, created_at DESC);
CREATE INDEX build_jobs_target_created_idx
    ON build_jobs (target_id, created_at DESC);

CREATE TABLE build_attempts (
    id uuid PRIMARY KEY,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    attempt_no integer NOT NULL CHECK (attempt_no > 0),
    state text NOT NULL,
    worker_id uuid REFERENCES workers(id) ON DELETE SET NULL,
    fence_token bigint NOT NULL CHECK (fence_token > 0),
    failure_class text NOT NULL DEFAULT '',
    failure_detail text NOT NULL DEFAULT '',
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (job_id, attempt_no),
    UNIQUE (job_id, fence_token)
);

CREATE INDEX build_attempts_worker_state_idx
    ON build_attempts (worker_id, state);

CREATE TABLE worker_leases (
    id uuid PRIMARY KEY,
    worker_id uuid NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    attempt_id uuid NOT NULL UNIQUE REFERENCES build_attempts(id) ON DELETE CASCADE,
    fence_token bigint NOT NULL,
    expires_at timestamptz NOT NULL,
    renewed_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (worker_id, fence_token)
);

CREATE INDEX worker_leases_expiry_idx ON worker_leases (expires_at);

CREATE TABLE infra_instances (
    id uuid PRIMARY KEY,
    project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
    attempt_id uuid REFERENCES build_attempts(id) ON DELETE SET NULL,
    provider text NOT NULL,
    provider_instance_id text NOT NULL,
    state text NOT NULL,
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    owner_token text NOT NULL,
    remote_state_ref text NOT NULL DEFAULT '',
    attributes jsonb NOT NULL DEFAULT '{}'::jsonb,
    cleanup_after timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_instance_id)
);

CREATE INDEX infra_instances_cleanup_idx
    ON infra_instances (cleanup_after)
    WHERE cleanup_after IS NOT NULL;

CREATE TABLE artifacts (
    id uuid PRIMARY KEY,
    project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
    job_id uuid REFERENCES build_jobs(id) ON DELETE SET NULL,
    attempt_id uuid REFERENCES build_attempts(id) ON DELETE SET NULL,
    kind text NOT NULL,
    state text NOT NULL,
    digest_algorithm text NOT NULL DEFAULT 'sha256',
    digest text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    media_type text NOT NULL DEFAULT 'application/octet-stream',
    location text NOT NULL,
    lineage jsonb NOT NULL DEFAULT '{}'::jsonb,
    retention_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    UNIQUE (digest_algorithm, digest, location)
);

CREATE INDEX artifacts_job_idx ON artifacts (job_id, created_at);

CREATE TABLE job_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    attempt_id uuid REFERENCES build_attempts(id) ON DELETE SET NULL,
    event_type text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX job_events_job_cursor_idx ON job_events (job_id, id);

CREATE TABLE factory_runs (
    id uuid PRIMARY KEY,
    project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
    target_id uuid REFERENCES targets(id) ON DELETE RESTRICT,
    state text NOT NULL,
    input_lock_digest text NOT NULL,
    manifest jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

CREATE TABLE factory_steps (
    id uuid PRIMARY KEY,
    run_id uuid NOT NULL REFERENCES factory_runs(id) ON DELETE CASCADE,
    step_name text NOT NULL,
    state text NOT NULL,
    sequence integer NOT NULL CHECK (sequence >= 0),
    detail jsonb NOT NULL DEFAULT '{}'::jsonb,
    started_at timestamptz,
    completed_at timestamptz,
    UNIQUE (run_id, sequence),
    UNIQUE (run_id, step_name)
);

CREATE TABLE evidence_refs (
    id uuid PRIMARY KEY,
    project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
    job_id uuid REFERENCES build_jobs(id) ON DELETE SET NULL,
    factory_run_id uuid REFERENCES factory_runs(id) ON DELETE SET NULL,
    kind text NOT NULL,
    location text NOT NULL,
    digest text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE runtime_settings (
    scope text NOT NULL,
    settings_key text NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    value jsonb NOT NULL,
    secret_refs jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, settings_key)
);

CREATE TABLE audit_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor text NOT NULL,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text NOT NULL,
    request_id text NOT NULL DEFAULT '',
    source_ip inet,
    detail jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_resource_idx
    ON audit_events (resource_type, resource_id, id DESC);

CREATE TABLE outbox_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    topic text NOT NULL,
    aggregate_type text NOT NULL,
    aggregate_id text NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    available_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error text NOT NULL DEFAULT ''
);

CREATE INDEX outbox_events_pending_idx
    ON outbox_events (available_at, id)
    WHERE published_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS runtime_settings;
DROP TABLE IF EXISTS evidence_refs;
DROP TABLE IF EXISTS factory_steps;
DROP TABLE IF EXISTS factory_runs;
DROP TABLE IF EXISTS job_events;
DROP TABLE IF EXISTS artifacts;
DROP TABLE IF EXISTS infra_instances;
DROP TABLE IF EXISTS worker_leases;
DROP TABLE IF EXISTS build_attempts;
DROP TABLE IF EXISTS build_jobs;
DROP TABLE IF EXISTS workers;
DROP TABLE IF EXISTS targets;
DROP TABLE IF EXISTS catalog_versions;
DROP TABLE IF EXISTS projects;
