#!/usr/bin/env bash
# Create before/after audit markers around a PITR target in an explicitly
# isolated drill database. The generated state file contains no credential.

set -euo pipefail

owner="${PORTAGE_DRILL_OWNER:-}"
isolated_target="${PORTAGE_DRILL_TARGET:-}"
present_marker="${PORTAGE_PITR_ASSERT_RESOURCE_ID:-}"
absent_marker="${PORTAGE_PITR_ASSERT_ABSENT_RESOURCE_ID:-}"
state_file="${PORTAGE_PITR_STATE_FILE:-}"
evidence_dir="${PORTAGE_DRILL_EVIDENCE_DIR:-}"

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
[[ -n "${owner}" ]] || fail_input "PORTAGE_DRILL_OWNER is required"
[[ "${isolated_target}" =~ ^[A-Za-z0-9._:/-]*(drill|recovery|sandbox|test)[A-Za-z0-9._:/-]*$ ]] ||
  fail_input "PORTAGE_DRILL_TARGET must explicitly name an isolated drill target"
target_lower="$(printf '%s' "${isolated_target}" | tr '[:upper:]' '[:lower:]')"
[[ ! "${target_lower}" =~ (^|[._:/-])(prod|production|live|main|primary)([._:/-]|$) ]] ||
  fail_input "production-like marker targets are refused"
[[ "${present_marker}" =~ ^[A-Za-z0-9._:-]+$ ]] ||
  fail_input "PORTAGE_PITR_ASSERT_RESOURCE_ID is required and must be audit-safe"
[[ "${absent_marker}" =~ ^[A-Za-z0-9._:-]+$ ]] ||
  fail_input "PORTAGE_PITR_ASSERT_ABSENT_RESOURCE_ID is required and must be audit-safe"
[[ "${present_marker}" != "${absent_marker}" ]] ||
  fail_input "PITR present and absent markers must differ"
[[ -n "${evidence_dir}" && "${evidence_dir}" == /* ]] ||
  fail_input "PORTAGE_DRILL_EVIDENCE_DIR must be an absolute path"
[[ -n "${state_file}" && "${state_file}" == "${evidence_dir}"/* ]] ||
  fail_input "PORTAGE_PITR_STATE_FILE must be inside the live evidence directory"
[[ ! -e "${state_file}" ]] || fail_input "PITR state file already exists: ${state_file}"

env_file="${PORTAGE_COMPOSE_ENV_FILE:-.env.compose}"
compose=(docker compose -f docker-compose.yml -f docker-compose.pgbackrest.yml)
if [[ -f "${env_file}" ]]; then
  compose+=(--env-file "${env_file}")
fi

psql_drill() {
  # The single quotes intentionally defer container environment expansion.
  # shellcheck disable=SC2016
  "${compose[@]}" exec -T postgres sh -ec \
    'exec psql --no-psqlrc --quiet --set=ON_ERROR_STOP=1 --tuples-only --no-align --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" "$@"' \
    sh "$@"
}

last_durable_utc="$(
  psql_drill --set=marker="${present_marker}" --set=owner="${owner}" <<'SQL'
INSERT INTO audit_events (
    actor, action, resource_type, resource_id, request_id, detail, outcome
) VALUES (
    :'owner', 'recovery.pitr.before', 'recovery_drill', :'marker',
    :'marker', jsonb_build_object('classification', 'non-secret-drill-marker'),
    'success'
)
RETURNING to_char(occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"');
SQL
)"
psql_drill --command 'SELECT pg_switch_wal();' >/dev/null
target_time="$(
  psql_drill --command \
    "SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"');"
)"
psql_drill --set=marker="${absent_marker}" --set=owner="${owner}" <<'SQL' >/dev/null
INSERT INTO audit_events (
    actor, action, resource_type, resource_id, request_id, detail, outcome
) VALUES (
    :'owner', 'recovery.pitr.after', 'recovery_drill', :'marker',
    :'marker', jsonb_build_object('classification', 'non-secret-drill-marker'),
    'success'
);
SQL
psql_drill --command 'SELECT pg_switch_wal();' >/dev/null

umask 077
state_parent="$(dirname "${state_file}")"
[[ -d "${state_parent}" ]] || fail_input "PITR state parent does not exist: ${state_parent}"
temporary_state="${state_file}.tmp-$$"
cleanup() {
  if [[ -e "${temporary_state}" ]]; then
    mv "${temporary_state}" "${temporary_state}.incomplete"
  fi
}
trap cleanup EXIT
printf 'PORTAGE_PITR_LAST_DURABLE_UTC=%q\nPORTAGE_PITR_TARGET_TIME=%q\n' \
  "${last_durable_utc}" "${target_time}" >"${temporary_state}"
chmod 0600 "${temporary_state}"
mv "${temporary_state}" "${state_file}"
trap - EXIT

python3 - "${owner}" "${isolated_target}" "${present_marker}" \
  "${absent_marker}" "${last_durable_utc}" "${target_time}" "${state_file}" <<'PY'
import json
import sys
print(json.dumps({
    "schema_version": 1,
    "evidence_kind": "pgbackrest-pitr-markers",
    "status": "passed",
    "owner": sys.argv[1],
    "isolated_target": sys.argv[2],
    "present_marker": sys.argv[3],
    "absent_marker": sys.argv[4],
    "last_durable_time": sys.argv[5],
    "target_time": sys.argv[6],
    "state_file": sys.argv[7],
}, sort_keys=True))
PY
