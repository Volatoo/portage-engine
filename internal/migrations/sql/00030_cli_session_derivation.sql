-- +goose Up
-- IAM-1H2 records where a session came from. The device flow mints a second
-- session from the browser session that approved it, and nothing linked the
-- two: revoking the browser session left its CLI token alive until its own
-- expiry, and the session list could not tell an operator which row was a
-- terminal. The link is also the revocation unit, so a deleted parent must
-- never leave a child token behind.

ALTER TABLE iam_sessions
    ADD COLUMN session_kind text NOT NULL DEFAULT 'browser'
        CHECK (session_kind IN ('browser', 'cli')),
    ADD COLUMN derived_from_session_id uuid
        REFERENCES iam_sessions(id) ON DELETE CASCADE,
    ADD CONSTRAINT iam_sessions_derived_shape CHECK (
        derived_from_session_id IS NULL
        OR (session_kind = 'cli' AND derived_from_session_id <> id)
    );

-- Revocation walks children by parent, and the sessions list filters on kind.
CREATE INDEX iam_sessions_derived_from_idx
    ON iam_sessions (derived_from_session_id)
    WHERE derived_from_session_id IS NOT NULL;

-- Sessions the device flow already minted keep the browser default: the
-- pre-migration code stored no link to reconstruct, and guessing one from
-- issue timestamps could revoke an unrelated browser session. Those tokens
-- disappear within the server session max lifetime.

-- +goose Down
DROP INDEX IF EXISTS iam_sessions_derived_from_idx;
ALTER TABLE iam_sessions
    DROP CONSTRAINT IF EXISTS iam_sessions_derived_shape,
    DROP COLUMN IF EXISTS derived_from_session_id,
    DROP COLUMN IF EXISTS session_kind;
