\set ON_ERROR_STOP on

-- Inputs are supplied with psql -v. They never contain credentials.
SELECT set_config('portage.drill.expected_schema', :'expected_schema', false) AS configured \gset
SELECT set_config('portage.drill.expected_marker', :'expected_marker', false) AS configured \gset
SELECT set_config('portage.drill.absent_marker', :'absent_marker', false) AS configured \gset
SELECT set_config('portage.drill.app_role', :'app_role', false) AS configured \gset
SELECT set_config('portage.drill.signer_role', :'signer_role', false) AS configured \gset
SELECT set_config('portage.drill.actuator_role', :'actuator_role', false) AS configured \gset

DO $validation$
DECLARE
    schema_version bigint;
    expected_schema bigint := current_setting('portage.drill.expected_schema')::bigint;
    missing_relations text[];
    empty_relations text[] := ARRAY[]::text[];
    membership_constraint text;
    relation_name text;
    relation_rows bigint;
    role_name text;
BEGIN
    SELECT COALESCE(max(version_id) FILTER (WHERE is_applied), 0)
    INTO schema_version
    FROM goose_db_version;
    IF expected_schema <= 0 OR schema_version <> expected_schema THEN
        RAISE EXCEPTION 'restore requires authoritative schema v%, found v%', expected_schema, schema_version;
    END IF;

    SELECT array_agg(required.name ORDER BY required.name)
    INTO missing_relations
    FROM unnest(ARRAY[
        'public.projects',
        'public.targets',
        'public.build_jobs',
        'public.build_attempts',
        'public.signing_tasks',
        'public.workload_issuer_generations',
        'public.workload_certificates',
        'public.scheduler_capacity_pool_state',
        'public.scheduler_capacity_actions',
        'public.scheduler_capacity_instances',
        'public.project_memberships',
        'public.audit_events',
        'public.monitor_job_outcomes'
    ]) AS required(name)
    WHERE to_regclass(required.name) IS NULL;
    IF missing_relations IS NOT NULL THEN
        RAISE EXCEPTION 'restore is missing required current-schema relations: %', missing_relations;
    END IF;

    FOREACH relation_name IN ARRAY ARRAY[
        'build_jobs',
        'build_attempts',
        'signing_tasks',
        'workload_issuer_generations',
        'workload_certificates',
        'scheduler_capacity_pool_state',
        'scheduler_capacity_actions',
        'scheduler_capacity_instances',
        'targets',
        'monitor_job_outcomes',
        'project_memberships'
    ] LOOP
        EXECUTE format('SELECT count(*) FROM %I', relation_name)
        INTO relation_rows;
        IF relation_rows < 1 THEN
            empty_relations := array_append(empty_relations, relation_name);
        END IF;
    END LOOP;
    IF cardinality(empty_relations) > 0 THEN
        RAISE EXCEPTION 'restore has no recovery lineage in required relations: %', empty_relations;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM build_attempts attempt
        LEFT JOIN build_jobs job ON job.id = attempt.job_id
        WHERE job.id IS NULL
    ) THEN
        RAISE EXCEPTION 'restore contains an attempt without its job';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM signing_tasks task
        LEFT JOIN build_jobs job ON job.id = task.job_id
        LEFT JOIN build_attempts attempt ON attempt.id = task.attempt_id
        WHERE job.id IS NULL OR attempt.id IS NULL
           OR attempt.job_id <> task.job_id
           OR attempt.fence_token <> task.attempt_fence
    ) THEN
        RAISE EXCEPTION 'restore contains a signing task with broken job/attempt lineage';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM workload_certificates certificate
        LEFT JOIN workload_issuer_generations issuer
            ON issuer.fingerprint = certificate.issuer_fingerprint
        LEFT JOIN build_attempts attempt ON attempt.id = certificate.attempt_id
        LEFT JOIN worker_gateway_sessions session
            ON session.worker_id = certificate.worker_id
        WHERE issuer.fingerprint IS NULL
           OR attempt.id IS NULL
           OR attempt.job_id <> certificate.job_id
           OR attempt.fence_token <> certificate.attempt_fence
           OR session.worker_id IS NULL
    ) THEN
        RAISE EXCEPTION 'restore contains a workload leaf without its issuer generation';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM scheduler_capacity_instances instance
        LEFT JOIN scheduler_capacity_pool_state pool ON pool.pool_id = instance.pool_id
        LEFT JOIN scheduler_capacity_actions action ON action.id = instance.create_action_id
        LEFT JOIN scheduler_capacity_actions delete_action
            ON delete_action.id = instance.delete_action_id
        WHERE pool.pool_id IS NULL OR action.id IS NULL
           OR action.pool_id <> instance.pool_id
           OR (
               instance.delete_action_id IS NOT NULL
               AND (
                   delete_action.id IS NULL
                   OR delete_action.pool_id <> instance.pool_id
               )
           )
    ) THEN
        RAISE EXCEPTION 'restore contains a capacity instance with broken pool/action lineage';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM build_jobs job
        LEFT JOIN targets target ON target.id = job.target_id
        WHERE job.target_id IS NOT NULL AND target.id IS NULL
    ) THEN
        RAISE EXCEPTION 'restore contains a job with a missing target';
    END IF;

    SELECT pg_get_constraintdef(oid)
    INTO membership_constraint
    FROM pg_constraint
    WHERE conrelid = 'public.project_memberships'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%viewer%developer%maintainer%owner%';
    IF membership_constraint IS NULL THEN
        RAISE EXCEPTION 'project role vocabulary is not viewer/developer/maintainer/owner';
    END IF;

    FOREACH role_name IN ARRAY ARRAY[
        current_setting('portage.drill.app_role'),
        current_setting('portage.drill.signer_role'),
        current_setting('portage.drill.actuator_role')
    ] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_roles
            WHERE rolname = role_name
              AND rolcanlogin
              AND NOT rolsuper
              AND NOT rolcreaterole
              AND NOT rolcreatedb
              AND NOT rolreplication
              AND NOT rolbypassrls
        ) THEN
            RAISE EXCEPTION 'required least-privilege login role % is missing or over-privileged', role_name;
        END IF;
    END LOOP;

    IF NOT has_table_privilege(
        current_setting('portage.drill.signer_role'), 'public.signing_tasks', 'SELECT'
    ) OR NOT has_table_privilege(
        current_setting('portage.drill.signer_role'), 'public.signing_tasks', 'UPDATE'
    ) OR has_table_privilege(
        current_setting('portage.drill.signer_role'), 'public.signing_tasks', 'INSERT'
    ) OR has_table_privilege(
        current_setting('portage.drill.signer_role'), 'public.signing_tasks', 'DELETE'
    ) OR has_table_privilege(
        current_setting('portage.drill.signer_role'), 'public.signing_tasks', 'TRUNCATE'
    ) OR has_table_privilege(
        current_setting('portage.drill.signer_role'), 'public.signing_tasks', 'REFERENCES'
    ) OR has_table_privilege(
        current_setting('portage.drill.signer_role'), 'public.signing_tasks', 'TRIGGER'
    ) THEN
        RAISE EXCEPTION 'signer role grants do not match the isolated queue contract';
    END IF;
    IF NOT has_table_privilege(
        current_setting('portage.drill.actuator_role'), 'public.scheduler_capacity_actions', 'SELECT'
    ) OR NOT has_table_privilege(
        current_setting('portage.drill.actuator_role'), 'public.scheduler_capacity_actions', 'INSERT'
    ) OR NOT has_table_privilege(
        current_setting('portage.drill.actuator_role'), 'public.scheduler_capacity_actions', 'UPDATE'
    ) OR NOT has_table_privilege(
        current_setting('portage.drill.actuator_role'), 'public.scheduler_capacity_actions', 'DELETE'
    ) OR has_table_privilege(
        current_setting('portage.drill.actuator_role'), 'public.signing_tasks', 'INSERT'
    ) OR has_table_privilege(
        current_setting('portage.drill.actuator_role'), 'public.signing_tasks', 'UPDATE'
    ) OR has_table_privilege(
        current_setting('portage.drill.actuator_role'), 'public.signing_tasks', 'DELETE'
    ) THEN
        RAISE EXCEPTION 'capacity actuator role grants cross trust domains';
    END IF;
    IF NOT has_table_privilege(
        current_setting('portage.drill.app_role'), 'public.build_jobs', 'SELECT'
    ) OR NOT has_table_privilege(
        current_setting('portage.drill.app_role'), 'public.build_jobs', 'INSERT'
    ) OR NOT has_table_privilege(
        current_setting('portage.drill.app_role'), 'public.build_jobs', 'UPDATE'
    ) OR NOT has_table_privilege(
        current_setting('portage.drill.app_role'), 'public.build_jobs', 'DELETE'
    ) THEN
        RAISE EXCEPTION 'application role cannot operate the restored job ledger';
    END IF;
    IF current_setting('portage.drill.expected_marker') <> '' AND NOT EXISTS (
        SELECT 1 FROM audit_events
        WHERE resource_id = current_setting('portage.drill.expected_marker')
    ) THEN
        RAISE EXCEPTION 'expected PITR audit marker is absent';
    END IF;
    IF current_setting('portage.drill.absent_marker') <> '' AND EXISTS (
        SELECT 1 FROM audit_events
        WHERE resource_id = current_setting('portage.drill.absent_marker')
    ) THEN
        RAISE EXCEPTION 'post-target PITR audit marker is unexpectedly present';
    END IF;
