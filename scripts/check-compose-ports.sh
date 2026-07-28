#!/usr/bin/env bash
set -euo pipefail

env_file="${1:-.env.compose}"
if [[ -f "${env_file}" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "${env_file}"
  set +a
elif [[ "${env_file}" != ".env.compose" ]]; then
  echo "environment file not found: ${env_file}" >&2
  exit 2
fi

ports=(
  "server:${PORTAGE_SERVER_PORT:-18080}"
  "worker-gateway:${PORTAGE_WORKER_GATEWAY_PORT:-19444}"
  "dashboard:${PORTAGE_DASHBOARD_PORT:-18081}"
  "grafana:${PORTAGE_GRAFANA_PORT:-23000}"
  "loki:${PORTAGE_LOKI_PORT:-23100}"
  "otel-health:${PORTAGE_OTEL_HEALTH_PORT:-23133}"
  "tempo:${PORTAGE_TEMPO_PORT:-23200}"
  "otlp-grpc:${PORTAGE_OTLP_GRPC_PORT:-24317}"
  "otlp-http:${PORTAGE_OTLP_HTTP_PORT:-24318}"
  "postgres:${PORTAGE_POSTGRES_PORT:-25432}"
  "redis:${PORTAGE_REDIS_PORT:-26379}"
  "prometheus:${PORTAGE_PROMETHEUS_PORT:-29090}"
)

status=0
for entry in "${ports[@]}"; do
  name="${entry%%:*}"
  port="${entry##*:}"
  if lsof -nP -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1; then
    printf '%-14s %5s  IN USE\n' "${name}" "${port}"
    status=1
  else
    printf '%-14s %5s  free\n' "${name}" "${port}"
  fi
done

if (( status != 0 )); then
  echo "Change the conflicting PORTAGE_*_PORT value in .env.compose." >&2
  exit "${status}"
fi
