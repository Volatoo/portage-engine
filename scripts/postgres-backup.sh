#!/usr/bin/env bash
set -euo pipefail

env_file="${PORTAGE_COMPOSE_ENV_FILE:-.env.compose}"
compose=(docker compose)
if [[ -f "${env_file}" ]]; then
  compose+=(--env-file "${env_file}")
fi

backup_dir="${PORTAGE_POSTGRES_BACKUP_DIR:-backups/postgres}"
timestamp="$(date -u '+%Y%m%dT%H%M%SZ')"
backup_path="${1:-${backup_dir}/portage-engine-${timestamp}.dump}"

mkdir -p "$(dirname "${backup_path}")"
umask 077
backup_tmp="$(mktemp "${backup_path}.tmp.XXXXXX")"
cleanup() {
  rm -f "${backup_tmp}"
}
trap cleanup EXIT

"${compose[@]}" exec -T postgres sh -ec \
  'exec pg_dump --format=custom --compress=9 --no-owner --no-privileges --username="$POSTGRES_USER" --dbname="$POSTGRES_DB"' \
  >"${backup_tmp}"

if [[ ! -s "${backup_tmp}" ]]; then
  echo "backup is empty: ${backup_path}" >&2
  exit 1
fi
mv "${backup_tmp}" "${backup_path}"

sha256_file="${backup_path}.sha256"
backup_name="$(basename "${backup_path}")"
backup_parent="$(dirname "${backup_path}")"
if command -v shasum >/dev/null 2>&1; then
  (cd "${backup_parent}" && shasum -a 256 "${backup_name}") >"${sha256_file}"
else
  (cd "${backup_parent}" && sha256sum "${backup_name}") >"${sha256_file}"
fi

printf 'PostgreSQL logical backup created:\n  %s\n  %s\n' "${backup_path}" "${sha256_file}"
