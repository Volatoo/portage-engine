-- +goose Up
-- MON-1 derives target history from immutable job/attempt/usage ledgers. The
-- view contains no package atoms, request payloads, credentials, or logs.

CREATE INDEX IF NOT EXISTS build_jobs_monitor_terminal_idx
    ON build_jobs (completed_at DESC, project_id)
    WHERE legacy_visible = true
      AND state IN ('completed', 'success', 'failed', 'canceled');

CREATE INDEX IF NOT EXISTS project_attempt_usage_monitor_job_idx
    ON project_attempt_usage (job_id);

CREATE OR REPLACE VIEW monitor_job_outcomes AS
SELECT
    j.id AS job_id,
    j.project_id,
    p.name AS project_name,
    concat_ws(
        E'\x1f',
        j.project_id::text,
        COALESCE(j.request -> 'resolved_context' ->> 'provider',
                 j.request ->> 'cloud_provider', 'unknown'),
        COALESCE(j.request -> 'resolved_context' ->> 'execution_zone',
                 'default'),
        COALESCE(j.request -> 'resolved_context' ->> 'arch',
                 j.request ->> 'arch', 'unknown'),
        COALESCE(j.request -> 'resolved_context' ->> 'build_mode',
                 'unknown'),
        COALESCE(j.request -> 'resolved_context' ->> 'profile_id',
                 j.request ->> 'profile_id', 'compatibility'),
        COALESCE(j.request -> 'resolved_context' ->> 'image_id', 'unknown'),
        COALESCE(j.request -> 'resolved_context' ->> 'image_generation',
                 'unknown'),
        COALESCE(j.request -> 'resolved_context' ->> 'resource_class',
                 j.request ->> 'resource_class', 'default')
    ) AS target_key,
    COALESCE(j.request -> 'resolved_context' ->> 'provider',
             j.request ->> 'cloud_provider', 'unknown') AS provider,
    COALESCE(j.request -> 'resolved_context' ->> 'execution_zone',
             'default') AS execution_zone,
    COALESCE(j.request -> 'resolved_context' ->> 'arch',
             j.request ->> 'arch', 'unknown') AS architecture,
    COALESCE(j.request -> 'resolved_context' ->> 'build_mode',
             'unknown') AS build_mode,
    COALESCE(j.request -> 'resolved_context' ->> 'profile_id',
             j.request ->> 'profile_id', 'compatibility') AS profile_id,
    COALESCE(j.request -> 'resolved_context' ->> 'image_id',
             'unknown') AS image_id,
    COALESCE(j.request -> 'resolved_context' ->> 'image_generation',
             'unknown') AS image_generation,
    COALESCE(j.request -> 'resolved_context' ->> 'resource_class',
             j.request ->> 'resource_class', 'default') AS resource_class,
    CASE
        WHEN j.state IN ('completed', 'success') THEN 'success'
        WHEN j.state = 'failed' THEN 'failure'
        ELSE 'canceled'
    END AS outcome,
    COALESCE(
        (
            SELECT NULLIF(a.failure_class, '')
            FROM build_attempts a
            WHERE a.job_id = j.id AND a.failure_class <> ''
            ORDER BY a.attempt_no DESC
            LIMIT 1
        ),
        NULLIF(j.status_snapshot ->> 'failed_stage', ''),
        CASE
            WHEN j.state = 'failed' THEN 'unclassified'
            WHEN j.state = 'canceled' THEN 'canceled'
            ELSE ''
        END
    ) AS failure_class,
    j.created_at AS queued_at,
    attempts.started_at,
    COALESCE(j.completed_at, attempts.finished_at, j.updated_at)
        AS completed_at,
    GREATEST(
        0,
        floor(extract(epoch FROM (
            COALESCE(attempts.started_at, j.created_at) - j.created_at
        )))
    )::bigint AS queue_seconds,
    GREATEST(
        0,
        floor(extract(epoch FROM (
            COALESCE(j.completed_at, attempts.finished_at, j.updated_at)
            - COALESCE(attempts.started_at, j.created_at)
        )))
    )::bigint AS run_seconds,
    COALESCE(usage.reserved_cloud_cost_microunits, 0)
        AS reserved_cloud_cost_microunits,
    COALESCE(usage.charged_cloud_cost_microunits, 0)
        AS charged_cloud_cost_microunits
FROM build_jobs j
JOIN projects p ON p.id = j.project_id
LEFT JOIN LATERAL (
    SELECT
        min(started_at) FILTER (WHERE started_at IS NOT NULL) AS started_at,
        max(finished_at) FILTER (WHERE finished_at IS NOT NULL) AS finished_at
    FROM build_attempts
    WHERE job_id = j.id
) attempts ON true
LEFT JOIN LATERAL (
    SELECT
        COALESCE(sum(reserved_cloud_cost_microunits), 0)::bigint
            AS reserved_cloud_cost_microunits,
        COALESCE(sum(charged_cloud_cost_microunits), 0)::bigint
            AS charged_cloud_cost_microunits
    FROM project_attempt_usage
    WHERE job_id = j.id
) usage ON true
WHERE j.legacy_visible = true
  AND j.state IN ('completed', 'success', 'failed', 'canceled');

-- +goose Down
DROP VIEW IF EXISTS monitor_job_outcomes;
DROP INDEX IF EXISTS project_attempt_usage_monitor_job_idx;
DROP INDEX IF EXISTS build_jobs_monitor_terminal_idx;
