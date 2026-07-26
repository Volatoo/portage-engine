-- +goose Up
-- P0-A makes infrastructure reclamation a PostgreSQL-leased queue. The
-- Terraform workspace itself lives on the shared DATA_DIR volume; these fields
-- prevent two control-plane replicas from destroying the same resource.

ALTER TABLE infra_instances
    ADD COLUMN cleanup_owner text NOT NULL DEFAULT '',
    ADD COLUMN cleanup_fence bigint NOT NULL DEFAULT 0 CHECK (cleanup_fence >= 0),
    ADD COLUMN cleanup_lease_expires_at timestamptz,
    ADD COLUMN cleanup_attempts integer NOT NULL DEFAULT 0 CHECK (cleanup_attempts >= 0),
    ADD COLUMN next_cleanup_at timestamptz NOT NULL DEFAULT clock_timestamp();

CREATE INDEX infra_instances_cleanup_claim_idx
    ON infra_instances (next_cleanup_at, cleanup_after, updated_at)
    WHERE deleted_at IS NULL AND remote_state_ref <> '';

-- +goose Down
DROP INDEX IF EXISTS infra_instances_cleanup_claim_idx;

ALTER TABLE infra_instances
    DROP COLUMN IF EXISTS next_cleanup_at,
    DROP COLUMN IF EXISTS cleanup_attempts,
    DROP COLUMN IF EXISTS cleanup_lease_expires_at,
    DROP COLUMN IF EXISTS cleanup_fence,
    DROP COLUMN IF EXISTS cleanup_owner;
