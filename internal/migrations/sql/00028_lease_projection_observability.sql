-- +goose Up
-- MON-2 persists the low-cardinality lease-expiry totals which are updated in
-- the same transactions as the corresponding scheduler recovery. The closed
-- key set prevents job, project, package, pool, or provider identities from
-- becoming metric dimensions.

ALTER TABLE worker_leases
    ADD COLUMN lease_kind text;

-- Existing live leases predate the explicit field. Capture their current
-- immutable meaning once during migration; all new leases write it directly.
UPDATE worker_leases leases
SET lease_kind = CASE
    WHEN COALESCE(workers.capabilities -> 'labels', '[]'::jsonb)
         ? 'worker-kind:admission'
    THEN 'admission'
    ELSE 'attempt'
END
FROM workers
WHERE workers.id = leases.worker_id;

ALTER TABLE worker_leases
    ALTER COLUMN lease_kind SET DEFAULT 'attempt',
    ALTER COLUMN lease_kind SET NOT NULL,
    ADD CONSTRAINT worker_leases_kind_known
        CHECK (lease_kind IN ('attempt', 'admission'));

CREATE TABLE scheduler_lease_expiry_counters (
    lease_kind text NOT NULL,
    result text NOT NULL,
    total bigint NOT NULL DEFAULT 0 CHECK (total >= 0),
    last_occurred_at timestamptz,
    PRIMARY KEY (lease_kind, result),
    CONSTRAINT scheduler_lease_expiry_counter_key_known CHECK (
        (
            lease_kind IN ('attempt', 'admission')
            AND result IN ('requeued', 'failed', 'canceled')
        )
        OR (lease_kind = 'phase' AND result = 'reclaimed')
    )
);

INSERT INTO scheduler_lease_expiry_counters (lease_kind, result)
VALUES
    ('attempt', 'requeued'),
    ('attempt', 'failed'),
    ('attempt', 'canceled'),
    ('admission', 'requeued'),
    ('admission', 'failed'),
    ('admission', 'canceled'),
    ('phase', 'reclaimed');

-- +goose Down
DROP TABLE IF EXISTS scheduler_lease_expiry_counters;
ALTER TABLE worker_leases
    DROP CONSTRAINT IF EXISTS worker_leases_kind_known,
    DROP COLUMN IF EXISTS lease_kind;
