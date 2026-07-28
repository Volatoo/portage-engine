-- +goose Up
-- IAM-1C keeps OIDC token bytes out of PostgreSQL while making their
-- lifecycle and revocation durable across every control-plane replica.

CREATE TABLE iam_subject_security (
    subject_id uuid PRIMARY KEY REFERENCES iam_subjects(id) ON DELETE CASCADE,
    tokens_valid_after timestamptz NOT NULL DEFAULT '1970-01-01 00:00:00+00',
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO iam_subject_security (subject_id)
SELECT id FROM iam_subjects
ON CONFLICT (subject_id) DO NOTHING;

CREATE TABLE iam_sessions (
    id uuid PRIMARY KEY,
    subject_id uuid NOT NULL REFERENCES iam_subjects(id) ON DELETE CASCADE,
    token_hash text NOT NULL UNIQUE
        CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    provider_session_id text NOT NULL DEFAULT ''
        CHECK (length(provider_session_id) <= 512),
    provider_token_id text NOT NULL DEFAULT ''
        CHECK (length(provider_token_id) <= 512),
    issued_at timestamptz NOT NULL,
    authenticated_at timestamptz,
    expires_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    acr text NOT NULL DEFAULT '' CHECK (length(acr) <= 512),
    amr text[] NOT NULL DEFAULT '{}',
    revoked_at timestamptz,
    revoked_by_subject_id uuid REFERENCES iam_subjects(id) ON DELETE SET NULL,
    revoke_reason text NOT NULL DEFAULT '' CHECK (length(revoke_reason) <= 1024),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > issued_at),
    CHECK (authenticated_at IS NULL OR authenticated_at <= expires_at)
);

CREATE INDEX iam_sessions_subject_last_seen_idx
    ON iam_sessions (subject_id, last_seen_at DESC);
CREATE INDEX iam_sessions_active_expiry_idx
    ON iam_sessions (expires_at)
    WHERE revoked_at IS NULL;
CREATE INDEX iam_sessions_provider_sid_idx
    ON iam_sessions (provider_session_id)
    WHERE provider_session_id <> '';

-- +goose Down
DROP INDEX IF EXISTS iam_sessions_provider_sid_idx;
DROP INDEX IF EXISTS iam_sessions_active_expiry_idx;
DROP INDEX IF EXISTS iam_sessions_subject_last_seen_idx;
DROP TABLE IF EXISTS iam_sessions;
DROP TABLE IF EXISTS iam_subject_security;
