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

# Both gates under test refuse to run without these, so a missing one must read
# as a broken environment rather than as a failing assertion.
for required_tool in jq rg python3; do
  command -v "${required_tool}" >/dev/null 2>&1 ||
    fail "the recovery suite requires ${required_tool}"
done

future_schema="$(
  PORTAGE_MIGRATE_BIN="${repo_root}/tests/fixtures/recovery/fake-bin/portage-migrate" \
    scripts/recovery/current-schema-version.sh
)"
[[ "${future_schema}" == "29" ]] ||
  fail "recovery schema authority did not accept a future supported version"
if PORTAGE_MIGRATE_BIN="${repo_root}/tests/fixtures/recovery/fake-bin/portage-migrate-mismatch" \
  scripts/recovery/current-schema-version.sh >/dev/null 2>&1; then
  fail "mismatched binary and embedded migration schema versions were accepted"
fi

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
assert len(live) == 5, live
assert {check["status"] for check in live} == {"not-run"}, live
# The object contract check only proves three identifiers exist in a test file
# that skips itself without PORTAGE_S3_INTEGRATION=1, so it must not claim
# behaviour coverage that no gate executed.
contract = next(c for c in summary["checks"] if c["check_id"] == "object-contract")
assert "declared in the object integration source" in contract["detail"], contract
assert "object-integration-live" in {check["check_id"] for check in live}, live
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

# A database host without ripgrep must refuse the drill rather than skip the
# PGDATA NFS/SMB prohibition, whose bare `if rg ...` cannot see rc 127.
norg_bin="${test_root}/norg-bin"
mkdir -p "${norg_bin}"
for tool in bash python3 date git mkdir chmod ls cat tr sed tail wc \
  dirname basename env; do
  tool_path="$(command -v "${tool}" || true)"
  [[ -n "${tool_path}" ]] && ln -sf "${tool_path}" "${norg_bin}/${tool}"
done
norg_evidence="${test_root}/norg-evidence"
if PATH="${norg_bin}" \
  PORTAGE_DRILL_EVIDENCE_DIR="${norg_evidence}" \
  PORTAGE_DRILL_OWNER='operator@example.test' \
  PORTAGE_DRILL_TARGET='postgres-recovery-drill' \
  PORTAGE_DRILL_ISOLATED=true \
  PORTAGE_DRILL_CONFIRM=RUN_PUBLIC_BETA_RECOVERY_DRILL \
  PORTAGE_DRILL_DESTRUCTIVE_CONFIRM=DESTROY_ISOLATED_DRILL_TARGET_ONLY \
  scripts/public-beta-recovery-drill.sh postgres >/dev/null 2>&1; then
  fail "postgres drill ran on a host without ripgrep"
fi
python3 - "${norg_evidence}/summary.json" <<'PY'
import json
import sys

summary = json.load(open(sys.argv[1], encoding="utf-8"))
assert summary["status"] == "failed", summary
preflight = summary["checks"][0]
assert preflight["check_id"] == "preflight", summary
assert "ripgrep" in preflight["detail"], summary
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

# A Deny is not a grant and a grant is not a Deny: the validator must read the
# two effects separately in both directions.
if python3 scripts/recovery/validate.py object-policies \
  --read-policy tests/fixtures/recovery/object-read-policy.json \
  --executor-policy tests/fixtures/recovery/object-executor-policy.json \
  --signer-policy tests/fixtures/recovery/object-signer-denied-put-policy.json \
  --quarantine-gc-policy tests/fixtures/recovery/object-quarantine-gc-policy.json \
  --generation-gc-policy tests/fixtures/recovery/object-generation-gc-policy.json \
  --read-principal read --executor-principal executor --signer-principal signer \
  --quarantine-gc-principal qgc --generation-gc-principal ggc >/dev/null 2>&1; then
  fail "signer policy that denies its own PutObject was accepted"
fi
python3 scripts/recovery/validate.py object-policies \
  --read-policy tests/fixtures/recovery/object-read-denied-write-policy.json \
  --executor-policy tests/fixtures/recovery/object-executor-policy.json \
  --signer-policy tests/fixtures/recovery/object-signer-policy.json \
  --quarantine-gc-policy tests/fixtures/recovery/object-quarantine-gc-policy.json \
  --generation-gc-policy tests/fixtures/recovery/object-generation-gc-policy.json \
  --read-principal read --executor-principal executor --signer-principal signer \
  --quarantine-gc-principal qgc --generation-gc-principal ggc >/dev/null ||
  fail "read policy narrowed with an explicit write/delete Deny was rejected"

