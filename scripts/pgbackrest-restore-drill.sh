#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
target_time="${1:-}"
expected_marker="${PORTAGE_PITR_ASSERT_RESOURCE_ID:-}"
absent_marker="${PORTAGE_PITR_ASSERT_ABSENT_RESOURCE_ID:-}"
source_repo="${PORTAGE_PGBACKREST_REPO:-}"
owner="${PORTAGE_DRILL_OWNER:-}"
isolated_target="${PORTAGE_DRILL_TARGET:-}"
last_durable_utc="${PORTAGE_PITR_LAST_DURABLE_UTC:-}"
max_rpo_seconds="${PORTAGE_PITR_MAX_RPO_SECONDS:-}"
max_rto_seconds="${PORTAGE_PITR_MAX_RTO_SECONDS:-}"
app_role="${PORTAGE_POSTGRES_APP_USER:-portage_app}"
signer_role="${PORTAGE_POSTGRES_SIGNER_USER:-portage_signer}"
actuator_role="${PORTAGE_POSTGRES_ACTUATOR_USER:-portage_actuator}"

fail_input() {
  echo "$1" >&2
  exit 2
}

[[ "${PORTAGE_DRILL_ISOLATED:-}" == "true" ]] ||
  fail_input "PORTAGE_DRILL_ISOLATED must be exactly true"
[[ "${PORTAGE_DRILL_CONFIRM:-}" == "RUN_PUBLIC_BETA_RECOVERY_DRILL" ]] ||
  fail_input "PORTAGE_DRILL_CONFIRM must be RUN_PUBLIC_BETA_RECOVERY_DRILL"
[[ "${PORTAGE_DRILL_DESTRUCTIVE_CONFIRM:-}" == "DESTROY_ISOLATED_DRILL_TARGET_ONLY" ]] ||
  fail_input "PORTAGE_DRILL_DESTRUCTIVE_CONFIRM must be DESTROY_ISOLATED_DRILL_TARGET_ONLY"
[[ "${isolated_target}" =~ ^[A-Za-z0-9._:/-]*(drill|recovery|sandbox|test)[A-Za-z0-9._:/-]*$ ]] ||
  fail_input "PORTAGE_DRILL_TARGET must explicitly name an isolated drill target"
isolated_target_lower="$(printf '%s' "${isolated_target}" | tr '[:upper:]' '[:lower:]')"
[[ ! "${isolated_target_lower}" =~ (^|[._:/-])(prod|production|live|main|primary)([._:/-]|$) ]] ||
  fail_input "production-like restore targets are refused"
[[ -n "${owner}" ]] || fail_input "PORTAGE_DRILL_OWNER is required"
[[ -n "${source_repo}" && -d "${source_repo}" ]] ||
  fail_input "PORTAGE_PGBACKREST_REPO must explicitly name a readable repository"
[[ -n "${target_time}" ]] || fail_input "an explicit PITR target time is required"
[[ -n "${expected_marker}" ]] ||
  fail_input "PORTAGE_PITR_ASSERT_RESOURCE_ID is required to prove the durable marker"
[[ "${expected_marker}" =~ ^[A-Za-z0-9._:-]+$ ]] ||
  fail_input "PORTAGE_PITR_ASSERT_RESOURCE_ID contains unsupported characters"
[[ "${absent_marker}" =~ ^[A-Za-z0-9._:-]+$ ]] ||
  fail_input "PORTAGE_PITR_ASSERT_ABSENT_RESOURCE_ID is required and contains unsupported characters"
[[ "${expected_marker}" != "${absent_marker}" ]] ||
  fail_input "PITR present and absent markers must differ"
[[ -n "${last_durable_utc}" ]] || fail_input "PORTAGE_PITR_LAST_DURABLE_UTC is required"
[[ "${max_rpo_seconds}" =~ ^[0-9]+$ ]] || fail_input "PORTAGE_PITR_MAX_RPO_SECONDS must be an integer"
[[ "${max_rto_seconds}" =~ ^[0-9]+$ ]] || fail_input "PORTAGE_PITR_MAX_RTO_SECONDS must be an integer"

expected_schema="$("${repo_root}/scripts/recovery/current-schema-version.sh")"

started_epoch_ms="$(python3 -c 'import time; print(time.time_ns() // 1000000)')"
drill_root="$(mktemp -d "${TMPDIR:-/tmp}/portage-pitr.XXXXXX")"
validation_container=""
cleanup() {
  if [[ -n "${validation_container}" ]]; then
    docker rm -f "${validation_container}" >/dev/null 2>&1 || true
  fi
  rm -rf "${drill_root}"
}
trap cleanup EXIT

filesystem_type() {
  local path="$1"
  if stat -f -c '%T' "${path}" >/dev/null 2>&1; then
    stat -f -c '%T' "${path}"
  else
    stat -f '%T' "${path}"
  fi
}

restore_filesystem="$(filesystem_type "${drill_root}")"
restore_filesystem_lower="$(printf '%s' "${restore_filesystem}" | tr '[:upper:]' '[:lower:]')"
if [[ "${restore_filesystem_lower}" =~ (nfs|cifs|smbfs|fuse\.sshfs) ]]; then
  fail_input "restored PGDATA must not use NFS/SMB/SSHFS (found ${restore_filesystem})"
