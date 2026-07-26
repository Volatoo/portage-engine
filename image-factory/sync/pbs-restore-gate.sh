#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 first|runtime|second" >&2
  exit 2
fi

mode=$1

cloud_init_status() {
  local output status_code
  set +e
  output=$(cloud-init status --format=json --long 2>&1)
  status_code=$?
  set -e
  if (( status_code != 0 && status_code != 2 )); then
    printf '%s\n' "${output}" >&2
    return "${status_code}"
  fi
  printf '%s\n' "${output}"
}

validate_status() {
  python3 -c '
import json
import sys

allowed = "\x27user\x27 of type string is deprecated in 22.2 and scheduled to be removed in 27.2. Use \x27users\x27 list instead."
data = json.load(sys.stdin)
if data.get("status") != "done":
    raise SystemExit("cloud-init status is not done")
if data.get("errors"):
    raise SystemExit("cloud-init errors: {!r}".format(data["errors"]))
for stage in ("init-local", "init", "modules-config", "modules-final"):
    details = data.get(stage) or {}
    if details.get("errors"):
        raise SystemExit("cloud-init {} errors: {!r}".format(stage, details["errors"]))
for where, recoverable in (("top-level", data.get("recoverable_errors") or {}), *(
    (stage, (data.get(stage) or {}).get("recoverable_errors") or {})
    for stage in ("init-local", "init", "modules-config", "modules-final")
)):
    for category, messages in recoverable.items():
        if category != "DEPRECATED" or any(message != allowed for message in messages):
            raise SystemExit(f"unexpected recoverable cloud-init error in {where}: {category}={messages!r}")
'
}

assert_no_implicit_sync() {
  if grep -R -F -- 'emerge --quiet --sync' /var/log/cloud-init.log /var/log/cloud-init-output.log 2>/dev/null; then
    echo "IMPLICIT_EMERGE_SYNC=FOUND"
    return 1
  fi
  echo "IMPLICIT_EMERGE_SYNC=ABSENT"
}

case ${mode} in
  first)
    status_json=$(cloud_init_status)
    validate_status <<<"${status_json}"
    echo "CLOUD_INIT_GATE=PASS"
    printf '%s\n' "${status_json}"
    assert_no_implicit_sync
    ;;
  runtime)
    printf 'CLOUD_INIT_LOCAL=%s\n' "$(systemctl is-active cloud-init-local.service)"
    printf 'CLOUD_INIT_NETWORK=%s\n' "$(systemctl is-active cloud-init-network.service)"
    printf 'CLOUD_CONFIG=%s\n' "$(systemctl is-active cloud-config.service)"
    printf 'CLOUD_FINAL=%s\n' "$(systemctl is-active cloud-final.service)"
    printf 'QEMU_GUEST_AGENT=%s\n' "$(systemctl is-active qemu-guest-agent.service)"
    . /etc/os-release
    printf 'OS=%s\n' "${PRETTY_NAME}"
    printf 'KERNEL=%s\n' "$(uname -r)"
    printf 'PROFILE=%s\n' "$(eselect profile show | tail -n 1 | sed 's/^[[:space:]]*//')"
    printf 'ROOT_FS=%s\n' "$(findmnt -n -o FSTYPE,SOURCE /)"
    emerge --info | sed -n '1p'
    command -v cloud-init
    command -v qemu-ga
    command -v emerge
    command -v python3
    ;;
  second)
    status_json=$(cloud_init_status)
    validate_status <<<"${status_json}"
    echo "SECOND_CLOUD_INIT_GATE=PASS"
    printf '%s\n' "${status_json}"
    systemctl is-active cloud-final.service
    assert_no_implicit_sync
    echo "SECOND_RUN_GATE=PASS"
    ;;
  *)
    echo "unknown mode: ${mode}" >&2
    exit 2
    ;;
esac
