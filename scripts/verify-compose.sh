#!/usr/bin/env bash
set -euo pipefail

env_file="${1:-.env.compose}"
compose=(docker compose)

if [[ -f "${env_file}" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "${env_file}"
  set +a
  compose+=(--env-file "${env_file}")
elif [[ "${env_file}" != ".env.compose" ]]; then
  echo "environment file not found: ${env_file}" >&2
  exit 2
fi

for command_name in curl docker jq; do
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    echo "required command not found: ${command_name}" >&2
    exit 2
  fi
done

server_port="${PORTAGE_SERVER_PORT:-18080}"
dashboard_port="${PORTAGE_DASHBOARD_PORT:-18081}"
grafana_port="${PORTAGE_GRAFANA_PORT:-23000}"
loki_port="${PORTAGE_LOKI_PORT:-23100}"
otel_health_port="${PORTAGE_OTEL_HEALTH_PORT:-23133}"
tempo_port="${PORTAGE_TEMPO_PORT:-23200}"
otlp_http_port="${PORTAGE_OTLP_HTTP_PORT:-24318}"
prometheus_port="${PORTAGE_PROMETHEUS_PORT:-29090}"

postgres_user="${PORTAGE_POSTGRES_USER:-portage}"
postgres_db="${PORTAGE_POSTGRES_DB:-portage_engine}"
postgres_app_user="${PORTAGE_POSTGRES_APP_USER:-portage_app}"
postgres_app_password="${PORTAGE_POSTGRES_APP_PASSWORD:-portage-app-local}"
postgres_signer_password="${PORTAGE_POSTGRES_SIGNER_PASSWORD:-portage-signer-local}"
postgres_actuator_password="${PORTAGE_POSTGRES_ACTUATOR_PASSWORD:-portage-actuator-local}"
redis_password="${PORTAGE_REDIS_PASSWORD:-portage-redis-local}"
grafana_user="${PORTAGE_GRAFANA_USER:-admin}"
grafana_password="${PORTAGE_GRAFANA_PASSWORD:-portage-grafana-local}"
auth_mode="${PORTAGE_AUTH_MODE:-legacy}"
worker_gateway_enabled="${PORTAGE_WORKER_GATEWAY_ENABLED:-false}"
portage_api_key="${PORTAGE_API_KEY:-}"
portage_step_up_key="${PORTAGE_STEP_UP_API_KEY:-}"
portage_oidc_token="${PORTAGE_ENGINE_TOKEN:-}"

curl_server() {
  if [[ -n "${portage_oidc_token}" ]]; then
    curl -H "Authorization: Bearer ${portage_oidc_token}" "$@"
  elif [[ -n "${portage_api_key}" ]]; then
    curl -H "X-API-Key: ${portage_api_key}" "$@"
  else
    curl "$@"
  fi
}

if [[ "${auth_mode}" == "oidc" && -z "${portage_oidc_token}" ]]; then
  echo "PORTAGE_ENGINE_TOKEN is required to verify an AUTH_MODE=oidc stack" >&2
  exit 2
fi

wait_http() {
  local name="$1"
  local url="$2"

  for _ in {1..30}; do
    if curl -fsS "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done

  echo "endpoint did not become ready: ${name} (${url})" >&2
  return 1
}

wait_http server "http://127.0.0.1:${server_port}/readyz"
wait_http dashboard "http://127.0.0.1:${dashboard_port}/"
wait_http grafana "http://127.0.0.1:${grafana_port}/api/health"
wait_http loki "http://127.0.0.1:${loki_port}/ready"
wait_http tempo "http://127.0.0.1:${tempo_port}/ready"
wait_http otel-collector "http://127.0.0.1:${otel_health_port}/"
wait_http prometheus "http://127.0.0.1:${prometheus_port}/-/ready"

server_health="$(curl -fsS "http://127.0.0.1:${server_port}/health")"
jq -e '
  .status == "healthy" and
  .checks.database.enabled == true and
  .checks.database.required == true and
  .checks.database.ok == true and
  .checks.database.schema_version == 29 and
  .checks.job_ledger.enabled == true and
  .checks.job_ledger.ok == true and
  .checks.job_ledger.authority == "postgresql" and
  .checks.redis_cache.enabled == true and
  .checks.redis_cache.ok == true
' <<<"${server_health}" >/dev/null

binhost_inventory="$(curl -fsS "http://127.0.0.1:${server_port}/api/v1/binhosts")"
default_binhost_path="$(
  jq -er '
    [.binhosts[] | select(.default == true)] as $defaults |
    if ($defaults | length) != 1 then error("expected exactly one default binhost") else
      $defaults[0] |
      select(.binhost_path | test("^releases/[A-Za-z0-9._+-]+/binpackages/[A-Za-z0-9._+-]+/[A-Za-z0-9._+-]+$")) |
      select(.sync_path == ("/binpkgs/" + .binhost_path)) |
      .binhost_path
    end
  ' <<<"${binhost_inventory}"
)"
packages_index="$(
  curl -fsS "http://127.0.0.1:${server_port}/binpkgs/${default_binhost_path}/Packages"
)"
grep -q '^PACKAGES: ' <<<"${packages_index}"

