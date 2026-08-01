#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly repo_root
test_root="$(mktemp -d "${TMPDIR:-/tmp}/portage-recovery-test.XXXXXX")"
readonly test_root
cleanup() {
  rm -rf "${test_root}"
}
trap cleanup EXIT
cd "${repo_root}"

fail() {
  echo "recovery drill test failed: $1" >&2
  exit 1
}

static_evidence="${test_root}/static"
PORTAGE_DRILL_EVIDENCE_DIR="${static_evidence}" \
  scripts/public-beta-recovery-drill.sh static >/dev/null
python3 - "${static_evidence}/summary.json" <<'PY'
import json
import sys

summary = json.load(open(sys.argv[1], encoding="utf-8"))
assert summary["status"] == "passed", summary
assert summary["live_gate_status"] == "not-run", summary
live = [check for check in summary["checks"] if check["check_id"].endswith("-live")]
assert len(live) == 4, live
assert {check["status"] for check in live} == {"not-run"}, live
assert summary["counts"]["failed"] == 0, summary
PY

production_evidence="${test_root}/production-refusal"
if PORTAGE_DRILL_EVIDENCE_DIR="${production_evidence}" \
  PORTAGE_DRILL_OWNER="operator@example.test" \
  PORTAGE_DRILL_TARGET="production-recovery-drill" \
  PORTAGE_DRILL_ISOLATED=true \
  PORTAGE_DRILL_CONFIRM=RUN_PUBLIC_BETA_RECOVERY_DRILL \
  PORTAGE_DRILL_DESTRUCTIVE_CONFIRM=DESTROY_ISOLATED_DRILL_TARGET_ONLY \
  PORTAGE_DRILL_ISOLATION_CHECK_CMD="touch '${test_root}/unsafe-isolation-ran'" \
  scripts/public-beta-recovery-drill.sh postgres >/dev/null 2>&1; then
  fail "production-like target was accepted"
fi
[[ ! -e "${test_root}/unsafe-isolation-ran" ]] ||
  fail "isolation command ran for a refused production target"
python3 - "${production_evidence}/summary.json" <<'PY'
import json
import sys
summary = json.load(open(sys.argv[1], encoding="utf-8"))
assert summary["status"] == "failed", summary
assert summary["checks"][0]["check_id"] == "preflight", summary
PY

python3 scripts/recovery/validate.py vault-status \
  --file tests/fixtures/recovery/vault-ha-status.json >/dev/null
python3 scripts/recovery/validate.py vault-role \
  --file tests/fixtures/recovery/vault-minimal-role.json \
  --allowed-uri-san 'spiffe://portage-engine/*' \
  --allowed-common-name portage-worker >/dev/null
sed 's/"allow_any_name": false/"allow_any_name": true/' \
  tests/fixtures/recovery/vault-minimal-role.json >"${test_root}/unsafe-vault-role.json"
if python3 scripts/recovery/validate.py vault-role \
  --file "${test_root}/unsafe-vault-role.json" \
  --allowed-uri-san 'spiffe://portage-engine/*' \
  --allowed-common-name portage-worker >/dev/null 2>&1; then
  fail "unsafe Vault role was accepted"
fi

sed 's/"s3:GetObject"/"s3:DeleteObject"/' \
  tests/fixtures/recovery/object-read-policy.json >"${test_root}/unsafe-read-policy.json"
if python3 scripts/recovery/validate.py object-policies \
  --read-policy "${test_root}/unsafe-read-policy.json" \
  --executor-policy tests/fixtures/recovery/object-executor-policy.json \
  --signer-policy tests/fixtures/recovery/object-signer-policy.json \
  --quarantine-gc-policy tests/fixtures/recovery/object-quarantine-gc-policy.json \
  --generation-gc-policy tests/fixtures/recovery/object-generation-gc-policy.json \
  --read-principal read --executor-principal executor --signer-principal signer \
  --quarantine-gc-principal qgc --generation-gc-principal ggc >/dev/null 2>&1; then
  fail "API/read DeleteObject policy was accepted"
fi

if scripts/pgbackrest-restore-drill.sh '2026-08-01T00:00:00Z' >/dev/null 2>&1; then
  fail "pgBackRest drill ran without isolation confirmation"
fi
if scripts/pgbackrest-pitr-prepare.sh >/dev/null 2>&1; then
  fail "PITR marker preparation ran without isolation confirmation"
fi
marker_evidence="${test_root}/marker-evidence"
mkdir -p "${marker_evidence}"
PATH="${repo_root}/tests/fixtures/recovery/fake-bin:${PATH}" \
PORTAGE_DRILL_OWNER='operator@example.test' \
PORTAGE_DRILL_TARGET='postgres-recovery-drill' \
PORTAGE_DRILL_ISOLATED=true \
PORTAGE_DRILL_CONFIRM=RUN_PUBLIC_BETA_RECOVERY_DRILL \
PORTAGE_DRILL_DESTRUCTIVE_CONFIRM=DESTROY_ISOLATED_DRILL_TARGET_ONLY \
PORTAGE_DRILL_EVIDENCE_DIR="${marker_evidence}" \
PORTAGE_PITR_ASSERT_RESOURCE_ID='before-marker' \
PORTAGE_PITR_ASSERT_ABSENT_RESOURCE_ID='after-marker' \
PORTAGE_PITR_STATE_FILE="${marker_evidence}/pitr-state.env" \
scripts/pgbackrest-pitr-prepare.sh >"${test_root}/marker-output.json"
set -a
# shellcheck disable=SC1091
source "${marker_evidence}/pitr-state.env"
set +a
[[ "${PORTAGE_PITR_LAST_DURABLE_UTC}" == '2026-08-01T03:00:00.000000Z' ]] ||
  fail "PITR durable marker timestamp was not preserved"
[[ "${PORTAGE_PITR_TARGET_TIME}" == '2026-08-01T03:00:01.000000Z' ]] ||
  fail "PITR target timestamp was not preserved"
python3 -m json.tool "${test_root}/marker-output.json" >/dev/null
if scripts/postgres-restore-check.sh /nonexistent.dump >/dev/null 2>&1; then
  fail "logical restore drill ran without isolation confirmation"
fi

echo "recovery drill shell tests passed"
