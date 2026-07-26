#!/usr/bin/env bash
set -euo pipefail

env_file="${1:-.env.compose}"
compose=(docker compose -f docker-compose.yml -f docker-compose.pgbackrest.yml)
if [[ -f "${env_file}" ]]; then
  compose+=(--env-file "${env_file}")
fi

repo="${PORTAGE_PGBACKREST_REPO:-./backups/pgbackrest}"
mkdir -p "${repo}"
chmod 0750 "${repo}"

"${compose[@]}" exec -T --user root postgres \
  sh -ec 'chown -R postgres:postgres /var/lib/pgbackrest /var/spool/pgbackrest'
"${compose[@]}" exec -T --user postgres postgres \
  pgbackrest --stanza=portage-engine stanza-create
"${compose[@]}" exec -T --user postgres postgres \
  pgbackrest --stanza=portage-engine check