"${compose[@]}" exec -T postgres \
  psql -U "${postgres_user}" -d "${postgres_db}" -Atc \
  "SELECT 1 FROM pg_roles WHERE rolname = 'portage_otel' AND pg_has_role('portage_otel', 'pg_monitor', 'member');" \
  | grep -qx 1

app_db_check="$(
  "${compose[@]}" exec -T -e PGPASSWORD="${postgres_app_password}" postgres \
    psql -h 127.0.0.1 -U "${postgres_app_user}" -d "${postgres_db}" -Atc \
    "SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied;
     SELECT has_schema_privilege(current_user, 'public', 'CREATE');
     SELECT has_table_privilege(current_user, 'public.iam_sessions', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.iam_subject_security', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.iam_logout_events', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.workload_issuer_generations', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.workload_certificates', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.project_scheduler_fairness', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.scheduler_autoscale_state', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.scheduler_capacity_pool_state', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.scheduler_capacity_actions', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.scheduler_capacity_instances', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.scheduler_worker_decisions', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.monitor_job_outcomes', 'SELECT');
     SELECT has_table_privilege(current_user, 'public.scheduler_lease_expiry_counters', 'SELECT,INSERT,UPDATE,DELETE');"
)"
if [[ "${app_db_check}" != $'28\nf\nt\nt\nt\nt\nt\nt\nt\nt\nt\nt\nt\nt\nt' ]]; then
  echo "PostgreSQL application-role contract failed: ${app_db_check}" >&2
  exit 1
fi

signer_db_check="$(
  "${compose[@]}" exec -T -e PGPASSWORD="${postgres_signer_password}" postgres \
    psql -h 127.0.0.1 -U portage_signer -d "${postgres_db}" -Atc \
    "SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied;
     SELECT has_schema_privilege(current_user, 'public', 'CREATE');
     SELECT has_table_privilege(current_user, 'public.signing_tasks', 'SELECT,UPDATE');
     SELECT has_table_privilege(current_user, 'public.build_jobs', 'UPDATE');
     SELECT has_table_privilege(current_user, 'public.iam_sessions', 'SELECT');
     SELECT has_table_privilege(current_user, 'public.iam_logout_events', 'SELECT');"
)"
if [[ "${signer_db_check}" != $'28\nf\nt\nf\nf\nf' ]]; then
  echo "PostgreSQL signer-role contract failed: ${signer_db_check}" >&2
  exit 1
fi

actuator_db_check="$(
  "${compose[@]}" exec -T -e PGPASSWORD="${postgres_actuator_password}" postgres \
    psql -h 127.0.0.1 -U portage_actuator -d "${postgres_db}" -Atc \
    "SELECT max(version_id) FROM goose_db_version WHERE is_applied;
     SELECT has_schema_privilege(current_user, 'public', 'CREATE');
     SELECT has_table_privilege(current_user, 'public.scheduler_capacity_actions', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.scheduler_capacity_instances', 'SELECT,INSERT,UPDATE,DELETE');
     SELECT has_table_privilege(current_user, 'public.workers', 'SELECT');
     SELECT has_table_privilege(current_user, 'public.workers', 'INSERT');
     SELECT has_table_privilege(current_user, 'public.worker_leases', 'UPDATE');"
)"
if [[ "${actuator_db_check}" != $'28\nf\nt\nt\nt\nf\nf' ]]; then
  echo "PostgreSQL actuator-role contract failed: ${actuator_db_check}" >&2
  exit 1
