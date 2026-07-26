#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: STRICT_OFFLINE=1 $0 <base-systemd|desktop-verifier> <common.json> <offline-root> <inputs.lock.json> <candidate-manifest.json>" >&2
}

if [[ $# -ne 5 || ${STRICT_OFFLINE:-} != 1 ]]; then
  usage
  exit 2
fi

target=$1
common_config=$2
offline_root=$3
input_lock=$4
candidate_manifest=$5
case "$target" in
  base-systemd|desktop-verifier) ;;
  *) usage; exit 2 ;;
esac

absolute_file() {
  local path=$1
  local directory
  directory=$(cd -- "$(dirname -- "$path")" && pwd)
  printf '%s/%s\n' "$directory" "$(basename -- "$path")"
}

common_config=$(absolute_file "$common_config")
offline_root=$(cd -- "$offline_root" && pwd)
input_lock=$(absolute_file "$input_lock")
candidate_manifest=$(absolute_file "$candidate_manifest")

for command in jq python3 ssh ssh-keygen; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "required smoke command is missing: $command" >&2
    exit 1
  fi
done

factory_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$factory_dir/.." && pwd)
build_plan=${IMAGE_FACTORY_PLAN:-$factory_dir/plans/$target.build.json}
build_plan=$(absolute_file "$build_plan")
factory_bin=${IMAGE_FACTORY_BIN:-$repo_dir/bin/portage-image-factory}
terraform_bin="$offline_root/terraform/bin/terraform"
terraform_cli_config_template="$offline_root/terraform/terraform.rc"
terraform_lock="$offline_root/terraform/.terraform.lock.hcl"
ssh_key=$(jq -er '.ssh_private_key_file' "$common_config")
ssh_user=$(jq -er '.ssh_username' "$common_config")

for path in "$common_config" "$offline_root" "$input_lock" "$candidate_manifest" "$build_plan" \
  "$factory_bin" "$terraform_bin" "$terraform_cli_config_template" "$terraform_lock" "$ssh_key" "$ssh_key.pub"; do
  if [[ ! -e $path ]]; then
    echo "required smoke path is missing: $path" >&2
    exit 1
  fi
done
if [[ ! -x $factory_bin || ! -x $terraform_bin ]]; then
  echo "image-factory and Terraform binaries must be executable" >&2
  exit 1
fi

