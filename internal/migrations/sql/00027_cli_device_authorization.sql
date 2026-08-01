-- +goose Up
-- IAM-1H adds a short-lived, one-time browser approval bridge for public CLI
-- clients. The high-entropy device capability is represented only by its
-- SHA-256 digest. PostgreSQL owns expiry, polling cadence, approval and
-- single-consumption state across every API replica.

CREATE TABLE iam_device_authorizations (
    id uuid PRIMARY KEY,
    device_code_hash text NOT NULL UNIQUE
        CHECK (device_code_hash ~ '^[0-9a-f]{64}$'),
    user_code text NOT NULL UNIQUE
        CHECK (user_code ~ '^[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}$'),
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'denied', 'consumed', 'expired')),
    -- If the approving subject/session is deleted during the short device
    -- window, delete this bridge too. A later poll then fails as expired;
    -- SET NULL would conflict with the approved/consumed invariants below.
    subject_id uuid REFERENCES iam_subjects(id) ON DELETE CASCADE,
    approver_session_id uuid REFERENCES iam_sessions(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    interval_seconds integer NOT NULL
        CHECK (interval_seconds BETWEEN 1 AND 60),
    last_polled_at timestamptz,
    approved_at timestamptz,
    denied_at timestamptz,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK (
        (status = 'pending' AND subject_id IS NULL AND approver_session_id IS NULL
            AND approved_at IS NULL AND denied_at IS NULL AND consumed_at IS NULL)
        OR
        (status = 'approved' AND subject_id IS NOT NULL AND approver_session_id IS NOT NULL
            AND approved_at IS NOT NULL AND denied_at IS NULL AND consumed_at IS NULL)
        OR
        (status = 'denied' AND denied_at IS NOT NULL AND consumed_at IS NULL)
        OR
        (status = 'consumed' AND subject_id IS NOT NULL AND approver_session_id IS NOT NULL
            AND approved_at IS NOT NULL AND denied_at IS NULL AND consumed_at IS NOT NULL)
        OR
        (status = 'expired' AND consumed_at IS NULL)
    )
);

CREATE INDEX iam_device_authorizations_expiry_idx
    ON iam_device_authorizations (expires_at);
CREATE INDEX iam_device_authorizations_pending_expiry_idx
    ON iam_device_authorizations (expires_at)
    WHERE status IN ('pending', 'approved');

-- +goose Down
DROP INDEX IF EXISTS iam_device_authorizations_pending_expiry_idx;
DROP INDEX IF EXISTS iam_device_authorizations_expiry_idx;
DROP TABLE IF EXISTS iam_device_authorizations;
