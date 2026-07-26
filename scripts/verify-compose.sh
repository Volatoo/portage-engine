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
redis_password="${PORTAGE_REDIS_PASSWORD:-portage-redis-local}"
grafana_user="${PORTAGE_GRAFANA_USER:-admin}"
grafana_password="${PORTAGE_GRAFANA_PASSWORD:-portage-grafana-local}"

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
  .checks.database.schema_version == 7 and
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
     SELECT has_schema_privilege(current_user, 'public', 'CREATE');"
)"
if [[ "${app_db_check}" != $'7\nf' ]]; then
  echo "PostgreSQL application-role contract failed: ${app_db_check}" >&2
  exit 1
fi

signer_db_check="$(
  "${compose[@]}" exec -T -e PGPASSWORD="${postgres_signer_password}" postgres \
    psql -h 127.0.0.1 -U portage_signer -d "${postgres_db}" -Atc \
    "SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied;
     SELECT has_schema_privilege(current_user, 'public', 'CREATE');
     SELECT has_table_privilege(current_user, 'public.signing_tasks', 'SELECT,UPDATE');
     SELECT has_table_privilege(current_user, 'public.build_jobs', 'UPDATE');"
)"
if [[ "${signer_db_check}" != $'7\nf\nt\nf' ]]; then
  echo "PostgreSQL signer-role contract failed: ${signer_db_check}" >&2
  exit 1
fi

gpg_status="$(curl -fsS "http://127.0.0.1:${server_port}/api/v1/gpg/status")"
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

cache_status="$(curl -fsS "http://127.0.0.1:${server_port}/api/v1/cache/status")"
jq -e '.enabled == true and .ok == true and .control_plane_presence >= 1' \
  <<<"${cache_status}" >/dev/null

runtime_metadata="$(curl -fsS "http://127.0.0.1:${server_port}/api/v1/runtime-metadata/status")"
jq -e '.enabled == true and .ok == true' <<<"${runtime_metadata}" >/dev/null

sse_ready="$(
  curl -sN --max-time 2 "http://127.0.0.1:${server_port}/api/v1/events/jobs" 2>/dev/null || true
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
  "  state: PostgreSQL schema v7 sole authority + durable scheduler/logs/metadata/infra-cleanup/signing leases" \
  "  signing: isolated outbound-pull signer + least-privilege role + server has public key only" \
  "  binhost: official-style per-profile namespace + independently generated Packages index" \
  "  cache: Redis auth + presence + rate-limit backend + SSE stream" \
  "  metrics: all Prometheus targets up" \
  "  Grafana: Prometheus + Loki + Tempo provisioned" \
  "  telemetry: OTLP log -> Loki; OTLP trace -> Tempo"
