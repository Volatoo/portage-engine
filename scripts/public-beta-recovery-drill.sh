#!/usr/bin/env bash
# Public Beta recovery Gate coordinator. Live phases are deliberately operator-
# driven: the repository records and validates real command results but never
# substitutes a local fixture for production recovery evidence.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd "${script_dir}/.." && pwd)"
readonly repo_root
readonly evidence_helper="${repo_root}/scripts/recovery/evidence.py"
readonly validator="${repo_root}/scripts/recovery/validate.py"
readonly gate="${1:-static}"
case "${gate}" in
  static|vault|postgres|object|signer|all) ;;
  *) echo "usage: $0 [static|vault|postgres|object|signer|all]" >&2; exit 2 ;;
esac
execution="$([[ "${gate}" == "static" ]] && printf static || printf live)"
readonly execution
readonly run_id="${PORTAGE_DRILL_RUN_ID:-$(date -u '+%Y%m%dT%H%M%SZ')-${gate}-$$}"
started_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
readonly started_at
baseline_commit="$(git -C "${repo_root}" rev-parse HEAD)"
readonly baseline_commit
readonly owner="${PORTAGE_DRILL_OWNER:-repository-static-validation}"
readonly isolated_target="${PORTAGE_DRILL_TARGET:-not-applicable}"

if [[ "${execution}" == "live" && -z "${PORTAGE_DRILL_EVIDENCE_DIR:-}" ]]; then
  echo "live drills require an explicit absolute PORTAGE_DRILL_EVIDENCE_DIR" >&2
  exit 2
fi
readonly evidence_dir="${PORTAGE_DRILL_EVIDENCE_DIR:-${repo_root}/.local/recovery-evidence/${run_id}}"
readonly records_file="${evidence_dir}/checks.jsonl"
readonly summary_file="${evidence_dir}/summary.json"
readonly logs_dir="${evidence_dir}/logs"
finalized=false

if [[ ! "${run_id}" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ || "${run_id}" == *..* ]]; then
  echo "PORTAGE_DRILL_RUN_ID must be a path-free audit identifier" >&2
  exit 2