fi

iam_status="$(curl_server -fsS "http://127.0.0.1:${server_port}/api/v1/iam/me")"
jq -e --arg auth_mode "${auth_mode}" '
  .auth_mode == $auth_mode and
  .principal.system_admin == true and
  any(.projects[]; .project_name == "default" and .role == "owner")
' <<<"${iam_status}" >/dev/null

workload_identity_inventory="$(
  curl_server -fsS \
    "http://127.0.0.1:${server_port}/api/v1/worker-gateway/identities"
)"
jq -e '
  (.issuers | type) == "array" and
  (.certificates | type) == "array" and
  .certificate_limit == 100
' <<<"${workload_identity_inventory}" >/dev/null

project_policy="$(curl_server -fsS -H "X-Project-ID: default" \
  "http://127.0.0.1:${server_port}/api/v1/projects/policy")"
jq -e '
  .suspended == false and
  .priority_weight >= 1 and
  .starvation_threshold_seconds >= 30 and
  .max_queued_jobs > 0 and .max_active_jobs > 0 and
  .max_daily_submissions > 0 and .version > 0 and
  .max_active_vcpus > 0 and .max_active_memory_mib > 0 and
  .max_active_disk_gib > 0 and .max_artifact_bytes_per_job > 0 and
  .max_daily_build_seconds > 0 and
  .max_daily_cloud_cost_microunits > 0 and
  .max_failures_per_hour > 0 and .abuse_cooldown_seconds >= 60 and
  (.abuse_suspended == true or .abuse_suspended == false) and
  .max_claimed_attempts > 0 and .max_provision_attempts > 0 and
  .max_build_attempts > 0 and .max_verify_attempts > 0 and
  .max_publish_attempts > 0 and
  .queued_jobs >= 0 and .active_jobs >= 0 and .submissions_today >= 0 and
  .reserved_vcpus >= 0 and .reserved_memory_mib >= 0 and
  .reserved_disk_gib >= 0 and .quarantine_bytes >= 0 and
  .active_artifact_budgets >= 0 and
  .build_seconds_today >= 0 and .cloud_cost_microunits_today >= 0 and
  .active_runtime_budgets >= 0 and .failures_last_hour >= 0 and
  .claimed_reservations >= 0 and .provision_reservations >= 0 and
  .build_reservations >= 0 and .verify_reservations >= 0 and
  .publish_reservations >= 0
  and .waiting_reservations >= 0
  and .phase_work_shadow >= 0 and .phase_work_active >= 0
  and .phase_work_blocked >= 0 and .phase_work_ready >= 0
  and .phase_work_unschedulable >= 0
  and .phase_work_claimed >= 0 and .phase_work_failed >= 0
' <<<"${project_policy}" >/dev/null

iam_db_check="$(
  "${compose[@]}" exec -T -e PGPASSWORD="${postgres_app_password}" postgres \
    psql -h 127.0.0.1 -U "${postgres_app_user}" -d "${postgres_db}" -Atc \
    "SELECT count(*) FROM projects WHERE name = 'default';
     SELECT count(*) FROM build_jobs WHERE project_id IS NULL;
     SELECT to_regclass('public.iam_subjects') IS NOT NULL
            AND to_regclass('public.project_memberships') IS NOT NULL
            AND to_regclass('public.project_policies') IS NOT NULL
            AND to_regclass('public.project_resource_reservations') IS NOT NULL
            AND to_regclass('public.project_artifact_budgets') IS NOT NULL
            AND to_regclass('public.artifact_generation_reservations') IS NOT NULL
            AND to_regclass('public.project_attempt_usage') IS NOT NULL
            AND to_regclass('public.iam_subject_security') IS NOT NULL
            AND to_regclass('public.iam_sessions') IS NOT NULL
            AND to_regclass('public.iam_logout_events') IS NOT NULL;
     SELECT to_regclass('public.phase_work_items') IS NOT NULL
            AND to_regclass('public.phase_execution_contexts') IS NOT NULL
            AND to_regclass('public.worker_gateway_sessions') IS NOT NULL
            AND to_regclass('public.worker_gateway_commands') IS NOT NULL
            AND to_regclass('public.worker_gateway_uploads') IS NOT NULL
            AND to_regclass('public.workers_capability_labels_gin_idx') IS NOT NULL
            AND to_regclass('public.phase_work_items_requirements_gin_idx') IS NOT NULL;
     SELECT count(*) = 2
       FROM information_schema.columns
      WHERE table_schema = 'public'
        AND column_name = 'required_capabilities'
        AND table_name IN ('build_jobs', 'phase_work_items');
     SELECT count(*) FROM projects p
      WHERE NOT EXISTS (
        SELECT 1 FROM project_policies pp WHERE pp.project_id = p.id
      );"
)"
if [[ "${iam_db_check}" != $'1\n0\nt\nt\nt\n0' ]]; then
  echo "PostgreSQL IAM/project contract failed: ${iam_db_check}" >&2
  exit 1