# The edge template's own header promises OAuth query strings never reach the
# access log, so prove the assertion that enforces it can go red. docker is kept
# off PATH: the container-dependent checks then record not-run instead of
# pulling an image, and the static verdict is read from the manifest.
edge_bin="${test_root}/edge-bin"
mkdir -p "${edge_bin}"
for tool in bash jq rg awk sed git date mktemp mkdir dirname rm; do
  tool_path="$(command -v "${tool}" || true)"
  [[ -n "${tool_path}" ]] && ln -sf "${tool_path}" "${edge_bin}/${tool}"
done
edge_status=0
PATH="${edge_bin}" scripts/validate-public-edge.sh \
  --output "${test_root}/edge-baseline.json" >/dev/null 2>&1 || edge_status=$?
[[ "${edge_status}" -eq 3 ]] ||
  fail "public edge gate did not report not-run without docker (exit ${edge_status})"

# Mirror the repository so only the nginx template differs; the gate resolves
# every input from the directory above its own scripts/ directory.
edge_mirror="${test_root}/edge-mirror"
mkdir -p "${edge_mirror}/deploy/public-edge"
for entry in "${repo_root}"/* "${repo_root}"/.git; do
  name="$(basename "${entry}")"
  [[ "${name}" == deploy ]] || ln -sf "${entry}" "${edge_mirror}/${name}"
done
for entry in "${repo_root}"/deploy/*; do
  name="$(basename "${entry}")"
  [[ "${name}" == public-edge ]] || ln -sf "${entry}" "${edge_mirror}/deploy/${name}"
done
for entry in "${repo_root}"/deploy/public-edge/*; do
  name="$(basename "${entry}")"
  [[ "${name}" == nginx.conf.template ]] ||
    ln -sf "${entry}" "${edge_mirror}/deploy/public-edge/${name}"
done
sed 's/rt=\$request_time/rt=$request_time uri=$request_uri/' \
  "${repo_root}/deploy/public-edge/nginx.conf.template" \
  >"${edge_mirror}/deploy/public-edge/nginx.conf.template"
edge_status=0
PATH="${edge_bin}" "${edge_mirror}/scripts/validate-public-edge.sh" \
  --output "${test_root}/edge-injected.json" >/dev/null 2>&1 || edge_status=$?
[[ "${edge_status}" -eq 1 ]] ||
  fail "public edge gate accepted \$request_uri in the access log (exit ${edge_status})"
python3 - "${test_root}/edge-baseline.json" "${test_root}/edge-injected.json" <<'PY'
import json
import sys


def status(path, check_id):
    manifest = json.load(open(path, encoding="utf-8"))
    return {check["id"]: check["status"] for check in manifest["checks"]}[check_id]


assert status(sys.argv[1], "edge_static_policy") == "pass", sys.argv[1]
assert status(sys.argv[2], "edge_static_policy") == "fail", sys.argv[2]
PY

if scripts/pgbackrest-restore-drill.sh '2026-08-01T00:00:00Z' >/dev/null 2>&1; then
  fail "pgBackRest drill ran without isolation confirmation"
fi
normalized_target="$(
  python3 scripts/recovery/pgbackrest-target-time.py \
    '2026-08-01T03:00:01.123456Z'
)"
[[ "${normalized_target}" == '2026-08-01 03:00:01.123456+00' ]] ||
  fail "pgBackRest target time did not normalize the PostgreSQL T/Z spelling"
normalized_offset_target="$(
  python3 scripts/recovery/pgbackrest-target-time.py \
    '2026-08-01T11:00:01.123456+08:00'
)"
[[ "${normalized_offset_target}" == '2026-08-01 03:00:01.123456+00' ]] ||
  fail "pgBackRest target time did not normalize an explicit UTC offset"
if python3 scripts/recovery/pgbackrest-target-time.py \
  '2026-08-01T03:00:01.123456' >/dev/null 2>&1; then
  fail "pgBackRest target time accepted a timezone-free timestamp"
fi
restore_filesystem="$(
  python3 scripts/recovery/filesystem-type.py "${test_root}"
)"
[[ -n "${restore_filesystem}" && "${restore_filesystem}" != '/' && \
  "${restore_filesystem}" != '@' ]] ||
  fail "restore filesystem detection returned a file-type placeholder"
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
python3 - "${test_root}/marker-output.json" <<'PY'
import json
import sys

payload = json.load(open(sys.argv[1], encoding="utf-8"))
counts = payload["fixture"]["row_counts"]
assert counts, payload
assert all(value == 1 for value in counts.values()), counts
PY
rg -q "restore has no recovery lineage" \
  scripts/recovery/schema-current-restore-check.sql ||
  fail "current-schema restore check does not reject an empty business lineage"
if scripts/postgres-restore-check.sh /nonexistent.dump >/dev/null 2>&1; then
  fail "logical restore drill ran without isolation confirmation"
fi

echo "recovery drill shell tests passed"
