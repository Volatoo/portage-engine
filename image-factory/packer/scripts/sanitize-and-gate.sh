#!/usr/bin/env bash
set -euo pipefail

log() {
  echo "[image-gate] $*"
}

on_error() {
  local rc=$?
  echo "[image-gate] failed at line ${BASH_LINENO[0]}: ${BASH_COMMAND} (exit ${rc})" >&2
  exit "${rc}"
}
trap on_error ERR

require_command() {
  local command_name=$1
  if ! command -v "${command_name}" >/dev/null; then
    echo "required command is missing: ${command_name}" >&2
    return 1
  fi
}

IFS=',' read -r -a repository_names <<<"$PE_REPOSITORY_NAMES"
IFS=',' read -r -a repository_revisions <<<"$PE_REPOSITORY_REVISIONS"
if [[ ${#repository_names[@]} -ne ${#repository_revisions[@]} ]]; then
  echo "repository evidence vectors are inconsistent" >&2
  exit 1
fi
log "verifying pinned repositories"
for index in "${!repository_names[@]}"; do
  repo="/var/db/repos/${repository_names[$index]}"
  expected_revision=${repository_revisions[$index]}
  actual_revision=$(git -C "$repo" rev-parse HEAD)
  if [[ $actual_revision != "$expected_revision" ]]; then
    echo "repository revision drift for ${repository_names[$index]}: $actual_revision" >&2
    exit 1
  fi
  if [[ -n $(git -C "$repo" status --porcelain=v1 --untracked-files=all) ]]; then
    echo "repository worktree is dirty: ${repository_names[$index]}" >&2
    exit 1
  fi
  if [[ -n $(git -C "$repo" clean -ndx) ]]; then
    echo "repository contains ignored or generated files: ${repository_names[$index]}" >&2
    exit 1
  fi
done

resolved_profile=$(readlink -f /etc/portage/make.profile)
expected_profile=$(readlink -f "/var/db/repos/$PE_PROFILE_REPOSITORY/profiles/$PE_PROFILE_PATH")
if [[ $resolved_profile != "$expected_profile" ]]; then
  echo "make.profile mismatch: $resolved_profile" >&2
  exit 1
fi

log "verifying runtime contract"
require_command emerge
require_command cloud-init
require_command qemu-ga
require_command systemctl
test -s /etc/portage/sets/portage-engine-image
systemctl is-enabled portage-cloud-init-network-refresh.service >/dev/null
test -x /usr/local/libexec/portage-engine/cloud-init-network-refresh
grep -Fq 'ClientIdentifier=mac' \
  /usr/local/libexec/portage-engine/cloud-init-network-refresh
if [[ $PE_DESKTOP == true ]]; then
  log "verifying desktop runtime contract"
  for command_name in Xorg Xvfb startxfce4 gpg gtk-launch runuser xrandr xset scrot xdotool; do
    require_command "${command_name}"
  done
  require_command /usr/libexec/portage-desktop-agent
  python3 -c 'import gi; gi.require_version("Atspi", "2.0"); from gi.repository import Atspi'
  test "$(sha256sum /usr/share/portage-engine/desktop-fixtures/editor-fixture.txt | awk '{print $1}')" = 861bc826497b0f7a91a2c8c25e5541f14dc2d5d0109318b080e3c92251498c42
  test "$(sha256sum /usr/share/portage-engine/desktop-fixtures/webview-fixture.html | awk '{print $1}')" = 40ab1938f53b876bb5c6047f3d8642f060b63dcc0d30870d337bf8abc91e2e34
  id -u portage-e2e >/dev/null
  grep -Fxq 'autologin-user=portage-e2e' /etc/lightdm/lightdm.conf.d/50-portage-engine-e2e.conf
  test "$(stat -c '%U:%G' /home/portage-e2e/.config)" = portage-e2e:portage-e2e
  runuser --user portage-e2e -- test -w /home/portage-e2e/.config
  systemctl is-enabled lightdm.service
  systemctl is-enabled display-manager.service
  test "$(systemctl get-default)" = graphical.target
fi

log "scanning for residual secrets"
if find /etc /root -xdev -type f \( -name '*.secret' -o -name '*token*' -o -name 'terraform.tfstate*' \) -print -quit | grep -q .; then
  echo "secret-like file remains in the image" >&2
  exit 1
fi
if grep -ERIl --exclude='build-plan.json' --exclude='image-build.json' \
  "(api[_-]?key|token|password|secret)[[:space:]]*[:=][[:space:]]*[^[:space:]\"']+" \
  /etc/portage /etc/portage-engine /root 2>/dev/null | grep -q .; then
  echo "secret-like configuration content remains in the image" >&2
  exit 1
fi

log "sanitizing machine identity and transient state"
cloud-init clean --logs --seed || cloud-init clean --logs
# Never seal the Packer VM's MAC-specific renderer output into its successor.
# cloud-init recreates the file for the clone and the enabled refresh unit
# reloads networkd after that network stage.
find /etc/systemd/network -maxdepth 1 -type f \
  -name '10-cloud-init-*.network' -delete
find /etc/systemd/network -maxdepth 1 -type d \
  -name '10-cloud-init-*.network.d' -exec rm -rf -- {} +
rm -f /etc/ssh/ssh_host_*
find /root /home -xdev -type d -name .ssh -prune -exec rm -rf -- {} +
rm -f /root/.bash_history
find /tmp /var/tmp -xdev -mindepth 1 -delete
rm -rf /var/lib/cloud/instance /var/lib/cloud/instances /var/lib/cloud/sem
rm -f /var/lib/systemd/random-seed
rm -f /var/lib/dhcpcd/*.lease /var/lib/dhcp/*.leases /var/lib/NetworkManager/*lease*
rm -rf /var/log/journal/*
find /var/log -type f -exec truncate -s 0 {} +
rm -f /etc/machine-id /var/lib/dbus/machine-id
install -m 0644 /dev/null /etc/machine-id

cat >/etc/portage-engine/image-build.json <<EOF
{
  "schema_version": 1,
  "profile_id": "${PE_PROFILE_ID}",
  "profile_path": "${PE_PROFILE_PATH}",
  "profile_repository": "${PE_PROFILE_REPOSITORY}",
  "profile_parents_csv": "${PE_PROFILE_PARENTS}",
  "package_sets_csv": "${PE_PACKAGE_SETS}",
  "package_set_catalog_digest": "${PE_PACKAGE_SET_CATALOG_DIGEST}",
  "image_generation": "${PE_IMAGE_GENERATION}",
  "mirror_bundle_id": "${PE_MIRROR_BUNDLE_ID}",
  "repository_names_csv": "${PE_REPOSITORY_NAMES}",
  "repository_revisions_csv": "${PE_REPOSITORY_REVISIONS}",
  "build_plan_digest": "${PE_BUILD_PLAN_DIGEST}",
  "input_lock_digest": "${PE_INPUT_LOCK_DIGEST}",
  "common_config_digest": "${PE_COMMON_CONFIG_DIGEST}",
  "source_template": "${PE_SOURCE_TEMPLATE}",
  "source_vmid": ${PE_SOURCE_VMID},
  "source_provenance_object_id": "${PE_SOURCE_PROVENANCE_OBJECT_ID}",
  "source_provenance_digest": "${PE_SOURCE_PROVENANCE_DIGEST}",
  "desktop": ${PE_DESKTOP},
  "display_model": "${PE_DISPLAY_MODEL}"
}
EOF
chmod 0644 /etc/portage-engine/image-build.json

test ! -s /etc/machine-id
test -z "$(find /etc/systemd/network -maxdepth 1 -type f \
  -name '10-cloud-init-*.network' -print -quit)"
test -z "$(find /etc/systemd/network -maxdepth 1 -type d \
  -name '10-cloud-init-*.network.d' -print -quit)"
test ! -e /root/.ssh/authorized_keys
test ! -e /root/.bash_history
test ! -e /etc/ssh/ssh_host_rsa_key
log "template gate passed"