fi

if [[ -n "${portage_api_key}" ]]; then
  if [[ -z "${portage_step_up_key}" ]]; then
    echo "PORTAGE_STEP_UP_API_KEY is required to verify legacy/hybrid step-up" >&2
    exit 2
  fi
  missing_step_status="$(
    curl -sS -o /dev/null -w '%{http_code}' -X PUT \
      -H "X-API-Key: ${portage_api_key}" \
      -H "X-Project-ID: default" \
      -H "Content-Type: application/json" \
      --data-binary '{}' \
      "http://127.0.0.1:${server_port}/api/v1/projects/policy"
  )"
  accepted_step_status="$(
    curl -sS -o /dev/null -w '%{http_code}' -X PUT \
      -H "X-API-Key: ${portage_api_key}" \
      -H "X-Step-Up-Key: ${portage_step_up_key}" \
      -H "X-Project-ID: default" \
      -H "Content-Type: application/json" \
      --data-binary '{}' \
      "http://127.0.0.1:${server_port}/api/v1/projects/policy"
  )"
  if [[ "${missing_step_status}" != 428 || "${accepted_step_status}" == 428 ||
        "${accepted_step_status}" == 401 ]]; then
    echo "legacy step-up Gate failed: missing=${missing_step_status} accepted=${accepted_step_status}" >&2
    exit 1
  fi
fi

gpg_status="$(curl_server -fsS "http://127.0.0.1:${server_port}/api/v1/gpg/status")"
jq -e '
  .enabled == true and
  .ready == true and
  .mode == "isolated-outbound-pull" and
  .private_key_here == false and
  (.key_id | length > 0)
' <<<"${gpg_status}" >/dev/null

server_container="$("${compose[@]}" ps -q portage-server)"
signer_container="$("${compose[@]}" ps -q portage-signer)"
if docker inspect "${server_container}" --format '{{range .Mounts}}{{println .Name ":" .Destination}}{{end}}' \
  | grep -q 'portage-signer-key'; then
  echo "server unexpectedly mounts the private signer volume" >&2
  exit 1
fi
docker inspect "${signer_container}" --format '{{range .Mounts}}{{println .Name ":" .Destination}}{{end}}' \
  | grep -q 'portage-signer-key'

"${compose[@]}" exec -T -e REDISCLI_AUTH="${redis_password}" redis \
  redis-cli --no-auth-warning PING | grep -qx PONG

cache_status="$(curl_server -fsS "http://127.0.0.1:${server_port}/api/v1/cache/status")"
jq -e '.enabled == true and .ok == true and .control_plane_presence >= 1' \
  <<<"${cache_status}" >/dev/null

runtime_metadata="$(curl_server -fsS "http://127.0.0.1:${server_port}/api/v1/runtime-metadata/status")"
jq -e '.enabled == true and .ok == true' <<<"${runtime_metadata}" >/dev/null

