\set ON_ERROR_STOP on

-- Seed one complete, non-secret lineage in an explicitly isolated recovery
-- database. Every identifier is derived from the caller's audit-safe marker,
-- so retries are idempotent and parallel drills do not collide.
SELECT set_config('portage.drill.fixture_key', :'fixture_key', false) AS configured \gset
SELECT set_config('portage.drill.fixture_owner', :'owner', false) AS configured \gset

SELECT
    md5(current_setting('portage.drill.fixture_key') || ':project')::uuid AS fixture_project_id,
    md5(current_setting('portage.drill.fixture_key') || ':target')::uuid AS fixture_target_id,
    md5(current_setting('portage.drill.fixture_key') || ':subject')::uuid AS fixture_subject_id,
    md5(current_setting('portage.drill.fixture_key') || ':job')::uuid AS fixture_job_id,
    md5(current_setting('portage.drill.fixture_key') || ':attempt')::uuid AS fixture_attempt_id,
    md5(current_setting('portage.drill.fixture_key') || ':signing')::uuid AS fixture_signing_id,
    md5(current_setting('portage.drill.fixture_key') || ':action')::uuid AS fixture_action_id,
    md5(current_setting('portage.drill.fixture_key') || ':instance')::uuid AS fixture_instance_id,
    md5(current_setting('portage.drill.fixture_key') || ':owner-token')::uuid AS fixture_owner_token,
    'recovery-drill-' || left(md5(current_setting('portage.drill.fixture_key')), 12) AS fixture_name,
    'recovery-worker-' || left(md5(current_setting('portage.drill.fixture_key') || ':worker'), 16) AS fixture_worker_id,
    'recovery-pool-' || left(md5(current_setting('portage.drill.fixture_key') || ':pool'), 16) AS fixture_pool_id,
    md5(current_setting('portage.drill.fixture_key') || ':issuer') ||
        md5(current_setting('portage.drill.fixture_key') || ':issuer:2') AS fixture_issuer_fingerprint,
    md5(current_setting('portage.drill.fixture_key') || ':certificate') ||
        md5(current_setting('portage.drill.fixture_key') || ':certificate:2') AS fixture_certificate_fingerprint
\gset

BEGIN;