END
$validation$;

SELECT json_build_object(
    'expected_schema_version', current_setting('portage.drill.expected_schema')::bigint,
    'schema_version', (
        SELECT max(version_id) FROM goose_db_version WHERE is_applied
    ),
    'row_counts', json_build_object(
        'jobs', (SELECT count(*) FROM build_jobs),
        'attempts', (SELECT count(*) FROM build_attempts),
        'signing_tasks', (SELECT count(*) FROM signing_tasks),
        'workload_issuers', (SELECT count(*) FROM workload_issuer_generations),
        'workload_certificates', (SELECT count(*) FROM workload_certificates),
        'capacity_pools', (SELECT count(*) FROM scheduler_capacity_pool_state),
        'capacity_actions', (SELECT count(*) FROM scheduler_capacity_actions),
        'capacity_instances', (SELECT count(*) FROM scheduler_capacity_instances),
        'targets', (SELECT count(*) FROM targets),
        'monitor_outcomes', (SELECT count(*) FROM monitor_job_outcomes),
        'project_memberships', (SELECT count(*) FROM project_memberships)
    ),
    'roles', json_build_array(
        current_setting('portage.drill.app_role'),
        current_setting('portage.drill.signer_role'),
        current_setting('portage.drill.actuator_role')
    ),
    'expected_marker_present', CASE
        WHEN current_setting('portage.drill.expected_marker') = '' THEN NULL
        ELSE EXISTS (
            SELECT 1 FROM audit_events
            WHERE resource_id = current_setting('portage.drill.expected_marker')
        )
    END,
    'post_target_marker_absent', CASE
        WHEN current_setting('portage.drill.absent_marker') = '' THEN NULL
        ELSE NOT EXISTS (
            SELECT 1 FROM audit_events
            WHERE resource_id = current_setting('portage.drill.absent_marker')
        )
    END,
    'validated_at', clock_timestamp()
)::text AS recovery_validation;