fi
repo_size_kib="$(du -sk "${source_repo}" | awk '{print $1}')"

restore_args=(
  --stanza=portage-engine --pg1-path=/restore --repo1-path=/repo
  --type=time "--target=${target_time}" --target-action=promote
)

docker run --rm \
  --user root \
  --env "PGBACKREST_REPO1_CIPHER_PASS=${PORTAGE_PGBACKREST_CIPHER_PASS:?set PORTAGE_PGBACKREST_CIPHER_PASS}" \
  --volume "${source_repo}:/repo:ro" \
  --volume "${drill_root}:/restore" \
  --volume "${repo_root}/deploy/postgres/pgbackrest.conf:/etc/pgbackrest/pgbackrest.conf:ro" \
  portage-engine/postgres-pgbackrest:18.4-2.59.0 \
  sh -ec 'chown postgres:postgres /restore && exec gosu postgres pgbackrest "$@" restore' sh "${restore_args[@]}"

validation_container="$(docker run --detach \
  --user postgres \
  --env "PGBACKREST_REPO1_CIPHER_PASS=${PORTAGE_PGBACKREST_CIPHER_PASS}" \
  --env "PGUSER=${PORTAGE_POSTGRES_USER:-portage}" \
  --env "PGDATABASE=${PORTAGE_POSTGRES_DB:-portage_engine}" \
  --env "PGDATA=/restore" \
  --volume "${drill_root}:/restore" \
  --volume "${source_repo}:/var/lib/pgbackrest:ro" \
  --volume "${source_repo}:/repo:ro" \
  --volume "${repo_root}/deploy/postgres/pgbackrest.conf:/etc/pgbackrest/pgbackrest.conf:ro" \
  --volume "${repo_root}/scripts/recovery:/recovery:ro" \
  portage-engine/postgres-pgbackrest:18.4-2.59.0 \
  postgres -D /restore)"

ready=false
for _ in $(seq 1 60); do
  if docker exec \
    --env "PGUSER=${PORTAGE_POSTGRES_USER:-portage}" \
    --env "PGDATABASE=${PORTAGE_POSTGRES_DB:-portage_engine}" \
    "${validation_container}" pg_isready -q; then
    ready=true
    break
  fi
  if [[ "$(docker inspect --format '{{.State.Running}}' "${validation_container}")" != "true" ]]; then
    docker logs "${validation_container}" >&2
    exit 1
  fi
  sleep 1
done
if [[ "${ready}" != "true" ]]; then
  docker logs "${validation_container}" >&2
  echo "restored PostgreSQL did not become ready" >&2
  exit 1
fi

validation="$(docker exec \
  --env "PGUSER=${PORTAGE_POSTGRES_USER:-portage}" \
  --env "PGDATABASE=${PORTAGE_POSTGRES_DB:-portage_engine}" \
  "${validation_container}" \
  psql --no-psqlrc --quiet --tuples-only --no-align \
  --set=expected_schema="${expected_schema}" \
  --set=expected_marker="${expected_marker}" \
  --set=absent_marker="${absent_marker}" \
  --set=app_role="${app_role}" \
  --set=signer_role="${signer_role}" \
  --set=actuator_role="${actuator_role}" \
  --file /recovery/schema-current-restore-check.sql)"

completed_epoch_ms="$(python3 -c 'import time; print(time.time_ns() // 1000000)')"
rto_seconds="$(((completed_epoch_ms - started_epoch_ms + 999) / 1000))"
rpo_seconds="$(python3 - "${target_time}" "${last_durable_utc}" <<'PY'
from datetime import datetime
import sys

def parse(value: str) -> datetime:
    return datetime.fromisoformat(value.strip().replace("Z", "+00:00"))

delta = int((parse(sys.argv[1]) - parse(sys.argv[2])).total_seconds())
if delta < 0:
    raise SystemExit("PITR durable marker time is later than the recovery target")
print(delta)
PY
)"
if (( rpo_seconds > max_rpo_seconds )); then
  echo "PITR RPO ${rpo_seconds}s exceeds ${max_rpo_seconds}s" >&2
  exit 1
fi
if (( rto_seconds > max_rto_seconds )); then
  echo "PITR RTO ${rto_seconds}s exceeds ${max_rto_seconds}s" >&2
  exit 1
fi

python3 - "${owner}" "${isolated_target}" "${target_time}" \
  "${last_durable_utc}" "${rpo_seconds}" "${rto_seconds}" \
  "${repo_size_kib}" "${restore_filesystem}" "${validation}" <<'PY'
import json
import sys

print(json.dumps({
    "schema_version": 1,
    "evidence_kind": "pgbackrest-pitr-restore",
    "status": "passed",
    "owner": sys.argv[1],
    "isolated_target": sys.argv[2],
    "target_time": sys.argv[3],
    "last_durable_time": sys.argv[4],
    "rpo_seconds": int(sys.argv[5]),
    "rto_seconds": int(sys.argv[6]),
    "backup_repository_size_bytes": int(sys.argv[7]) * 1024,
    "restore_pgdata_filesystem": sys.argv[8],
    "validation": json.loads(sys.argv[9]),
}, sort_keys=True))
PY
