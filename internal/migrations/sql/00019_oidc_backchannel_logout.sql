-- +goose Up
-- IAM-1D records provider logout-token replay state without retaining the
-- signed JWT or its raw jti. Session revocation remains PostgreSQL-authoritative.

CREATE TABLE iam_logout_events (
    id uuid PRIMARY KEY,
    issuer text NOT NULL CHECK (length(issuer) BETWEEN 1 AND 2048),
    token_id_hash text NOT NULL UNIQUE
        CHECK (token_id_hash ~ '^[0-9a-f]{64}$'),
    subject text NOT NULL DEFAULT '' CHECK (length(subject) <= 512),
    provider_session_id text NOT NULL DEFAULT ''
        CHECK (length(provider_session_id) <= 512),
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    revoked_sessions bigint NOT NULL DEFAULT 0 CHECK (revoked_sessions >= 0),
    CHECK (subject <> '' OR provider_session_id <> ''),
    CHECK (expires_at > issued_at)
);

CREATE INDEX iam_logout_events_received_idx
    ON iam_logout_events (received_at DESC);
CREATE INDEX iam_logout_events_provider_session_idx
    ON iam_logout_events (issuer, provider_session_id)
    WHERE provider_session_id <> '';

-- +goose Down
DROP INDEX IF EXISTS iam_logout_events_provider_session_idx;
DROP INDEX IF EXISTS iam_logout_events_received_idx;
DROP TABLE IF EXISTS iam_logout_events;
