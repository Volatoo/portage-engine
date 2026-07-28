-- +goose Up
-- IAM-1E makes executor compatibility a PostgreSQL-enforced hard constraint.
-- Old binaries do not write requirements or advertise canonical labels, so a
-- rolling deployment must first drain every non-terminal job.

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM build_jobs
        WHERE state NOT IN ('completed', 'success', 'failed', 'canceled')
    ) OR EXISTS (
        SELECT 1 FROM worker_leases WHERE expires_at > clock_timestamp()
    ) OR EXISTS (
        SELECT 1
        FROM phase_work_items
        WHERE state IN ('ready', 'claimed')
    ) THEN
        RAISE EXCEPTION
            'schema v20 requires all queued/running jobs, worker leases, and ready/claimed phase work to be drained';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE build_jobs
    ADD COLUMN required_capabilities jsonb NOT NULL
        DEFAULT '["legacy:unroutable"]'::jsonb,
    ADD CONSTRAINT build_jobs_required_capabilities_array
        CHECK (
            jsonb_typeof(required_capabilities) = 'array'
            AND jsonb_array_length(required_capabilities) BETWEEN 1 AND 64
        );

ALTER TABLE phase_work_items
    ADD COLUMN required_capabilities jsonb NOT NULL
        DEFAULT '["legacy:unroutable"]'::jsonb,
    ADD CONSTRAINT phase_work_required_capabilities_array
        CHECK (
            jsonb_typeof(required_capabilities) = 'array'
            AND jsonb_array_length(required_capabilities) BETWEEN 1 AND 64
        );

CREATE INDEX workers_capability_labels_gin_idx
    ON workers USING gin ((capabilities -> 'labels'));

CREATE INDEX phase_work_items_requirements_gin_idx
    ON phase_work_items USING gin (required_capabilities);

-- +goose Down
DROP INDEX IF EXISTS phase_work_items_requirements_gin_idx;
DROP INDEX IF EXISTS workers_capability_labels_gin_idx;

ALTER TABLE phase_work_items
    DROP CONSTRAINT IF EXISTS phase_work_required_capabilities_array,
    DROP COLUMN IF EXISTS required_capabilities;

ALTER TABLE build_jobs
    DROP CONSTRAINT IF EXISTS build_jobs_required_capabilities_array,
    DROP COLUMN IF EXISTS required_capabilities;
