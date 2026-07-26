#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 BACKUP.dump [portage_restore_name]" >&2
  exit 2
fi

backup_path="$1"
restore_db="${2:-portage_restore_$(date -u '+%Y%m%d%H%M%S')}"
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

if [[ "${schema_version}" -lt 1 || "${table_count}" -lt 10 ]]; then
  echo "restore validation failed: schema_version=${schema_version}, public_tables=${table_count}" >&2
  exit 1
fi
if [[ "${schema_version}" -ge 2 ]]; then
  ledger_columns="$(
    "${compose[@]}" exec -T postgres sh -ec \
      'psql --no-psqlrc --tuples-only --no-align --username="$POSTGRES_USER" --dbname="$1" -c "SELECT count(*) FROM information_schema.columns WHERE table_schema = '\''public'\'' AND table_name = '\''build_jobs'\'' AND column_name IN ('\''status_snapshot'\'','\''status_digest'\'','\''ledger_revision'\'','\''legacy_visible'\'','\''source'\'')"' \
      sh "${restore_db}"
  )"
  if [[ "${ledger_columns}" -ne 5 ]]; then
    echo "restore validation failed: schema v2 ledger columns=${ledger_columns}, expected=5" >&2
    exit 1
  fi
fi

printf 'PostgreSQL restore verified:\n  database: %s\n  schema version: %s\n  public tables: %s\n  job ledger rows: %s\n' \
  "${restore_db}" "${schema_version}" "${table_count}" "${job_count}"
if [[ "${PORTAGE_KEEP_RESTORE_DATABASE:-false}" == "true" ]]; then
  echo "  retained: true"
else
  echo "  retained: false (validation database will be removed)"
fi
