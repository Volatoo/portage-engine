-- +goose Up
-- IAM-1B2b1 adds durable per-project pipeline checkpoint caps. Existing
-- executors keep their attempt lease while waiting, but may not cross a phase
-- boundary until PostgreSQL atomically admits the reservation.

-- Old executors do not understand phase-cap rejections. Drain before enabling
-- the contract so no in-flight binary can bypass it.
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
            'schema v13 requires all active build attempts and leases to be drained';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE project_policies
    ADD COLUMN max_claimed_attempts integer NOT NULL DEFAULT 4
        CHECK (max_claimed_attempts > 0),
    ADD COLUMN max_provision_attempts integer NOT NULL DEFAULT 4
        CHECK (max_provision_attempts > 0),
    ADD COLUMN max_build_attempts integer NOT NULL DEFAULT 4
        CHECK (max_build_attempts > 0),
    ADD COLUMN max_verify_attempts integer NOT NULL DEFAULT 4
        CHECK (max_verify_attempts > 0),
    ADD COLUMN max_publish_attempts integer NOT NULL DEFAULT 4
        CHECK (max_publish_attempts > 0);

-- +goose Down
ALTER TABLE project_policies
    DROP COLUMN IF EXISTS max_publish_attempts,
    DROP COLUMN IF EXISTS max_verify_attempts,
    DROP COLUMN IF EXISTS max_build_attempts,
    DROP COLUMN IF EXISTS max_provision_attempts,
    DROP COLUMN IF EXISTS max_claimed_attempts;
