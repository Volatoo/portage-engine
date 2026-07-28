-- +goose Up
-- IAM-1B2b2b moves the outbound worker gateway's session, command and upload
-- authority into PostgreSQL. Older executors cannot understand delivery or
-- upload fences, so the migration is deliberately drain-only.

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM build_jobs
        WHERE state IN (
            'claimed', 'provisioning', 'forwarding', 'deploying', 'building',
            'collecting', 'verifying', 'signing', 'publishing'
        )
    ) OR EXISTS (
        SELECT 1 FROM worker_leases WHERE expires_at > clock_timestamp()
    ) THEN
        RAISE EXCEPTION
            'schema v15 requires all active build attempts and leases to be drained';
    END IF;
END
$$;
-- +goose StatementEnd

CREATE TABLE worker_gateway_sessions (
    worker_id text PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    attempt_id uuid NOT NULL UNIQUE REFERENCES build_attempts(id) ON DELETE CASCADE,
    attempt_fence bigint NOT NULL CHECK (attempt_fence > 0),
    state text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'revoked')),
    connected_at timestamptz,
    last_seen_at timestamptz,
    revoked_at timestamptz,
    revoke_reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (
        (state = 'active' AND revoked_at IS NULL)
        OR (state = 'revoked' AND revoked_at IS NOT NULL)
    )
);

CREATE INDEX worker_gateway_sessions_state_idx
    ON worker_gateway_sessions (state, last_seen_at);

CREATE TABLE worker_gateway_commands (
    id uuid PRIMARY KEY,
    worker_id text NOT NULL REFERENCES worker_gateway_sessions(worker_id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    attempt_id uuid NOT NULL REFERENCES build_attempts(id) ON DELETE CASCADE,
    action text NOT NULL CHECK (action IN ('build', 'verify', 'collect')),
    request jsonb NOT NULL,
    state text NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'delivered', 'completed', 'failed', 'canceled')),
    delivery_fence bigint NOT NULL DEFAULT 0 CHECK (delivery_fence >= 0),
    delivery_lease_expires_at timestamptz,
    response jsonb,
    error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    delivered_at timestamptz,
    completed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (
        (state = 'delivered'
            AND delivery_fence > 0
            AND delivery_lease_expires_at IS NOT NULL
            AND delivered_at IS NOT NULL
            AND completed_at IS NULL)
        OR
        (state <> 'delivered' AND delivery_lease_expires_at IS NULL)
    ),
    CHECK (
        (state IN ('completed', 'failed', 'canceled') AND completed_at IS NOT NULL)
        OR
        (state NOT IN ('completed', 'failed', 'canceled') AND completed_at IS NULL)
    )
);

CREATE INDEX worker_gateway_commands_pull_idx
    ON worker_gateway_commands (worker_id, created_at, id)
    WHERE state IN ('queued', 'delivered');

CREATE INDEX worker_gateway_commands_attempt_idx
    ON worker_gateway_commands (attempt_id, created_at);

CREATE TABLE worker_gateway_uploads (
    id uuid PRIMARY KEY,
    worker_id text NOT NULL REFERENCES worker_gateway_sessions(worker_id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    attempt_id uuid NOT NULL REFERENCES build_attempts(id) ON DELETE CASCADE,
    destination text NOT NULL,
    max_bytes bigint NOT NULL CHECK (max_bytes > 0),
    state text NOT NULL DEFAULT 'ready'
        CHECK (state IN ('ready', 'uploading', 'completed', 'canceled')),
    upload_fence bigint NOT NULL DEFAULT 0 CHECK (upload_fence >= 0),
    upload_lease_expires_at timestamptz,
    digest text NOT NULL DEFAULT '',
    size_bytes bigint CHECK (size_bytes IS NULL OR size_bytes >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (
        (state = 'uploading'
            AND upload_fence > 0
            AND upload_lease_expires_at IS NOT NULL
            AND completed_at IS NULL)
        OR
        (state <> 'uploading' AND upload_lease_expires_at IS NULL)
    ),
    CHECK (
        (state = 'completed'
            AND completed_at IS NOT NULL
            AND digest ~ '^[a-f0-9]{64}$'
            AND size_bytes IS NOT NULL)
        OR
        (state <> 'completed' AND completed_at IS NULL)
    )
);

CREATE INDEX worker_gateway_uploads_attempt_idx
    ON worker_gateway_uploads (attempt_id, created_at);

-- +goose Down
DROP INDEX IF EXISTS worker_gateway_uploads_attempt_idx;
DROP TABLE IF EXISTS worker_gateway_uploads;
DROP INDEX IF EXISTS worker_gateway_commands_attempt_idx;
DROP INDEX IF EXISTS worker_gateway_commands_pull_idx;
DROP TABLE IF EXISTS worker_gateway_commands;
DROP INDEX IF EXISTS worker_gateway_sessions_state_idx;
DROP TABLE IF EXISTS worker_gateway_sessions;