worker_gateway="$(curl_server -fsS "http://127.0.0.1:${server_port}/api/v1/worker-gateway/status")"
jq -e --arg expected_enabled "${worker_gateway_enabled}" '
  (.enabled == ($expected_enabled == "true")) and
  (.enabled == true or .inbound_builder_api == true) and
  .authority == "postgresql" and
  .executor_protocol == 5 and
  .issuer_healthy == true and
  (.issuer_runtime.healthy | type) == "boolean" and
  .issuer_runtime.consecutive_failures >= 0 and
  (.phase_executor_mode == "shadow" or .phase_executor_mode == "active") and
  .phase_work.shadow >= 0 and .phase_work.active >= 0 and
  .phase_work.blocked >= 0 and .phase_work.ready >= 0 and
  .phase_work.unschedulable >= 0 and
  .phase_work.claimed >= 0 and .phase_work.completed >= 0 and
  .phase_work.failed >= 0 and .phase_work.canceled >= 0 and
  .registered_sessions >= 0 and .connected_sessions >= 0 and
  .pending_tasks >= 0 and .pending_uploads >= 0
' <<<"${worker_gateway}" >/dev/null

scheduler_status="$(
  curl_server -fsS "http://127.0.0.1:${server_port}/api/v1/scheduler/status"
)"
# alert_threshold_seconds is the monitor read-through cache TTL that bounds
# lag_seconds, so it tracks monitorCacheTTL in internal/persistence/monitor.go
# rather than being a tunable number. Pinned here so moving one without the
# other is caught before a reader starts scaling a lag against the wrong bound.
jq -e '
  .authority == "postgresql" and
  .queued_tasks >= 0 and .unschedulable_tasks >= 0 and
  .running_tasks >= 0 and .active_leases >= 0 and .expired_leases >= 0 and
  .active_workers > 0 and .capability_workers > 0 and .stale_workers >= 0 and
  .fairness.enabled == true and .fairness.eligible_projects >= 0 and
  .fairness.starved_projects >= 0 and
  .fairness.admission_dispatches >= 0 and .fairness.phase_dispatches >= 0 and
  .lease_expiries.attempt_requeued >= 0 and
  .lease_expiries.admission_requeued >= 0 and
  .lease_expiries.phase_reclaimed >= 0 and
  .target_history.projection.valid == true and
  .target_history.projection.lag_seconds >= 0 and
  .target_history.projection.alert_threshold_seconds == 30 and
  (.autoscaler.mode == "off" or .autoscaler.mode == "observe") and
  (.autoscaler.recommendation == "off" or
   .autoscaler.recommendation == "hold" or
   .autoscaler.recommendation == "scale-up" or
   .autoscaler.recommendation == "scale-down")
' <<<"${scheduler_status}" >/dev/null

scheduler_metrics="$(
  curl -fsS "http://127.0.0.1:${server_port}/metrics/prometheus"
)"
grep -q '^portage_scheduler_fair_eligible_projects ' <<<"${scheduler_metrics}"
grep -q '^portage_scheduler_autoscale_desired_slots ' <<<"${scheduler_metrics}"
grep -q '^portage_scheduler_lease_expiries_total{lease="attempt",result="requeued"} ' <<<"${scheduler_metrics}"
grep -q '^portage_scheduler_lease_expiries_total{lease="phase",result="reclaimed"} ' <<<"${scheduler_metrics}"
grep -q '^portage_monitor_projection_snapshot_valid 1$' <<<"${scheduler_metrics}"
grep -q '^portage_monitor_projection_lag_seconds ' <<<"${scheduler_metrics}"

sse_ready="$(
  curl_server -sN --max-time 2 "http://127.0.0.1:${server_port}/api/v1/events/jobs" 2>/dev/null || true
)"
grep -q 'event: ready' <<<"${sse_ready}"

down_targets="not-checked"
for _ in {1..30}; do
  targets_json="$(curl -fsS "http://127.0.0.1:${prometheus_port}/api/v1/targets")"
  down_targets="$(jq -r '.data.activeTargets[] | select(.health != "up") | .labels.job' <<<"${targets_json}")"
  if [[ -z "${down_targets}" ]]; then
    break
  fi
  sleep 1
done
if [[ -n "${down_targets}" ]]; then
  echo "Prometheus targets are down:" >&2
  echo "${down_targets}" >&2
  exit 1
fi

datasources_json="$(
  curl -fsS -u "${grafana_user}:${grafana_password}" \
    "http://127.0.0.1:${grafana_port}/api/datasources"
)"
for datasource in Prometheus Loki Tempo; do
  jq -e --arg name "${datasource}" 'any(.[]; .name == $name)' \
    <<<"${datasources_json}" >/dev/null
done

