#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
mode=${1:-repo}

skip_live() {
  echo "SKIP live PVE SCHED-2B Gate: $*" >&2
  exit 2
}

run_repo_gate() {
  cd "$repo_root"
  bash -n \
    scripts/capacity-executor-identity.sh \
    image-factory/persistent-executor/run.sh \
    image-factory/persistent-executor/provision.sh \
    image-factory/persistent-executor/sanitize-and-gate.sh

  grep -Fq 'ConditionPathExists=/etc/portage-engine/executor.conf' \
    deploy/systemd/portage-capacity-executor.service
  grep -Fq 'ExecStartPre=/usr/local/libexec/portage-engine/capacity-executor-identity' \
    deploy/systemd/portage-capacity-executor.service
  grep -Fq 'EnvironmentFile=-/run/portage-engine/capacity-executor.env' \
    deploy/systemd/portage-capacity-executor.service
  if grep -Eq 'Listen(Stream|Datagram)|WORKER_GATEWAY_TLS_KEY' \
    deploy/systemd/portage-capacity-executor.service \
    image-factory/persistent-executor/provision.sh; then
    echo "repository Gate failed: executor template contains a listener credential or socket" >&2
    return 1
  fi

  shell_pool="pve-zone-a-amd64-$(
    printf '%s\0%s\0%s\0%s\0%s\0%s\0%s' \
      pve zone-a amd64 native-gentoo pe/amd64/base-v1 pe/amd64/base g17 |
      { sha256sum 2>/dev/null || shasum -a 256; } |
      awk '{print substr($1, 1, 24)}'
  )"
  [[ $shell_pool == pve-zone-a-amd64-db23fcaaaeb71219f0511397 ]] || {
    echo "repository Gate failed: shell capacity pool derivation drifted" >&2
    return 1
  }

  go test ./internal/builder -run CapacityPoolIDPersistentExecutorBuildVector -count=1
  go test ./pkg/config -run 'PersistentExecutor|PublicExecutor' -count=1
  go test ./internal/capacity -run 'Actuator|PVE' -count=1
  go test ./internal/iac -run 'EnsurePVEVMAbsent' -count=1

  # Drain completion, the refusal to delete an instance that still owns live
  # work, and the exact-identity delete boundary are decided in SQL, so they only
  # execute against a real database. The case skips itself when the DSN is unset,
  # and a skipped case is a passing `go test` -- which is how this Gate used to
  # report those three boundaries as verified without running any of them. Name
  # the PASS line rather than trusting the exit status, and say not-run out loud
  # otherwise, the way scripts/public-beta-recovery-drill.sh does.
  local capacity_state=not-run
  if [[ -n ${PORTAGE_TEST_DATABASE_URL:-} ]]; then
    local capacity_log
    capacity_log=$(mktemp "${TMPDIR:-/tmp}/portage-capacity-gate.XXXXXX")
    go test ./internal/persistence \
      -run TestPostgresSchedulerFairnessAndAutoscaling -count=1 -v |
      tee "$capacity_log"
    if ! grep -q -- '--- PASS: TestPostgresSchedulerFairnessAndAutoscaling ' "$capacity_log"; then
      rm -f "$capacity_log"
      echo "repository Gate failed: capacity delete boundaries did not execute" >&2
      return 1
    fi
    rm -f "$capacity_log"
    capacity_state=pass
  fi

  if command -v packer >/dev/null 2>&1; then
    (
      cd image-factory/persistent-executor
      packer fmt -check template.pkr.hcl
      packer validate -syntax-only template.pkr.hcl
    )
  else
    echo "SKIP Packer syntax validation: packer is not installed" >&2
  fi

  if [[ $capacity_state == not-run ]]; then
    echo "NOT-RUN capacity delete boundaries (drain completion, live-work delete refusal, exact identity delete): set PORTAGE_TEST_DATABASE_URL"
    echo "repository persistent-executor Gate passed without the capacity delete boundaries"
  else
    echo "repository persistent-executor Gate passed"
  fi
  echo "SKIP live PVE SCHED-2B Gate: repo mode never claims an action or reports live evidence"
}

psql_scalar() {
  local sql=$1
  psql --no-psqlrc --tuples-only --no-align --set ON_ERROR_STOP=1 \
    --dbname "$PORTAGE_CAPACITY_GATE_DATABASE_URL" --command "$sql"
}

