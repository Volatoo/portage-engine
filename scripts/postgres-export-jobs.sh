#!/usr/bin/env bash
set -euo pipefail

output="${1:-backups/postgres/jobs-export-$(date -u +%Y%m%dT%H%M%SZ).json}"
mkdir -p "$(dirname "${output}")"

tmp="${output}.tmp"
docker compose exec -T postgres \
  psql --no-psqlrc --quiet --tuples-only --no-align \
    --username="${PORTAGE_POSTGRES_USER:-portage}" \
    --dbname="${PORTAGE_POSTGRES_DB:-portage_engine}" \
    --command="
      SELECT jsonb_pretty(jsonb_build_object(
        'schema_version', (SELECT max(version_id) FROM goose_db_version WHERE is_applied),
        'exported_at', clock_timestamp(),
        'authority', 'postgresql',
        'jobs', COALESCE(jsonb_agg(jsonb_build_object(
          'id', id,
          'state', state,
          'request', request,
          'status', status_snapshot,
          'visible', legacy_visible,
          'created_at', created_at,
          'updated_at', updated_at
        ) ORDER BY created_at), '[]'::jsonb)
      ))
      FROM build_jobs;
    " >"${tmp}"

chmod 0600 "${tmp}"
mv "${tmp}" "${output}"
printf 'Exported PostgreSQL job snapshot to %s\n' "${output}"