INSERT INTO projects (id, name, description)
VALUES (
    :'fixture_project_id', :'fixture_name',
    'Non-secret PostgreSQL physical recovery fixture'
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO iam_subjects (
    id, issuer, subject, preferred_username, display_name, email
)
VALUES (
    :'fixture_subject_id', 'urn:portage-engine:recovery-drill',
    current_setting('portage.drill.fixture_key'),
    current_setting('portage.drill.fixture_owner'),
    'Recovery Drill Operator', ''
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO project_memberships (
    project_id, subject_id, role, granted_by_subject_id
)
VALUES (
    :'fixture_project_id', :'fixture_subject_id', 'owner', :'fixture_subject_id'
)
ON CONFLICT (project_id, subject_id) DO NOTHING;

INSERT INTO targets (
    id, project_id, name, architecture, profile, service_manager,
    repository_revision, image_revision, configuration
)
VALUES (
    :'fixture_target_id', :'fixture_project_id', :'fixture_name', 'amd64',
    'default/linux/amd64/17.1', 'systemd', 'recovery-fixture-revision',
    'recovery-fixture-image', '{"classification":"non-secret-drill-fixture"}'::jsonb
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO build_jobs (
    id, project_id, target_id, idempotency_key, package_atom, state,
    priority, request, request_digest, requested_by_subject_id,
    completed_at
)
VALUES (
    :'fixture_job_id', :'fixture_project_id', :'fixture_target_id',
    current_setting('portage.drill.fixture_key'), 'app-misc/hello', 'success',
    0,
    jsonb_build_object(
        'classification', 'non-secret-drill-fixture',
        'resolved_context', jsonb_build_object(
            'provider', 'recovery-drill',
            'execution_zone', 'isolated',
            'arch', 'amd64',
            'build_mode', 'package',
            'profile_id', 'default/linux/amd64/17.1',
            'image_id', 'recovery-fixture',
            'image_generation', '1',
            'resource_class', 'small'
        )
    ),
    md5(current_setting('portage.drill.fixture_key') || ':request') ||
        md5(current_setting('portage.drill.fixture_key') || ':request:2'),
    :'fixture_subject_id', clock_timestamp() - interval '1 minute'
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO build_attempts (
    id, job_id, attempt_no, state, fence_token, started_at, finished_at
)
VALUES (
    :'fixture_attempt_id', :'fixture_job_id', 1, 'completed', 1,
    clock_timestamp() - interval '2 minutes',
    clock_timestamp() - interval '1 minute'
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO signing_tasks (
    id, job_id, attempt_id, attempt_fence, state, source_token,
    architecture, input_manifest, output_manifest, signing_key_id,
    completed_at
)
VALUES (
    :'fixture_signing_id', :'fixture_job_id', :'fixture_attempt_id', 1,
    'completed', md5(current_setting('portage.drill.fixture_key') || ':source'),
    'amd64', '{"classification":"non-secret-drill-fixture"}'::jsonb,
    '{"signed":true,"classification":"non-secret-drill-fixture"}'::jsonb,
    'recovery-drill-key', clock_timestamp() - interval '1 minute'
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO worker_gateway_sessions (
    worker_id, project_id, job_id, attempt_id, attempt_fence, state,
    connected_at, last_seen_at
)
VALUES (
    :'fixture_worker_id', :'fixture_project_id', :'fixture_job_id',
    :'fixture_attempt_id', 1, 'active', clock_timestamp(), clock_timestamp()
)
ON CONFLICT (worker_id) DO NOTHING;

INSERT INTO workload_issuer_generations (
    fingerprint, issuer_id, provider, subject, serial_hex,
    not_before, not_after, state
)
VALUES (
    :'fixture_issuer_fingerprint', :'fixture_name' || '-issuer', 'vault',
    'urn:portage-engine:recovery-drill',
    md5(current_setting('portage.drill.fixture_key') || ':issuer-serial'),
    clock_timestamp() - interval '1 hour',
    clock_timestamp() + interval '24 hours', 'active'
)
ON CONFLICT (fingerprint) DO NOTHING;

INSERT INTO workload_certificates (
    fingerprint, serial_hex, issuer_fingerprint, worker_id, job_id,
    attempt_id, attempt_fence, not_before, not_after, state,
    first_seen_at, last_seen_at
)
VALUES (
    :'fixture_certificate_fingerprint',
    md5(current_setting('portage.drill.fixture_key') || ':certificate-serial'),
    :'fixture_issuer_fingerprint', :'fixture_worker_id', :'fixture_job_id',
    :'fixture_attempt_id', 1, clock_timestamp() - interval '5 minutes',
    clock_timestamp() + interval '1 hour', 'active',
    clock_timestamp(), clock_timestamp()
)
ON CONFLICT (fingerprint) DO NOTHING;

UPDATE worker_gateway_sessions
SET certificate_fingerprint = :'fixture_certificate_fingerprint',
    updated_at = clock_timestamp()
WHERE worker_id = :'fixture_worker_id'
  AND certificate_fingerprint IS NULL;

INSERT INTO scheduler_capacity_pool_state (
    pool_id, provider, execution_zone, architecture, build_mode,
    profile_id, image_id, image_generation, selector, mode,
    active_slots, busy_slots, backlog, unschedulable_backlog,
    desired_slots, recommendation, reason
)
VALUES (
    :'fixture_pool_id', 'recovery-drill', 'isolated', 'amd64', 'package',
    'default/linux/amd64/17.1', 'recovery-fixture', '1',
    '["provider:recovery-drill","arch:amd64"]'::jsonb, 'observe',
    1, 0, 0, 0, 1, 'hold', 'non-secret recovery fixture'
)
ON CONFLICT (pool_id) DO NOTHING;

INSERT INTO scheduler_capacity_actions (
    id, pool_id, action_kind, state, requested_slots, observed_slots,
    delta_slots, reason, completed_at
)
VALUES (
    :'fixture_action_id', :'fixture_pool_id', 'scale-up', 'completed',
    1, 0, 1, 'non-secret recovery fixture', clock_timestamp()
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO scheduler_capacity_instances (
    id, pool_id, create_action_id, provider, provider_instance_id,
    owner_token, generation, state, attributes, heartbeat_observed_at
)
VALUES (
    :'fixture_instance_id', :'fixture_pool_id', :'fixture_action_id',
    'recovery-drill', :'fixture_name', :'fixture_owner_token', 1, 'active',
    '{"classification":"non-secret-drill-fixture"}'::jsonb,
    clock_timestamp()
)
ON CONFLICT (id) DO NOTHING;

COMMIT;

SELECT json_build_object(
    'fixture_key', current_setting('portage.drill.fixture_key'),
    'classification', 'non-secret-drill-fixture',
    'row_counts', json_build_object(
        'jobs', (SELECT count(*) FROM build_jobs WHERE id = :'fixture_job_id'),
        'attempts', (SELECT count(*) FROM build_attempts WHERE id = :'fixture_attempt_id'),
        'signing_tasks', (SELECT count(*) FROM signing_tasks WHERE id = :'fixture_signing_id'),
        'workload_issuers', (SELECT count(*) FROM workload_issuer_generations WHERE fingerprint = :'fixture_issuer_fingerprint'),
        'workload_certificates', (SELECT count(*) FROM workload_certificates WHERE fingerprint = :'fixture_certificate_fingerprint'),
        'capacity_pools', (SELECT count(*) FROM scheduler_capacity_pool_state WHERE pool_id = :'fixture_pool_id'),
        'capacity_actions', (SELECT count(*) FROM scheduler_capacity_actions WHERE id = :'fixture_action_id'),
        'capacity_instances', (SELECT count(*) FROM scheduler_capacity_instances WHERE id = :'fixture_instance_id'),
        'targets', (SELECT count(*) FROM targets WHERE id = :'fixture_target_id'),
        'monitor_outcomes', (SELECT count(*) FROM monitor_job_outcomes WHERE job_id = :'fixture_job_id'),
        'project_memberships', (SELECT count(*) FROM project_memberships WHERE project_id = :'fixture_project_id' AND subject_id = :'fixture_subject_id')
    )
)::text;
