-- +goose Up
-- DISTCC-ALPHA is intentionally migration 29. Migrations 27 (CLI device flow)
-- and 28 (monitoring) are owned by parallel milestones and are expected to be
-- integrated before this file.
--
-- Compile workers are a credential-minimal inventory separate from Portage
-- builders/executors. Exact compatibility is a hard routing boundary; normal
-- project fairness and quotas remain authoritative before reservation.

CREATE TABLE compile_workers (
    id uuid PRIMARY KEY,
    stable_name text NOT NULL UNIQUE CHECK (stable_name <> ''),
    endpoint text NOT NULL CHECK (endpoint <> ''),
    pool_id text NOT NULL CHECK (pool_id <> ''),
    architecture text NOT NULL CHECK (architecture <> ''),
    chost text NOT NULL CHECK (chost <> ''),
    compiler_digest text NOT NULL CHECK (compiler_digest <> ''),
    toolchain_image_generation text NOT NULL
        CHECK (toolchain_image_generation <> ''),
    cpu_features text[] NOT NULL DEFAULT '{}'::text[]
        CHECK (cardinality(cpu_features) <= 64),
    network_zone text NOT NULL CHECK (network_zone <> ''),
    project_trust_domain text NOT NULL
        CHECK (project_trust_domain <> ''),
    max_slots integer NOT NULL CHECK (max_slots BETWEEN 1 AND 256),
    desired_state text NOT NULL DEFAULT 'active'
        CHECK (desired_state IN ('active', 'draining')),
    next_fence bigint NOT NULL DEFAULT 0 CHECK (next_fence >= 0),
    last_heartbeat_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (
        stable_name, pool_id, architecture, chost, compiler_digest,
        toolchain_image_generation, cpu_features, network_zone,
        project_trust_domain
    )
);

CREATE INDEX compile_workers_exact_pool_idx
    ON compile_workers(pool_id, desired_state, last_heartbeat_at DESC);

CREATE TABLE compile_slot_leases (
    id uuid PRIMARY KEY,
    worker_id uuid NOT NULL
        REFERENCES compile_workers(id) ON DELETE RESTRICT,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE RESTRICT,
    attempt_id uuid NOT NULL REFERENCES build_attempts(id) ON DELETE RESTRICT,
    attempt_fence bigint NOT NULL CHECK (attempt_fence > 0),
    builder_id text NOT NULL CHECK (builder_id <> ''),
    builder_network_identity text NOT NULL
        CHECK (builder_network_identity <> ''),
    fence_token bigint NOT NULL CHECK (fence_token > 0),
    slots integer NOT NULL CHECK (slots BETWEEN 1 AND 256),
    pool_id text NOT NULL CHECK (pool_id <> ''),
    fallback_policy text NOT NULL
        CHECK (fallback_policy IN ('local', 'blocked')),
    state text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'released', 'expired')),
    expires_at timestamptz NOT NULL,
    reserved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    renewed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    released_at timestamptz,
    failure_reason text NOT NULL DEFAULT ''
        CHECK (failure_reason IN (
            '', 'capacity', 'connect', 'lease-expired', 'lease-fenced',
            'pool-mismatch', 'remote-compile', 'worker-stale', 'policy',
            'unknown'
        )),
    UNIQUE (worker_id, fence_token),
    CONSTRAINT compile_slot_lease_release_shape CHECK (
        (state = 'active' AND released_at IS NULL)
        OR (state <> 'active' AND released_at IS NOT NULL)
    )
);

CREATE INDEX compile_slot_leases_live_worker_idx
    ON compile_slot_leases(worker_id, expires_at)
    WHERE state = 'active';
CREATE INDEX compile_slot_leases_attempt_idx
    ON compile_slot_leases(attempt_id, state, expires_at);

-- Actual compiler-wrapper/sidecar observations use only reviewed enum labels.
-- Job, project, worker, package, endpoint, and arbitrary error text never
-- become metric labels.
CREATE TABLE compile_observations (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lease_id uuid NOT NULL
        REFERENCES compile_slot_leases(id) ON DELETE RESTRICT,
    outcome text NOT NULL
        CHECK (outcome IN ('local', 'remote', 'hit', 'fallback')),
    failure_reason text NOT NULL DEFAULT ''
        CHECK (failure_reason IN (
            '', 'capacity', 'connect', 'lease-expired', 'lease-fenced',
            'pool-mismatch', 'remote-compile', 'worker-stale', 'policy',
            'unknown'
        )),
    compile_count bigint NOT NULL CHECK (compile_count > 0),
    network_bytes bigint NOT NULL DEFAULT 0 CHECK (network_bytes >= 0),
    queue_millis bigint NOT NULL DEFAULT 0 CHECK (queue_millis >= 0),
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX compile_observations_metrics_idx
    ON compile_observations(observed_at, outcome, failure_reason);

-- +goose Down
DROP INDEX IF EXISTS compile_observations_metrics_idx;
DROP TABLE IF EXISTS compile_observations;
DROP INDEX IF EXISTS compile_slot_leases_attempt_idx;
DROP INDEX IF EXISTS compile_slot_leases_live_worker_idx;
DROP TABLE IF EXISTS compile_slot_leases;
DROP INDEX IF EXISTS compile_workers_exact_pool_idx;
DROP TABLE IF EXISTS compile_workers;
