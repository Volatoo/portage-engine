#!/usr/bin/env bash
set -euo pipefail

target_time="${1:-}"
expected_marker="${PORTAGE_PITR_ASSERT_RESOURCE_ID:-}"
source_repo="${PORTAGE_PGBACKREST_REPO:-./backups/pgbackrest}"
drill_root="$(mktemp -d "${TMPDIR:-/tmp}/portage-pitr.XXXXXX")"
validation_container=""
cleanup() {
  if [[ -n "${validation_container}" ]]; then
    docker rm -f "${validation_container}" >/dev/null 2>&1 || true
  fi
  rm -rf "${drill_root}"
}
trap cleanup EXIT

restore_args=(--stanza=portage-engine --pg1-path=/restore --repo1-path=/repo)
if [[ -n "${target_time}" ]]; then
  restore_args+=(--type=time "--target=${target_time}" --target-action=promote)
fi

docker run --rm \
  --user root \
  --env "PGBACKREST_REPO1_CIPHER_PASS=${PORTAGE_PGBACKREST_CIPHER_PASS:?set PORTAGE_PGBACKREST_CIPHER_PASS}" \
  --volume "${source_repo}:/repo:ro" \
  --volume "${drill_root}:/restore" \
  --volume "$(pwd)/deploy/postgres/pgbackrest.conf:/etc/pgbackrest/pgbackrest.conf:ro" \
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
  --volume "$(pwd)/deploy/postgres/pgbackrest.conf:/etc/pgbackrest/pgbackrest.conf:ro" \
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

validation_sql="SELECT max(version_id)::text || ':' || (SELECT count(*) FROM build_jobs)::text FROM goose_db_version WHERE is_applied;"
if [[ -n "${expected_marker}" ]]; then
  if [[ ! "${expected_marker}" =~ ^[A-Za-z0-9._:-]+$ ]]; then
    echo "PORTAGE_PITR_ASSERT_RESOURCE_ID contains unsupported characters" >&2
    exit 2
  fi
  validation_sql="SELECT max(version_id)::text || ':' || (SELECT count(*) FROM build_jobs)::text || ':' || (SELECT count(*) FROM audit_events WHERE resource_id = '${expected_marker}')::text FROM goose_db_version WHERE is_applied;"
fi

validation="$(docker exec \
  --env "PGUSER=${PORTAGE_POSTGRES_USER:-portage}" \
  --env "PGDATABASE=${PORTAGE_POSTGRES_DB:-portage_engine}" \
  "${validation_container}" \
  psql --no-psqlrc --tuples-only --no-align --command \
  "${validation_sql}")"
if [[ -n "${expected_marker}" && "${validation}" != 7:*:1 ]]; then
  echo "unexpected restored schema/job/marker validation: ${validation}" >&2
  exit 1
fi
if [[ -z "${expected_marker}" && "${validation}" != 7:* ]]; then
  echo "unexpected restored schema/job validation: ${validation}" >&2
  exit 1
fi

printf 'pgBackRest restore drill passed (target=%s, schema_jobs=%s)\n' \
  "${target_time:-latest}" "${validation}"