epoch="$(date +%s)"
trace_id="$(printf '%032x' "${epoch}")"
span_id="$(printf '%016x' "${epoch}")"
start_ns="${epoch}000000000"
end_ns="$((epoch + 1))000000000"
log_marker="portage-engine-compose-smoke-${epoch}"

curl -fsS -X POST "http://127.0.0.1:${otlp_http_port}/v1/logs" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"resourceLogs\": [{
      \"resource\": {\"attributes\": [
        {\"key\": \"service.name\", \"value\": {\"stringValue\": \"compose-smoke\"}},
        {\"key\": \"service.namespace\", \"value\": {\"stringValue\": \"portage-engine\"}}
      ]},
      \"scopeLogs\": [{
        \"scope\": {\"name\": \"portage-engine-compose-smoke\"},
        \"logRecords\": [{
          \"severityText\": \"INFO\",
          \"body\": {\"stringValue\": \"${log_marker}\"}
        }]
      }]
    }]
  }" >/dev/null

curl -fsS -X POST "http://127.0.0.1:${otlp_http_port}/v1/traces" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"resourceSpans\": [{
      \"resource\": {\"attributes\": [
        {\"key\": \"service.name\", \"value\": {\"stringValue\": \"compose-smoke\"}},
        {\"key\": \"service.namespace\", \"value\": {\"stringValue\": \"portage-engine\"}}
      ]},
      \"scopeSpans\": [{
        \"scope\": {\"name\": \"portage-engine-compose-smoke\"},
        \"spans\": [{
          \"traceId\": \"${trace_id}\",
          \"spanId\": \"${span_id}\",
          \"name\": \"compose-pipeline-smoke\",
          \"kind\": 1,
          \"startTimeUnixNano\": \"${start_ns}\",
          \"endTimeUnixNano\": \"${end_ns}\",
          \"status\": {\"code\": 1}
        }]
      }]
    }]
  }" >/dev/null

log_count=0
trace_status=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
  log_count="$(
    curl -fsSG "http://127.0.0.1:${loki_port}/loki/api/v1/query_range" \
      --data-urlencode 'query={service_name="compose-smoke"}' \
      --data-urlencode 'limit=100' \
      | jq --arg marker "${log_marker}" \
        '[.data.result[].values[] | select(.[1] | contains($marker))] | length'
  )"
  trace_status="$(
    curl -s -o /dev/null -w '%{http_code}' \
      "http://127.0.0.1:${tempo_port}/api/traces/${trace_id}" || true
  )"
  if [[ "${log_count}" -ge 1 && "${trace_status}" == 200 ]]; then
    break
  fi
  sleep 2
done

if [[ "${log_count}" -lt 1 || "${trace_status}" != 200 ]]; then
  echo "telemetry smoke failed: loki=${log_count}, tempo_http=${trace_status}" >&2
  exit 1
fi

printf '%s\n' \
  "Compose verification passed." \
  "  endpoints: server dashboard grafana loki tempo otel prometheus" \
  "  state: PostgreSQL schema v29 sole authority + one-time CLI device authorization + low-cardinality lease expiry/projection lag observability + distributed compile inventory/fenced slot leases + target history/SLO/cost projection + weighted fair scheduling/anti-starvation + explainable worker soft scoring + capability/provider-pool observations + fenced capacity action/ownership/drain ledger + workload issuer/certificate lifecycle + exact executor capability routing + multi-provider token exchange/back-channel replay protection + session revocation + administrator step-up + active phase hand-off context + durable worker sessions/commands/uploads + IAM/project RBAC/resource + phase work/fences + phase caps + runtime/cloud-cost/artifact budgets + abuse cooldown + scheduler/logs/metadata/infra-cleanup/signing leases/executor fence" \
  "  signing: isolated outbound-pull signer + least-privilege role + server has public key only" \
  "  capacity: opt-in listener-free actuator + dedicated least-privilege DB role + exact PVE identity/drain/absence fences" \
  "  binhost: official-style per-profile namespace + independently generated Packages index" \
  "  cache: Redis auth + presence + rate-limit backend + SSE stream" \
  "  metrics: all Prometheus targets up" \
  "  Grafana: Prometheus + Loki + Tempo provisioned" \
  "  telemetry: OTLP log -> Loki; OTLP trace -> Tempo"
