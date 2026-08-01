#!/usr/bin/env bash
# Container-side environment variables intentionally expand inside sh -ec.
# shellcheck disable=SC2016
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 BACKUP.dump [portage_restore_name]" >&2
  exit 2
fi

backup_path="$1"
restore_db="${2:-portage_restore_$(date -u '+%Y%m%d%H%M%S')}"
drill_target="${PORTAGE_DRILL_TARGET:-}"
drill_owner="${PORTAGE_DRILL_OWNER:-}"
if [[ "${PORTAGE_DRILL_ISOLATED:-}" != "true" ||
  "${PORTAGE_DRILL_CONFIRM:-}" != "RUN_PUBLIC_BETA_RECOVERY_DRILL" ||
  "${PORTAGE_DRILL_DESTRUCTIVE_CONFIRM:-}" != "DESTROY_ISOLATED_DRILL_TARGET_ONLY" ]]; then
  echo "logical restore requires the isolated drill and destructive confirmation variables" >&2
  exit 2
fi
if [[ -z "${drill_owner}" ]]; then
  echo "PORTAGE_DRILL_OWNER is required" >&2
  exit 2
fi
drill_target_lower="$(printf '%s' "${drill_target}" | tr '[:upper:]' '[:lower:]')"
if [[ ! "${drill_target}" =~ ^[A-Za-z0-9._:/-]*(drill|recovery|sandbox|test)[A-Za-z0-9._:/-]*$ ]] ||
  [[ "${drill_target_lower}" =~ (^|[._:/-])(prod|production|live|main|primary)([._:/-]|$) ]]; then
  echo "PORTAGE_DRILL_TARGET must name a non-production isolated target" >&2
  exit 2
fi
if [[ ! -r "${backup_path}" ]]; then
  echo "backup is not readable: ${backup_path}" >&2
  exit 2
fi
if [[ ! "${restore_db}" =~ ^portage_restore_[a-zA-Z0-9_]+$ ]]; then
  echo "restore database must start with portage_restore_ and contain only letters, digits, or underscores" >&2
  exit 2
fi

checksum_path="${backup_path}.sha256"
if [[ -f "${checksum_path}" ]]; then
  backup_parent="$(dirname "${backup_path}")"
  checksum_name="$(basename "${checksum_path}")"
  if command -v shasum >/dev/null 2>&1; then
    (cd "${backup_parent}" && shasum -a 256 -c "${checksum_name}")
  else
    (cd "${backup_parent}" && sha256sum -c "${checksum_name}")
  fi
fi

env_file="${PORTAGE_COMPOSE_ENV_FILE:-.env.compose}"
compose=(docker compose)
if [[ -f "${env_file}" ]]; then
  compose+=(--env-file "${env_file}")
fi

pgdata_filesystem="$(
  "${compose[@]}" exec -T postgres sh -ec 'stat -f -c %T "$PGDATA"'
)"
if printf '%s' "${pgdata_filesystem}" |
  tr '[:upper:]' '[:lower:]' |
  rg -q '(nfs|cifs|smbfs|fuse\.sshfs)'; then
  echo "logical restore refused: PGDATA is on ${pgdata_filesystem}" >&2
  exit 2
fi

restore_created=false
cleanup() {
  if [[ "${restore_created}" == "true" && "${PORTAGE_KEEP_RESTORE_DATABASE:-false}" != "true" ]]; then
    "${compose[@]}" exec -T postgres sh -ec \
      'dropdb --if-exists --force --username="$POSTGRES_USER" "$1"' sh "${restore_db}" >/dev/null
  fi
}
trap cleanup EXIT

"${compose[@]}" exec -T postgres sh -ec \
  'createdb --username="$POSTGRES_USER" "$1"' sh "${restore_db}"
restore_created=true
"${compose[@]}" exec -T postgres sh -ec \
  'exec pg_restore --exit-on-error --no-owner --no-privileges --username="$POSTGRES_USER" --dbname="$1"' sh "${restore_db}" \
  <"${backup_path}"

schema_version="$(
  "${compose[@]}" exec -T postgres sh -ec \
    'psql --no-psqlrc --tuples-only --no-align --username="$POSTGRES_USER" --dbname="$1" -c "SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied"' \
    sh "${restore_db}"
)"
table_count="$(
  "${compose[@]}" exec -T postgres sh -ec \
    'psql --no-psqlrc --tuples-only --no-align --username="$POSTGRES_USER" --dbname="$1" -c "SELECT count(*) FROM information_schema.tables WHERE table_schema = '\''public'\''"' \
    sh "${restore_db}"
)"
job_count="$(
  "${compose[@]}" exec -T postgres sh -ec \
    'psql --no-psqlrc --tuples-only --no-align --username="$POSTGRES_USER" --dbname="$1" -c "SELECT count(*) FROM build_jobs"' \
    sh "${restore_db}"
)"

if [[ "${schema_version}" -ne 26 || "${table_count}" -lt 30 ]]; then
  echo "restore validation failed: schema_version=${schema_version}, public_tables=${table_count}" >&2
  exit 1
fi
required_relations="$(
  "${compose[@]}" exec -T postgres sh -ec \
    'psql --no-psqlrc --tuples-only --no-align --username="$POSTGRES_USER" --dbname="$1" -c "SELECT count(*) FROM unnest(ARRAY['\''build_jobs'\'','\''build_attempts'\'','\''signing_tasks'\'','\''workload_issuer_generations'\'','\''workload_certificates'\'','\''scheduler_capacity_actions'\'','\''scheduler_capacity_instances'\'','\''targets'\'','\''project_memberships'\'','\''monitor_job_outcomes'\'']) AS required(name) WHERE to_regclass('\''public.'\'' || required.name) IS NOT NULL"' \
    sh "${restore_db}"
)"
if [[ "${required_relations}" -ne 10 ]]; then
  echo "restore validation failed: schema v26 required relations=${required_relations}, expected=10" >&2
  exit 1
fi

printf 'PostgreSQL restore verified:\n  database: %s\n  schema version: %s\n  public tables: %s\n  job ledger rows: %s\n' \
  "${restore_db}" "${schema_version}" "${table_count}" "${job_count}"
printf '  owner: %s\n  isolated target: %s\n  PGDATA filesystem: %s\n' \
  "${drill_owner}" "${drill_target}" "${pgdata_filesystem}"
if [[ "${PORTAGE_KEEP_RESTORE_DATABASE:-false}" == "true" ]]; then
  echo "  retained: true"
else
  echo "  retained: false (validation database will be removed)"
fi