fi
if [[ "${evidence_dir}" != /* || "${evidence_dir}" == "/" ||
  "${evidence_dir}" == "/tmp" || "${evidence_dir}" == "${repo_root}" ]]; then
  echo "evidence directory must be an explicit, narrow absolute path" >&2
  exit 2
fi
if [[ -e "${evidence_dir}" ]]; then
  if [[ ! -d "${evidence_dir}" || -n "$(ls -A "${evidence_dir}")" ]]; then
    echo "evidence directory already exists and is not empty: ${evidence_dir}" >&2
    exit 2
  fi
else
  mkdir -p "${evidence_dir}"
  chmod 0700 "${evidence_dir}"
fi
mkdir "${logs_dir}"
chmod 0700 "${logs_dir}"
: >"${records_file}"
chmod 0600 "${records_file}"

now_millis() {
  python3 -c 'import time; print(time.time_ns() // 1000000)'
}

now_utc() {
  date -u '+%Y-%m-%dT%H:%M:%SZ'
}

append_record() {
  local check_id="$1" status="$2" destructive="$3" detail="$4"
  local command_label="$5" command_value="$6" log_file="$7"
  local check_started="$8" check_completed="$9" duration_ms="${10}"
  python3 "${evidence_helper}" append \
    --file "${records_file}" --run-id "${run_id}" --gate "${gate}" \
    --check-id "${check_id}" --execution "${execution}" --status "${status}" \
    --destructive "${destructive}" --started-at "${check_started}" \
    --completed-at "${check_completed}" --duration-ms "${duration_ms}" \
    --owner "${owner}" --target "${isolated_target}" --detail "${detail}" \
    --command-label "${command_label}" --command "${command_value}" --log "${log_file}"
}

finish() {
  local status="${1:-}"
  if [[ "${finalized}" == "true" ]]; then
    return
  fi
  local arguments=(
    finalize --file "${records_file}" --output "${summary_file}"
    --run-id "${run_id}" --gate "${gate}" --execution "${execution}"
    --owner "${owner}" --target "${isolated_target}"
    --baseline-commit "${baseline_commit}" --started-at "${started_at}"
    --completed-at "$(now_utc)"
  )
  if [[ -n "${status}" ]]; then
    arguments+=(--status "${status}")
  fi
  python3 "${evidence_helper}" "${arguments[@]}"
  finalized=true
}

on_exit() {
  local status=$?
  if [[ "${finalized}" != "true" ]]; then
    finish failed || true
  fi
  if [[ ${status} -ne 0 ]]; then
    echo "recovery Gate failed; evidence: ${summary_file}" >&2
  fi
  trap - EXIT
  exit "${status}"
}
trap on_exit EXIT

preflight_fail() {
  local detail="$1"
  local stamp
  stamp="$(now_utc)"
  append_record preflight failed false "${detail}" preflight "" /dev/null \
    "${stamp}" "${stamp}" 0
  echo "${detail}" >&2
  exit 2
}

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    preflight_fail "required operator input is missing: ${name}"
  fi
  if [[ "${name}" == *_CMD ]]; then
    case "${!name}" in
      true|:|/bin/true|/usr/bin/true|echo|echo\ *|printf|printf\ *)
        preflight_fail "${name} must be a real environment check, not a no-op command"
        ;;
    esac
  fi
}

require_file() {
  local name="$1"
  require_env "${name}"
  if [[ ! -r "${!name}" ]]; then
    preflight_fail "operator evidence is not readable: ${name}=${!name}"
  fi
}

shell_quote_join() {
  local value output=""
  for value in "$@"; do
    printf -v value '%q' "${value}"
    output+="${output:+ }${value}"
  done
  printf '%s' "${output}"
}

run_shell_check() {
  local check_id="$1" destructive="$2" command_label="$3" command_value="$4"
  local detail="${5:-command completed successfully}"
  local log_file="${logs_dir}/${check_id}.log" start_ms end_ms rc status
  start_ms="$(now_millis)"
  set +e
  (cd "${repo_root}" && bash -o pipefail -c "${command_value}") >"${log_file}" 2>&1
  rc=$?
  set -e
  end_ms="$(now_millis)"
  status=passed
  if [[ ${rc} -ne 0 ]]; then
    status=failed
    detail="${command_label} exited with status ${rc}"
  fi
  chmod 0600 "${log_file}"
  append_record "${check_id}" "${status}" "${destructive}" "${detail}" \
    "${command_label}" "${command_value}" "${log_file}" \
    "$(date -u -r "$((start_ms / 1000))" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || now_utc)" \
    "$(now_utc)" "$((end_ms - start_ms))"
  if [[ ${rc} -ne 0 ]]; then
    tail -n 40 "${log_file}" >&2 || true
    exit "${rc}"
  fi
}

run_argv_check() {
  local check_id="$1" destructive="$2" command_label="$3" detail="$4"
  shift 4
  local command_value log_file start_ms end_ms rc status
  command_value="$(shell_quote_join "$@")"
  log_file="${logs_dir}/${check_id}.log"
  start_ms="$(now_millis)"
  set +e
  (cd "${repo_root}" && "$@") >"${log_file}" 2>&1
  rc=$?
  set -e
  end_ms="$(now_millis)"
  status=passed
  if [[ ${rc} -ne 0 ]]; then
    status=failed
    detail="${command_label} exited with status ${rc}"
  fi
  chmod 0600 "${log_file}"
  append_record "${check_id}" "${status}" "${destructive}" "${detail}" \
    "${command_label}" "${command_value}" "${log_file}" \
    "$(now_utc)" "$(now_utc)" "$((end_ms - start_ms))"
  if [[ ${rc} -ne 0 ]]; then
    tail -n 40 "${log_file}" >&2 || true
    exit "${rc}"
  fi
}

append_not_run() {
  local check_id="$1" detail="$2" stamp
  stamp="$(now_utc)"
  append_record "${check_id}" not-run false "${detail}" "operator live Gate" "" \
    /dev/null "${stamp}" "${stamp}" 0
}

validate_owner_list() {
  local name="$1" raw="$2"
  local count
  count="$(python3 - "${raw}" <<'PY'
import sys
owners = {item.strip() for item in sys.argv[1].split(",") if item.strip()}
print(len(owners))
PY
)"
  if [[ "${count}" -lt 2 ]]; then
    preflight_fail "${name} must name at least two distinct custodians"
  fi
}

validate_live_contract() {
  local target_lower
  target_lower="$(printf '%s' "${PORTAGE_DRILL_TARGET:-}" | tr '[:upper:]' '[:lower:]')"
  case "${gate}" in
    vault|postgres|object|signer|all) ;;
    *) preflight_fail "usage: $0 [static|vault|postgres|object|signer|all]" ;;
  esac
  require_env PORTAGE_DRILL_OWNER
  require_env PORTAGE_DRILL_TARGET
  if [[ "${PORTAGE_DRILL_ISOLATED:-}" != "true" ]]; then
    preflight_fail "PORTAGE_DRILL_ISOLATED must be exactly true"
  fi
  if [[ "${PORTAGE_DRILL_CONFIRM:-}" != "RUN_PUBLIC_BETA_RECOVERY_DRILL" ]]; then
    preflight_fail "PORTAGE_DRILL_CONFIRM must be RUN_PUBLIC_BETA_RECOVERY_DRILL"
  fi
  if [[ "${PORTAGE_DRILL_DESTRUCTIVE_CONFIRM:-}" != "DESTROY_ISOLATED_DRILL_TARGET_ONLY" ]]; then
    preflight_fail "PORTAGE_DRILL_DESTRUCTIVE_CONFIRM must be DESTROY_ISOLATED_DRILL_TARGET_ONLY"
  fi
  if [[ ! "${PORTAGE_DRILL_TARGET}" =~ ^[A-Za-z0-9._:/-]*(drill|recovery|sandbox|test)[A-Za-z0-9._:/-]*$ ]]; then
    preflight_fail "PORTAGE_DRILL_TARGET must explicitly name a drill/recovery/sandbox/test target"
  fi
  if [[ "${target_lower}" =~ (^|[._:/-])(prod|production|live|main|primary)([._:/-]|$) ]]; then
    preflight_fail "production-like drill targets are refused"
  fi
  require_env PORTAGE_DRILL_ISOLATION_CHECK_CMD
  run_shell_check isolation-target false "operator isolation proof" \
    "${PORTAGE_DRILL_ISOLATION_CHECK_CMD}" \
    "operator isolation proof completed before destructive stages"
}

run_static() {
  run_argv_check python-syntax false "Python recovery helper syntax" \
    "evidence helpers compile" env "PYTHONPYCACHEPREFIX=${evidence_dir}/pycache" \
    python3 -m py_compile \
    scripts/recovery/evidence.py scripts/recovery/validate.py \
    scripts/recovery/pgbackrest-target-time.py \
    scripts/recovery/filesystem-type.py
  run_shell_check shell-syntax false "recovery shell syntax" \
    "bash -n scripts/public-beta-recovery-drill.sh scripts/pgbackrest-pitr-prepare.sh scripts/pgbackrest-restore-drill.sh scripts/postgres-restore-check.sh scripts/recovery/current-schema-version.sh scripts/test-vault-issuer.sh tests/recovery-drill-test.sh" \
    "recovery shell entry points parse"
  run_argv_check schema-authority-contract false "migration binary schema authority" \
    "binary support range and latest embedded migration agree" \
    scripts/recovery/current-schema-version.sh --json
  run_shell_check vault-contract false "Vault recovery contract" \
    "rg -q 'TOKEN_RECOVERY' scripts/test-vault-issuer.sh && rg -q 'old -> dual -> new' scripts/test-vault-issuer.sh && rg -q 'allowed_uri_sans' scripts/recovery/validate.py" \
    "Vault token recovery, staged CA trust, and minimal role validators exist"
  run_argv_check object-policy-fixture false "object IAM split fixture" \
    "read/executor/signer/quarantine-GC/generation-GC policies are separated" \
    python3 scripts/recovery/validate.py object-policies \
    --read-policy tests/fixtures/recovery/object-read-policy.json \
    --executor-policy tests/fixtures/recovery/object-executor-policy.json \
    --signer-policy tests/fixtures/recovery/object-signer-policy.json \
    --quarantine-gc-policy tests/fixtures/recovery/object-quarantine-gc-policy.json \
    --generation-gc-policy tests/fixtures/recovery/object-generation-gc-policy.json \
    --read-principal fixture-read --executor-principal fixture-executor \
    --signer-principal fixture-signer --quarantine-gc-principal fixture-quarantine-gc \
    --generation-gc-principal fixture-generation-gc
  run_shell_check object-contract false "immutable object recovery contract" \
    "rg -q 'CompareAndSwap' internal/storage/s3_integration_test.go && rg -q 'ReplicatePublishedChannel' internal/storage/s3_integration_test.go && rg -q 'GarbageCollectPublishedGenerations' internal/storage/s3_integration_test.go" \
    "CAS, replication, and generation-GC cases are declared in the object integration source"
  run_shell_check signer-boundary false "signer private-key boundary" \
    "test \"$(rg -l 'portage-signer-key:/var/lib/portage-signer' docker-compose.yml docker-compose.public.yml | wc -l | tr -d ' ')\" = 1 && ! sed -n '/FROM runtime-base AS api-runtime/,/FROM runtime-base AS dashboard-runtime/p' Dockerfile | rg -q 'gnupg' && ! sed -n '/FROM runtime-base AS dashboard-runtime/,/FROM runtime-base AS migrate-runtime/p' Dockerfile | rg -q 'gnupg'" \
    "only signer receives its private volume; API and Dashboard images contain no GnuPG"
  for live_gate in vault postgres object signer; do
    append_not_run "${live_gate}-live" \
      "external ${live_gate} environment was not supplied;现场 Gate intentionally not run"
  done
  # object-contract above reads source, not results: s3_integration_test.go
  # skips itself unless PORTAGE_S3_INTEGRATION=1, which no workflow and no make
  # target sets. Behaviour coverage is a live gate, and it did not run.
  append_not_run object-integration-live \
    "internal/storage/s3_integration_test.go requires PORTAGE_S3_INTEGRATION=1 and a live object store; CAS, replication, audit, rollback closure, and reference-aware GC behaviour was not executed"
}

run_vault() {
  local storage_backend_lower
  require_env PORTAGE_VAULT_STORAGE_BACKEND
  storage_backend_lower="$(printf '%s' "${PORTAGE_VAULT_STORAGE_BACKEND}" | tr '[:upper:]' '[:lower:]')"
  if [[ "${storage_backend_lower}" =~ ^(file|inmem|dev)$ ]]; then
    preflight_fail "Vault storage backend must be durable and HA-capable"
  fi
  require_env PORTAGE_VAULT_UNSEAL_METHOD
  require_env PORTAGE_VAULT_UNSEAL_OWNERS
  require_env PORTAGE_VAULT_RECOVERY_OWNERS
  validate_owner_list PORTAGE_VAULT_UNSEAL_OWNERS "${PORTAGE_VAULT_UNSEAL_OWNERS}"
  validate_owner_list PORTAGE_VAULT_RECOVERY_OWNERS "${PORTAGE_VAULT_RECOVERY_OWNERS}"
  require_env VAULT_ADDR
  require_env PORTAGE_VAULT_DRILL_MOUNT
  require_env PORTAGE_VAULT_DRILL_ROLE
  require_env PORTAGE_VAULT_ALLOWED_URI_SAN
  require_env PORTAGE_VAULT_ALLOWED_COMMON_NAME
  if [[ ! "${PORTAGE_VAULT_DRILL_MOUNT}" =~ (drill|recovery|test) ]]; then
    preflight_fail "PORTAGE_VAULT_DRILL_MOUNT must be an isolated drill mount"
  fi
  run_argv_check vault-ha-status false "vault status -format=json" \
    "Vault is initialized, unsealed, HA-enabled, and reports durable storage" \
    vault status -format=json
  run_argv_check vault-ha-status-validate false "validate Vault HA status" \
    "Vault HA status is fail-closed" python3 "${validator}" vault-status \
    --file "${logs_dir}/vault-ha-status.log"
  run_argv_check vault-minimal-sign-role false "vault read PKI role" \
    "Vault drill PKI sign role was read from the live mount" vault read -format=json \
    "${PORTAGE_VAULT_DRILL_MOUNT%/}/roles/${PORTAGE_VAULT_DRILL_ROLE}"
  run_argv_check vault-minimal-sign-role-validate false "validate minimal PKI role" \
    "PKI role is client-only, URI-SAN constrained, modern-key-only, and <=24h" \
    python3 "${validator}" vault-role \
    --file "${logs_dir}/vault-minimal-sign-role.log" \
    --allowed-uri-san "${PORTAGE_VAULT_ALLOWED_URI_SAN}" \
    --allowed-common-name "${PORTAGE_VAULT_ALLOWED_COMMON_NAME}"
  local name
  for name in \
    PORTAGE_VAULT_UNSEAL_CHECK_CMD \
    PORTAGE_VAULT_TOKEN_LOSS_CHECK_CMD PORTAGE_VAULT_TOKEN_RECOVERY_CMD \
    PORTAGE_VAULT_OLD_CA_CHECK_CMD PORTAGE_VAULT_DUAL_CA_CHECK_CMD \
    PORTAGE_VAULT_NEW_CA_CHECK_CMD PORTAGE_VAULT_LEAF_REVOKE_CMD \
    PORTAGE_VAULT_ISSUER_REVOKE_CMD PORTAGE_VAULT_ISOLATED_RESTORE_CMD; do
    require_env "${name}"
  done
  run_shell_check vault-unseal true "HA seal/unseal owner ceremony" "${PORTAGE_VAULT_UNSEAL_CHECK_CMD}"
  run_shell_check vault-token-loss true "lost token rejection" "${PORTAGE_VAULT_TOKEN_LOSS_CHECK_CMD}"
  run_shell_check vault-token-recovery true "replacement token recovery" "${PORTAGE_VAULT_TOKEN_RECOVERY_CMD}"
  run_shell_check vault-old-ca true "old CA baseline" "${PORTAGE_VAULT_OLD_CA_CHECK_CMD}"
  run_shell_check vault-dual-ca true "old+new dual trust" "${PORTAGE_VAULT_DUAL_CA_CHECK_CMD}"
  run_shell_check vault-new-ca true "new CA only after drain" "${PORTAGE_VAULT_NEW_CA_CHECK_CMD}"
  run_shell_check vault-leaf-revoke true "leaf revocation" "${PORTAGE_VAULT_LEAF_REVOKE_CMD}"
  run_shell_check vault-issuer-revoke true "issuer revocation" "${PORTAGE_VAULT_ISSUER_REVOKE_CMD}"
  run_shell_check vault-isolated-restore true "isolated Vault recovery" "${PORTAGE_VAULT_ISOLATED_RESTORE_CMD}"
}

run_postgres() {
  local name
  for name in PORTAGE_POSTGRES_PGDATA_FSTYPE_CMD PORTAGE_POSTGRES_FULL_BACKUP_CMD \
    PORTAGE_POSTGRES_DIFF_BACKUP_CMD PORTAGE_POSTGRES_PITR_PREPARE_CMD \
    PORTAGE_POSTGRES_WAL_CHECK_CMD \
    PORTAGE_POSTGRES_PITR_CMD PORTAGE_POSTGRES_BACKUP_REPO \
    PORTAGE_MIGRATE_BIN \
    PORTAGE_PITR_ASSERT_RESOURCE_ID PORTAGE_PITR_ASSERT_ABSENT_RESOURCE_ID \
    PORTAGE_PITR_STATE_FILE PORTAGE_PITR_MAX_RPO_SECONDS \
    PORTAGE_PITR_MAX_RTO_SECONDS; do
    require_env "${name}"
  done
  if [[ "${PORTAGE_MIGRATE_BIN}" != /* || ! -x "${PORTAGE_MIGRATE_BIN}" ]]; then
    preflight_fail "PORTAGE_MIGRATE_BIN must be the absolute executable migration binary deployed for the drill"
  fi
  run_shell_check postgres-pgdata-fstype false "PGDATA filesystem inspection" \
    "${PORTAGE_POSTGRES_PGDATA_FSTYPE_CMD}" \
    "PGDATA filesystem type was inspected at the database host"
  if rg -qi '(^|[^a-z])(nfs|nfs4|cifs|smbfs|fuse\.sshfs)([^a-z]|$)' \
    "${logs_dir}/postgres-pgdata-fstype.log"; then
    preflight_fail "PGDATA on NFS/SMB/SSHFS is forbidden"
  fi
  run_shell_check postgres-full-backup false "pgBackRest full backup" \
    "${PORTAGE_POSTGRES_FULL_BACKUP_CMD}"
  run_shell_check postgres-diff-backup false "pgBackRest differential backup" \
    "${PORTAGE_POSTGRES_DIFF_BACKUP_CMD}"
  run_shell_check postgres-pitr-markers true "PITR before/after marker preparation" \
    "${PORTAGE_POSTGRES_PITR_PREPARE_CMD}" \
    "durable and post-target audit markers bracket the selected PITR time"
  run_shell_check postgres-wal-archive false "WAL archive continuity" \
    "${PORTAGE_POSTGRES_WAL_CHECK_CMD}"
  run_argv_check postgres-backup-size false "backup repository size" \
    "backup repository byte size recorded" du -sk "${PORTAGE_POSTGRES_BACKUP_REPO}"
  run_shell_check postgres-pitr true "authoritative-schema PITR restore and integrity checks" \
    "${PORTAGE_POSTGRES_PITR_CMD}" \
    "PITR restored and checked schema/job/attempt/signing/issuer/capacity/target/roles with RPO/RTO"
}

run_object() {
  local name
  for name in PORTAGE_OBJECT_READ_POLICY PORTAGE_OBJECT_EXECUTOR_POLICY \
    PORTAGE_OBJECT_SIGNER_POLICY PORTAGE_OBJECT_QUARANTINE_GC_POLICY \
    PORTAGE_OBJECT_GENERATION_GC_POLICY PORTAGE_OBJECT_READ_PRINCIPAL \
    PORTAGE_OBJECT_EXECUTOR_PRINCIPAL PORTAGE_OBJECT_SIGNER_PRINCIPAL \
    PORTAGE_OBJECT_QUARANTINE_GC_PRINCIPAL PORTAGE_OBJECT_GENERATION_GC_PRINCIPAL \
    PORTAGE_OBJECT_SOURCE_FAULT_DOMAIN PORTAGE_OBJECT_REPLICA_FAULT_DOMAIN; do
    require_env "${name}"
  done
  for name in PORTAGE_OBJECT_READ_POLICY PORTAGE_OBJECT_EXECUTOR_POLICY \
    PORTAGE_OBJECT_SIGNER_POLICY PORTAGE_OBJECT_QUARANTINE_GC_POLICY \
    PORTAGE_OBJECT_GENERATION_GC_POLICY; do
    require_file "${name}"
  done
  if [[ "${PORTAGE_OBJECT_SOURCE_FAULT_DOMAIN}" == "${PORTAGE_OBJECT_REPLICA_FAULT_DOMAIN}" ]]; then
    preflight_fail "object replica must use a different failure domain"
  fi
  run_argv_check object-iam-split false "validate exported object IAM policies" \
    "API has no DeleteObject; read/executor/signer and two GC authorities are distinct" \
    python3 "${validator}" object-policies \
    --read-policy "${PORTAGE_OBJECT_READ_POLICY}" \
    --executor-policy "${PORTAGE_OBJECT_EXECUTOR_POLICY}" \
    --signer-policy "${PORTAGE_OBJECT_SIGNER_POLICY}" \
    --quarantine-gc-policy "${PORTAGE_OBJECT_QUARANTINE_GC_POLICY}" \
    --generation-gc-policy "${PORTAGE_OBJECT_GENERATION_GC_POLICY}" \
    --read-principal "${PORTAGE_OBJECT_READ_PRINCIPAL}" \
    --executor-principal "${PORTAGE_OBJECT_EXECUTOR_PRINCIPAL}" \
    --signer-principal "${PORTAGE_OBJECT_SIGNER_PRINCIPAL}" \
    --quarantine-gc-principal "${PORTAGE_OBJECT_QUARANTINE_GC_PRINCIPAL}" \
    --generation-gc-principal "${PORTAGE_OBJECT_GENERATION_GC_PRINCIPAL}"
  for name in PORTAGE_OBJECT_IMMUTABLE_CAS_CMD PORTAGE_OBJECT_SOURCE_AUDIT_CMD \
    PORTAGE_OBJECT_REPLICATION_CMD PORTAGE_OBJECT_REPLICA_AUDIT_CMD \
    PORTAGE_OBJECT_RESTORE_CMD PORTAGE_OBJECT_ROLLBACK_CMD \
    PORTAGE_OBJECT_DEEP_AUDIT_CMD PORTAGE_OBJECT_QUARANTINE_GC_CMD \
    PORTAGE_OBJECT_GENERATION_GC_CMD PORTAGE_OBJECT_REFERENCE_GC_CHECK_CMD; do
    require_env "${name}"
  done
  run_shell_check object-immutable-cas true "immutable write and stale CAS rejection" "${PORTAGE_OBJECT_IMMUTABLE_CAS_CMD}"
  run_shell_check object-source-audit false "source deep audit" "${PORTAGE_OBJECT_SOURCE_AUDIT_CMD}"
  run_shell_check object-replication true "cross-failure-domain replication" "${PORTAGE_OBJECT_REPLICATION_CMD}"
  run_shell_check object-replica-audit false "replica deep audit" "${PORTAGE_OBJECT_REPLICA_AUDIT_CMD}"
  run_shell_check object-restore true "stable pointer/manifest/Packages/artifact restore" "${PORTAGE_OBJECT_RESTORE_CMD}"
  run_shell_check object-rollback true "CAS rollback" "${PORTAGE_OBJECT_ROLLBACK_CMD}"
  run_shell_check object-deep-audit false "post-rollback deep audit" "${PORTAGE_OBJECT_DEEP_AUDIT_CMD}"
  run_shell_check object-quarantine-gc true "quarantine delete authority" "${PORTAGE_OBJECT_QUARANTINE_GC_CMD}"
  run_shell_check object-generation-gc true "generation GC authority" "${PORTAGE_OBJECT_GENERATION_GC_CMD}"
  run_shell_check object-reference-aware-gc false "active/rollback/reference closure" "${PORTAGE_OBJECT_REFERENCE_GC_CHECK_CMD}"
}

run_signer() {
  local name old_key_upper new_key_upper
  for name in PORTAGE_SIGNER_BACKUP_OFFLINE PORTAGE_SIGNER_CUSTODIANS \
    PORTAGE_SIGNER_BACKUP_PATH PORTAGE_SIGNER_SOURCE_GNUPGHOME \
    PORTAGE_SIGNER_RESTORE_GNUPGHOME PORTAGE_SIGNER_OLD_KEY_ID \
    PORTAGE_SIGNER_NEW_KEY_ID PORTAGE_SIGNER_BACKUP_CMD \
    PORTAGE_SIGNER_RESTORE_CMD PORTAGE_SIGNER_OLD_VERIFY_CMD \
    PORTAGE_SIGNER_NEW_VERIFY_CMD PORTAGE_SIGNER_OLD_GENERATION_CMD \
    PORTAGE_SIGNER_ROLLBACK_CMD PORTAGE_SIGNER_BUILDER_NO_KEY_CMD \
    PORTAGE_SIGNER_API_NO_KEY_CMD PORTAGE_SIGNER_DASHBOARD_NO_KEY_CMD; do
    require_env "${name}"
  done
  if [[ "${PORTAGE_SIGNER_BACKUP_OFFLINE}" != "true" ]]; then
    preflight_fail "PORTAGE_SIGNER_BACKUP_OFFLINE must be exactly true"
  fi
  validate_owner_list PORTAGE_SIGNER_CUSTODIANS "${PORTAGE_SIGNER_CUSTODIANS}"
  old_key_upper="$(printf '%s' "${PORTAGE_SIGNER_OLD_KEY_ID}" | tr '[:lower:]' '[:upper:]')"
  new_key_upper="$(printf '%s' "${PORTAGE_SIGNER_NEW_KEY_ID}" | tr '[:lower:]' '[:upper:]')"
  if [[ ! "${PORTAGE_SIGNER_OLD_KEY_ID}" =~ ^([A-Fa-f0-9]{16}|[A-Fa-f0-9]{40})$ ]] ||
    [[ ! "${PORTAGE_SIGNER_NEW_KEY_ID}" =~ ^([A-Fa-f0-9]{16}|[A-Fa-f0-9]{40})$ ]] ||
    [[ "${old_key_upper}" == "${new_key_upper}" ]]; then
    preflight_fail "old and new signer key IDs must be distinct 16- or 40-hex identifiers"
  fi
  if [[ "${PORTAGE_SIGNER_RESTORE_GNUPGHOME}" != *"${PORTAGE_DRILL_TARGET}"* ]] ||
    [[ "${PORTAGE_SIGNER_RESTORE_GNUPGHOME}" == "${PORTAGE_SIGNER_SOURCE_GNUPGHOME}" ]]; then
    preflight_fail "signer restore GNUPGHOME must be distinct and contain the isolated target name"
  fi
  run_shell_check signer-encrypted-backup true "offline encrypted secret-key backup" \
    "${PORTAGE_SIGNER_BACKUP_CMD}"
  require_file PORTAGE_SIGNER_BACKUP_PATH
  local packet_log="${logs_dir}/signer-backup-packets.log"
  set +e
  gpg --batch --list-packets "${PORTAGE_SIGNER_BACKUP_PATH}" >"${packet_log}" 2>&1
  set -e
  chmod 0600 "${packet_log}"
  run_argv_check signer-backup-envelope false "validate encrypted signer backup" \
    "backup is an encrypted GPG envelope, not a plaintext private key" \
    python3 "${validator}" encrypted-backup --file "${PORTAGE_SIGNER_BACKUP_PATH}" \
    --packet-log "${packet_log}"
  run_shell_check signer-isolated-restore true "isolated signer key restore" \
    "${PORTAGE_SIGNER_RESTORE_CMD}"
  run_argv_check signer-restored-secret-keys false "list isolated restored secret keys" \
    "isolated GNUPGHOME contains both transition keys" gpg --batch \
    --homedir "${PORTAGE_SIGNER_RESTORE_GNUPGHOME}" --with-colons \
    --list-secret-keys "${PORTAGE_SIGNER_OLD_KEY_ID}" "${PORTAGE_SIGNER_NEW_KEY_ID}"
  run_shell_check signer-old-verify false "old-key verification during dual window" "${PORTAGE_SIGNER_OLD_VERIFY_CMD}"
  run_shell_check signer-new-verify false "new-key verification during dual window" "${PORTAGE_SIGNER_NEW_VERIFY_CMD}"
  run_shell_check signer-old-generation false "old generation compatibility" "${PORTAGE_SIGNER_OLD_GENERATION_CMD}"
  run_shell_check signer-rollback false "rollback generation compatibility" "${PORTAGE_SIGNER_ROLLBACK_CMD}"
  run_shell_check signer-builder-no-key false "builder has no private key" "${PORTAGE_SIGNER_BUILDER_NO_KEY_CMD}"
  run_shell_check signer-api-no-key false "API has no private key" "${PORTAGE_SIGNER_API_NO_KEY_CMD}"
  run_shell_check signer-dashboard-no-key false "Dashboard has no private key" "${PORTAGE_SIGNER_DASHBOARD_NO_KEY_CMD}"
}

command -v python3 >/dev/null 2>&1 || {
  echo "python3 is required for machine-readable drill evidence" >&2
  exit 2
}
# The PGDATA storage prohibition is a bare `if rg ...`, and set -e is suppressed
# inside an `if` condition, so a missing ripgrep (rc 127) would take the else
# branch and certify NFS-backed PGDATA as safe. Refuse the drill instead.
command -v rg >/dev/null 2>&1 ||
  preflight_fail "ripgrep is required for the repository and PGDATA storage assertions"

case "${gate}" in
  static)
    run_static
    ;;
  vault|postgres|object|signer|all)
    validate_live_contract
    if [[ "${gate}" == "vault" || "${gate}" == "all" ]]; then run_vault; fi
    if [[ "${gate}" == "postgres" || "${gate}" == "all" ]]; then run_postgres; fi
    if [[ "${gate}" == "object" || "${gate}" == "all" ]]; then run_object; fi
    if [[ "${gate}" == "signer" || "${gate}" == "all" ]]; then run_signer; fi
    ;;
  *)
    preflight_fail "usage: $0 [static|vault|postgres|object|signer|all]"
    ;;
esac

finish
printf 'Recovery Gate status: %s\nEvidence: %s\n' \
  "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "${summary_file}")" \
  "${summary_file}"