output_dir="$factory_dir/packer/output"
evidence_dir="$output_dir/$target.smoke-evidence"
mkdir -p "$evidence_dir"
rm -f "$evidence_dir"/*.log "$evidence_dir"/*.json

"$factory_bin" preflight -lock "$input_lock" -root "$offline_root" -target "$target" \
  -report "$evidence_dir/preflight.json" >/dev/null

work_dir=$(mktemp -d)
instance_name="pe-img1-${target//[^a-zA-Z0-9]/-}-$(date -u +%Y%m%d%H%M%S)"
cp "$terraform_lock" "$work_dir/.terraform.lock.hcl"
terraform_cli_config="$work_dir/terraform.rc"
python3 - "$terraform_cli_config_template" "$terraform_cli_config" "$offline_root/terraform/providers" <<'PY'
import pathlib
import sys

source, output, provider_dir = map(pathlib.Path, sys.argv[1:])
placeholder = "__PORTAGE_ENGINE_OFFLINE_TERRAFORM_PROVIDERS__"
text = source.read_text(encoding="utf-8")
if text.count(placeholder) != 1:
    raise SystemExit("locked Terraform CLI config must contain exactly one provider-mirror placeholder")
provider_dir = provider_dir.resolve(strict=True)
if not provider_dir.is_dir():
    raise SystemExit("locked Terraform provider mirror is not a directory")
output.write_text(text.replace(placeholder, str(provider_dir)), encoding="utf-8")
output.chmod(0o600)
PY

"$factory_bin" smoke-config -common "$common_config" -plan "$build_plan" \
  -manifest "$candidate_manifest" -lock "$input_lock" -name "$instance_name" -output "$work_dir/main.tf"

export CHECKPOINT_DISABLE=1
export TF_IN_AUTOMATION=1
export TF_INPUT=0
export TF_CLI_CONFIG_FILE="$terraform_cli_config"
export TF_VAR_pve_token_secret=${PKR_VAR_proxmox_token:?PKR_VAR_proxmox_token is required}
apply_started=0

cleanup() {
  local original_status=$?
  local destroy_status=0
  trap - EXIT
  if [[ $original_status -ne 0 && $apply_started -eq 0 ]]; then
    printf 'Terraform failed before apply; no managed resources were created.\n' >"$evidence_dir/pre-apply-cleanup.log"
    rm -r "$work_dir"
    exit "$original_status"
  fi
  set +e
  "$terraform_bin" -chdir="$work_dir" destroy -auto-approve >"$evidence_dir/terraform-destroy.log" 2>&1
  destroy_status=$?
  if [[ $destroy_status -eq 0 ]]; then
    rm -r "$work_dir"
    if [[ -f $evidence_dir/result.json ]]; then
      jq '. + {"terraform_destroyed": true}' "$evidence_dir/result.json" >"$evidence_dir/result.json.tmp"
      mv "$evidence_dir/result.json.tmp" "$evidence_dir/result.json"
    fi
  else
    echo "Terraform recovery directory retained at $work_dir" >&2
  fi
  if [[ $original_status -eq 0 && $destroy_status -ne 0 ]]; then
    echo "Terraform smoke passed but destroy failed; manual cleanup is required" >&2
    exit "$destroy_status"
  fi
  exit "$original_status"
}
trap cleanup EXIT

"$terraform_bin" -chdir="$work_dir" init -lockfile=readonly >"$evidence_dir/terraform-init.log" 2>&1
"$terraform_bin" -chdir="$work_dir" validate >"$evidence_dir/terraform-validate.log" 2>&1
apply_started=1
"$terraform_bin" -chdir="$work_dir" apply -auto-approve >"$evidence_dir/terraform-apply.log" 2>&1

vmid=$("$terraform_bin" -chdir="$work_dir" output -raw vmid)
node=$("$terraform_bin" -chdir="$work_dir" output -raw node)
guest_ip=$("$factory_bin" guest-ip -common "$common_config" -node "$node" -vmid "$vmid" -timeout 600)
guest_host_key=$("$factory_bin" guest-host-key -common "$common_config" -node "$node" -vmid "$vmid" -timeout 120)
printf '%s %s\n' "$guest_ip" "$guest_host_key" >"$work_dir/known_hosts"
chmod 0600 "$work_dir/known_hosts"
ssh-keygen -lf "$work_dir/known_hosts" >"$evidence_dir/guest-ssh-host-key.log"

ssh_options=(
  -i "$ssh_key"
  -o BatchMode=yes
  -o IdentitiesOnly=yes
  -o StrictHostKeyChecking=yes
  -o UserKnownHostsFile="$work_dir/known_hosts"
  -o ConnectTimeout=20
)

ssh "${ssh_options[@]}" "$ssh_user@$guest_ip" '
  set -e
  cloud_init_gate() {
    status_file=$(mktemp)
    status_rc=0
    cloud-init status --wait --format json >"$status_file" || status_rc=$?
    if [ "$status_rc" -ne 0 ] && [ "$status_rc" -ne 2 ]; then
      cat "$status_file"
      return "$status_rc"
    fi
    python3 - "$status_file" <<PY
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    status = json.load(handle)
if status.get("status") != "done" or status.get("errors"):
    raise SystemExit("cloud-init did not finish without hard errors")
allowed = (
    chr(39) + "user" + chr(39) + " of type string is deprecated in 22.2 "
    "and scheduled to be removed in 27.2. Use " + chr(39) + "users" + chr(39) + " list instead."
)
unexpected = [
    message
    for messages in status.get("recoverable_errors", {}).values()
    for message in messages
    if message != allowed
]
if unexpected:
    raise SystemExit("unexpected cloud-init recoverable errors: " + repr(unexpected))
PY
    cat "$status_file"
    rm -f "$status_file"
  }
  cloud_init_gate
  printf "CLOUD_INIT_GATE=PASS\n"
  ! grep -F "emerge --quiet --sync" /var/log/cloud-init.log /var/log/cloud-init-output.log
  printf "IMPLICIT_EMERGE_SYNC=ABSENT\n"
  systemctl is-enabled qemu-guest-agent.service
  test -s /etc/portage-engine/build-plan.json
  test -s /etc/portage-engine/image-build.json
  test -s /etc/portage/sets/portage-engine-image
  emerge --pretend --verbose --update --deep --newuse --with-bdeps=y @world
  emerge --pretend --verbose --update --deep --newuse --with-bdeps=y @portage-engine-image
  emerge --verbose --oneshot app-misc/hello
' >"$evidence_dir/guest-first-boot.log" 2>&1

ssh "${ssh_options[@]}" "$ssh_user@$guest_ip" '
  set -e
  cloud_init_gate() {
    status_file=$(mktemp)
    status_rc=0
    cloud-init status --wait --format json >"$status_file" || status_rc=$?
    if [ "$status_rc" -ne 0 ] && [ "$status_rc" -ne 2 ]; then
      cat "$status_file"
      return "$status_rc"
    fi
    python3 - "$status_file" <<PY
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    status = json.load(handle)
if status.get("status") != "done" or status.get("errors"):
    raise SystemExit("cloud-init did not finish without hard errors")
allowed = (
    chr(39) + "user" + chr(39) + " of type string is deprecated in 22.2 "
    "and scheduled to be removed in 27.2. Use " + chr(39) + "users" + chr(39) + " list instead."
)
unexpected = [
    message
    for messages in status.get("recoverable_errors", {}).values()
    for message in messages
    if message != allowed
]
if unexpected:
    raise SystemExit("unexpected cloud-init recoverable errors: " + repr(unexpected))
PY
    cat "$status_file"
    rm -f "$status_file"
  }
  cloud-init clean --logs
  cloud-init init --local
  cloud-init init
  cloud-init modules --mode=config
  cloud-init modules --mode=final
  cloud_init_gate
  printf "SECOND_CLOUD_INIT_GATE=PASS\n"
  ! grep -F "emerge --quiet --sync" /var/log/cloud-init.log /var/log/cloud-init-output.log
  printf "IMPLICIT_EMERGE_SYNC=ABSENT\n"
  systemctl is-active qemu-guest-agent.service
  printf "SECOND_RUN_GATE=PASS\n"
' >"$evidence_dir/guest-cloud-init-second-run.log" 2>&1

if [[ $target == desktop-verifier ]]; then
  ssh "${ssh_options[@]}" "$ssh_user@$guest_ip" '
    set -e
    systemctl is-enabled display-manager.service
    command -v Xorg
    command -v Xvfb
    command -v startxfce4
    timeout 20s Xvfb :99 -screen 0 1280x720x24 &
    xvfb_pid=$!
    sleep 2
    DISPLAY=:99 xmessage -timeout 2 portage-engine-img1
    wait "$xvfb_pid" || test $? -eq 124
  ' >"$evidence_dir/guest-desktop-smoke.log" 2>&1
fi

cat >"$evidence_dir/result.json" <<EOF
{
  "schema_version": 1,
  "target": "$target",
  "candidate_manifest": "$(basename -- "$candidate_manifest")",
  "instance_name": "$instance_name",
  "vmid": "$vmid",
  "node": "$node",
  "guest_ip": "$guest_ip",
  "cloud_init_runs": 2,
  "terraform_destroy_required": true,
  "completed_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

set +e
"$terraform_bin" -chdir="$work_dir" destroy -auto-approve >"$evidence_dir/terraform-destroy.log" 2>&1
destroy_status=$?
set -e
if [[ $destroy_status -ne 0 ]]; then
  echo "Terraform destroy failed; promotion is blocked" >&2
  exit "$destroy_status"
fi
trap - EXIT
rm -r "$work_dir"
jq '. + {"terraform_destroyed": true}' "$evidence_dir/result.json" >"$evidence_dir/result.json.tmp"
mv "$evidence_dir/result.json.tmp" "$evidence_dir/result.json"

"$factory_bin" stamp-output -common "$common_config" -plan "$build_plan" -lock "$input_lock" -manifest "$candidate_manifest" \
  -evidence-output "$evidence_dir/output-stamp.json"
jq '. + {"output_provenance_stamped": true}' "$evidence_dir/result.json" >"$evidence_dir/result.json.tmp"
mv "$evidence_dir/result.json.tmp" "$evidence_dir/result.json"
echo "smoke evidence: $evidence_dir"