run_live_once() {
  cd "$repo_root"
  [[ ${PORTAGE_CAPACITY_GATE_CONFIRM:-} == destroy-real-pve-capacity ]] ||
    skip_live "set PORTAGE_CAPACITY_GATE_CONFIRM=destroy-real-pve-capacity"
  for name in PORTAGE_CAPACITY_GATE_DATABASE_URL PORTAGE_CAPACITY_GATE_POOL_ID \
    PORTAGE_CAPACITY_GATE_KIND PORTAGE_CAPACITY_ACTUATOR_CONFIG \
    PORTAGE_CAPACITY_SERVER_CONFIG; do
    [[ -n ${!name:-} ]] || skip_live "missing ${name}"
  done
  [[ $PORTAGE_CAPACITY_GATE_KIND == scale-up ||
     $PORTAGE_CAPACITY_GATE_KIND == scale-down ]] ||
    skip_live "PORTAGE_CAPACITY_GATE_KIND must be scale-up or scale-down"
  command -v psql >/dev/null 2>&1 || skip_live "psql is not installed"
  [[ -r $PORTAGE_CAPACITY_ACTUATOR_CONFIG ]] ||
    skip_live "capacity actuator config is unreadable"
  [[ -r $PORTAGE_CAPACITY_SERVER_CONFIG ]] ||
    skip_live "server config is unreadable"
  if [[ -z ${CLOUD_PVE_TOKEN_ID:-${PM_API_TOKEN_ID:-}} ||
        -z ${CLOUD_PVE_TOKEN_SECRET:-${PM_API_TOKEN_SECRET:-}} ]]; then
    if [[ -z ${CLOUD_PVE_USERNAME:-${PM_USER:-}} ||
          -z ${CLOUD_PVE_PASSWORD:-${PM_PASS:-}} ]]; then
      skip_live "no complete PVE token or username/password credential pair is present"
    fi
  fi

  open_count=$(psql_scalar "
    SELECT count(*)
    FROM scheduler_capacity_actions
    WHERE state IN (
      'requested', 'claimed', 'waiting-for-heartbeat', 'waiting-for-drain'
    );
  ")
  [[ $open_count == 1 ]] ||
    skip_live "isolated Gate requires exactly one requested action (found ${open_count})"

  action_row=$(psql_scalar "
    SELECT id::text || '|' || action_kind
    FROM scheduler_capacity_actions
    WHERE state = 'requested';
  ")
  action_id=${action_row%%|*}
  action_kind=${action_row#*|}
  action_pool=$(psql_scalar "
    SELECT pool_id FROM scheduler_capacity_actions WHERE id = '${action_id}'::uuid;
  ")
  [[ $action_kind == "$PORTAGE_CAPACITY_GATE_KIND" ]] ||
    skip_live "the sole action is ${action_kind}, expected ${PORTAGE_CAPACITY_GATE_KIND}"
  [[ $action_pool == "$PORTAGE_CAPACITY_GATE_POOL_ID" ]] ||
    skip_live "the sole action belongs to ${action_pool}, expected ${PORTAGE_CAPACITY_GATE_POOL_ID}"

  go run ./cmd/capacity-actuator \
    -once \
    -config "$PORTAGE_CAPACITY_ACTUATOR_CONFIG" \
    -server-config "$PORTAGE_CAPACITY_SERVER_CONFIG"

  action_state=$(psql_scalar "
    SELECT state FROM scheduler_capacity_actions WHERE id = '${action_id}'::uuid;
  ")
  [[ $action_state == completed ]] || {
    echo "live Gate failed: action ${action_id} ended in ${action_state}" >&2
    return 1
  }

  if [[ $action_kind == scale-up ]]; then
    instance_row=$(psql_scalar "
      SELECT id::text || '|' || provider_instance_id || '|' || generation::text || '|' || state
      FROM scheduler_capacity_instances
      WHERE create_action_id = '${action_id}'::uuid;
    ")
    IFS='|' read -r instance_id provider_id generation instance_state <<<"$instance_row"
    [[ $provider_id == "portage-capacity-${instance_id}" &&
       $generation =~ ^[1-9][0-9]*$ && $instance_state == active ]] || {
      echo "live Gate failed: scale-up identity/generation/state readback mismatch" >&2
      return 1
    }
  else
    instance_row=$(psql_scalar "
      SELECT id::text || '|' || provider_instance_id || '|' || generation::text || '|' || state
      FROM scheduler_capacity_instances
      WHERE delete_action_id = '${action_id}'::uuid AND deleted_at IS NOT NULL;
    ")
    IFS='|' read -r instance_id provider_id generation instance_state <<<"$instance_row"
    [[ $provider_id == "portage-capacity-${instance_id}" &&
       $generation =~ ^[1-9][0-9]*$ && $instance_state == deleted ]] || {
      echo "live Gate failed: scale-down identity/generation/deletion readback mismatch" >&2
      return 1
    }
  fi

  echo "LIVE PVE SCHED-2B PASS action=${action_id} kind=${action_kind} instance=${instance_id} generation=${generation}"
}

case "$mode" in
  repo)
    run_repo_gate
    ;;
  live-once)
    run_live_once
    ;;
  *)
    echo "usage: $0 [repo|live-once]" >&2
    exit 64
    ;;
esac
