-- +goose Up
-- IAM-1F records the public half of every workload credential so issuer
-- rotation and revocation are enforced by every Worker Gateway replica.
-- Private keys and certificate PEM are deliberately excluded.

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM worker_gateway_sessions WHERE state = 'active'
    ) THEN
        RAISE EXCEPTION
            'schema v21 requires active worker gateway sessions to be drained';
    END IF;
END
$$;
-- +goose StatementEnd

CREATE TABLE workload_issuer_generations (
    fingerprint text PRIMARY KEY CHECK (fingerprint ~ '^[a-f0-9]{64}$'),
    issuer_id text NOT NULL CHECK (length(issuer_id) BETWEEN 1 AND 128),
    provider text NOT NULL CHECK (length(provider) BETWEEN 1 AND 64),
    subject text NOT NULL CHECK (length(subject) <= 4096),
    serial_hex text NOT NULL CHECK (serial_hex ~ '^[a-f0-9]+$'),
    not_before timestamptz NOT NULL,
    not_after timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'draining', 'revoked')),
    first_seen_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_issued_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at timestamptz,
    revoke_reason text NOT NULL DEFAULT '',
    CHECK (not_after > not_before),
    CHECK (
        (state = 'revoked' AND revoked_at IS NOT NULL)
        OR (state <> 'revoked' AND revoked_at IS NULL)
    ),
    UNIQUE (issuer_id, fingerprint)
);

CREATE UNIQUE INDEX workload_issuer_one_active_idx
    ON workload_issuer_generations (issuer_id)
    WHERE state = 'active';

CREATE INDEX workload_issuer_state_idx
    ON workload_issuer_generations (state, not_after);

CREATE TABLE workload_certificates (
    fingerprint text PRIMARY KEY CHECK (fingerprint ~ '^[a-f0-9]{64}$'),
    serial_hex text NOT NULL CHECK (serial_hex ~ '^[a-f0-9]+$'),
    issuer_fingerprint text NOT NULL
        REFERENCES workload_issuer_generations(fingerprint) ON DELETE RESTRICT,
    worker_id text NOT NULL UNIQUE
        REFERENCES worker_gateway_sessions(worker_id) ON DELETE CASCADE,
    job_id uuid NOT NULL REFERENCES build_jobs(id) ON DELETE CASCADE,
    attempt_id uuid NOT NULL REFERENCES build_attempts(id) ON DELETE CASCADE,
    attempt_fence bigint NOT NULL CHECK (attempt_fence > 0),
    not_before timestamptz NOT NULL,
    not_after timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'revoked')),
    issued_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    first_seen_at timestamptz,
    last_seen_at timestamptz,
    revoked_at timestamptz,
    revoke_reason text NOT NULL DEFAULT '',
    CHECK (not_after > not_before),
    CHECK (
        (state = 'revoked' AND revoked_at IS NOT NULL)
        OR (state = 'active' AND revoked_at IS NULL)
    ),
    UNIQUE (issuer_fingerprint, serial_hex)
);

CREATE INDEX workload_certificates_state_idx
    ON workload_certificates (state, not_after);

CREATE INDEX workload_certificates_attempt_idx
    ON workload_certificates (attempt_id, issued_at);

ALTER TABLE worker_gateway_sessions
    ADD COLUMN certificate_fingerprint text
        REFERENCES workload_certificates(fingerprint) ON DELETE RESTRICT;

CREATE UNIQUE INDEX worker_gateway_session_certificate_idx
    ON worker_gateway_sessions (certificate_fingerprint)
    WHERE certificate_fingerprint IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS worker_gateway_session_certificate_idx;
ALTER TABLE worker_gateway_sessions
    DROP COLUMN IF EXISTS certificate_fingerprint;
DROP INDEX IF EXISTS workload_certificates_attempt_idx;
DROP INDEX IF EXISTS workload_certificates_state_idx;
DROP TABLE IF EXISTS workload_certificates;
DROP INDEX IF EXISTS workload_issuer_state_idx;
DROP INDEX IF EXISTS workload_issuer_one_active_idx;
DROP TABLE IF EXISTS workload_issuer_generations;
