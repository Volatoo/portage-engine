#!/usr/bin/env bash
set -euo pipefail

log() {
  echo "[image-factory] $*"
}

require_value() {
  local name=$1
  if [[ -z ${!name:-} ]]; then
    echo "missing required environment value: $name" >&2
    exit 1
  fi
}

for name in PE_PROFILE_ID PE_PROFILE_PATH PE_PROFILE_REPOSITORY PE_IMAGE_GENERATION PE_MIRROR_BUNDLE_ID \
  PE_REPOSITORY_NAMES PE_REPOSITORY_URIS PE_REPOSITORY_REVISIONS PE_REPOSITORY_BUNDLE_NAMES PE_GENTOO_MIRROR PE_PACKAGES PE_DESKTOP \
  PE_PACKAGE_SETS PE_PACKAGE_SET_CATALOG_DIGEST \
	PE_GENTOO_REPOSITORY_KEY_NAME \
  PE_BUILD_PLAN_DIGEST PE_INPUT_LOCK_DIGEST PE_COMMON_CONFIG_DIGEST PE_SOURCE_TEMPLATE PE_SOURCE_VMID \
  PE_SOURCE_PROVENANCE_OBJECT_ID PE_SOURCE_PROVENANCE_DIGEST; do
  require_value "$name"
done

[[ $PE_PROFILE_ID =~ ^[a-zA-Z0-9][a-zA-Z0-9+._/-]+$ ]]
[[ $PE_PROFILE_PATH =~ ^[a-zA-Z0-9][a-zA-Z0-9+._/-]+$ ]]
[[ $PE_PROFILE_REPOSITORY =~ ^[a-zA-Z0-9][a-zA-Z0-9+._-]+$ ]]
[[ $PE_IMAGE_GENERATION =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]+$ ]]
[[ $PE_MIRROR_BUNDLE_ID =~ ^[a-zA-Z0-9][a-zA-Z0-9+._/-]+$ ]]
[[ $PE_BUILD_PLAN_DIGEST =~ ^sha256:[a-f0-9]{64}$ ]]
[[ $PE_INPUT_LOCK_DIGEST =~ ^sha256:[a-f0-9]{64}$ ]]
[[ $PE_COMMON_CONFIG_DIGEST =~ ^sha256:[a-f0-9]{64}$ ]]
[[ $PE_SOURCE_PROVENANCE_DIGEST =~ ^sha256:[a-f0-9]{64}$ ]]
[[ $PE_PACKAGE_SET_CATALOG_DIGEST =~ ^sha256:[a-f0-9]{64}$ ]]
[[ $PE_SOURCE_VMID =~ ^[0-9]+$ ]]
[[ $PE_SOURCE_TEMPLATE =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]+$ ]]
[[ $PE_SOURCE_PROVENANCE_OBJECT_ID =~ ^[a-zA-Z0-9][a-zA-Z0-9+._/-]+$ ]]
[[ $PE_GENTOO_MIRROR =~ ^https?://[a-zA-Z0-9._:-]+(/[a-zA-Z0-9._~:/%+-]*)?$ ]]
if [[ -n ${PE_BINHOST:-} ]]; then
  [[ $PE_BINHOST =~ ^https?://[a-zA-Z0-9._:-]+(/[a-zA-Z0-9._~:/%+-]*)?$ ]]
fi
[[ $PE_DESKTOP == true || $PE_DESKTOP == false ]]

if [[ -n ${PE_TRUSTED_CA_NAME:-} ]]; then
  ca_name=$PE_TRUSTED_CA_NAME
  [[ $ca_name == "$(basename -- "$ca_name")" && $ca_name == *.crt ]]
  install -d -m 0755 /usr/local/share/ca-certificates
  install -m 0644 "/tmp/pe-repository-bundles/$ca_name" "/usr/local/share/ca-certificates/portage-engine-$ca_name"
  update-ca-certificates
fi

install -d -m 0755 /etc/portage/repos.conf /etc/portage/binrepos.conf /etc/portage-engine
IFS=',' read -r -a repository_names <<<"$PE_REPOSITORY_NAMES"
IFS=',' read -r -a repository_uris <<<"$PE_REPOSITORY_URIS"
IFS=',' read -r -a repository_revisions <<<"$PE_REPOSITORY_REVISIONS"
IFS=',' read -r -a repository_bundle_names <<<"$PE_REPOSITORY_BUNDLE_NAMES"
if [[ ${#repository_names[@]} -lt 1 || ${#repository_names[@]} -gt 2 ||
      ${#repository_names[@]} -ne ${#repository_uris[@]} ||
      ${#repository_names[@]} -ne ${#repository_revisions[@]} ||
      ${#repository_names[@]} -ne ${#repository_bundle_names[@]} ]]; then
  echo "repository vectors are empty, oversized, or inconsistent" >&2
  exit 1
fi

gentoo_seen=0
profile_seen=0
profile_revision=
for index in "${!repository_names[@]}"; do
  name=${repository_names[$index]}
  uri=${repository_uris[$index]}
  revision=${repository_revisions[$index]}
  bundle_name=${repository_bundle_names[$index]}
  [[ $name =~ ^[a-zA-Z0-9][a-zA-Z0-9+._-]+$ ]]
  [[ $uri =~ ^https?://[a-zA-Z0-9._:-]+(/[a-zA-Z0-9._~:/%+-]*)?$ ]]
  [[ $revision =~ ^([a-f0-9]{40}|[a-f0-9]{64})$ ]]
  [[ $bundle_name == "$(basename -- "$bundle_name")" && $bundle_name != . && $bundle_name != .. ]]
  repo="/var/db/repos/$name"
  bundle="/tmp/pe-repository-bundles/$bundle_name"
  [[ -f $bundle ]]

  # Catalyst may preserve the build user's ownership on repositories copied
  # into the stage4. This provisioner runs as root, so normalize ownership
  # before Git applies its dubious-ownership protection. Do not weaken that
  # protection globally with safe.directory exceptions.
  if [[ -d $repo ]]; then
    chown -R root:root -- "$repo"
  fi

  if [[ $name == gentoo ]]; then
    gentoo_seen=1
    if [[ ! -d $repo/.git ]]; then
      candidate_repo=/var/db/repos/.gentoo-portage-engine-new
      seed_repo=/var/db/repos/.gentoo-portage-engine-seed
      [[ ! -e $candidate_repo && ! -e $seed_repo ]]
      install -d -m 0755 -- "$candidate_repo"
      git -C "$candidate_repo" init
      git -C "$candidate_repo" remote add origin "$uri"
      git -C "$candidate_repo" bundle unbundle "$bundle" >/dev/null
      git -C "$candidate_repo" cat-file -e "$revision^{commit}"
      if ! git -C "$candidate_repo" cat-file -e "$revision^" 2>/dev/null; then
        printf '%s\n' "$revision" >"$candidate_repo/.git/shallow"
      fi
      git -C "$candidate_repo" checkout --detach "$revision"
      test "$(git -C "$candidate_repo" rev-parse HEAD)" = "$revision"
      key_name=$PE_GENTOO_REPOSITORY_KEY_NAME
      [[ $key_name == "$(basename -- "$key_name")" && $key_name != . && $key_name != .. ]]
      gentoo_gnupg_home=/tmp/pe-gentoo-gnupg
      install -d -m 0700 "$gentoo_gnupg_home"
      gpg --batch --homedir "$gentoo_gnupg_home" --import "/tmp/pe-repository-bundles/$key_name" >/dev/null
      GNUPGHOME="$gentoo_gnupg_home" git -C "$candidate_repo" verify-commit "$revision"
      mv -- "$repo" "$seed_repo"
      mv -- "$candidate_repo" "$repo"
      rm -rf -- "$seed_repo"
    fi
  elif [[ ! -d $repo/.git ]]; then
    if [[ -e $repo ]]; then
      if [[ ! -d $repo || -n $(find "$repo" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
        echo "external repository path is occupied: $repo" >&2
        exit 1
      fi
    fi
    install -d -m 0755 -- "$repo"
    git -C "$repo" init
  fi

  log "pinning repository $name to $revision"
  if git -C "$repo" remote get-url origin >/dev/null 2>&1; then
    git -C "$repo" remote set-url origin "$uri"
  else
    git -C "$repo" remote add origin "$uri"
  fi
  if ! git -C "$repo" cat-file -e "$revision^{commit}" 2>/dev/null; then
    git -C "$repo" bundle unbundle "$bundle" >/dev/null
    git -C "$repo" cat-file -e "$revision^{commit}"
    if ! git -C "$repo" cat-file -e "$revision^" 2>/dev/null; then
      printf '%s\n' "$revision" >"$repo/.git/shallow"
    fi
  fi
  git -C "$repo" checkout --detach "$revision"
  git -C "$repo" clean -ffdx
  test "$(git -C "$repo" rev-parse HEAD)" = "$revision"
  test -z "$(git -C "$repo" status --porcelain=v1 --untracked-files=all)"
  test -z "$(git -C "$repo" clean -ndx)"
  if [[ $name == gentoo ]]; then
    key_name=$PE_GENTOO_REPOSITORY_KEY_NAME
    [[ $key_name == "$(basename -- "$key_name")" && $key_name != . && $key_name != .. ]]
    gentoo_gnupg_home=/tmp/pe-gentoo-gnupg
    install -d -m 0700 "$gentoo_gnupg_home"
    gpg --batch --homedir "$gentoo_gnupg_home" --import "/tmp/pe-repository-bundles/$key_name" >/dev/null
    GNUPGHOME="$gentoo_gnupg_home" git -C "$repo" verify-commit "$revision"
  fi
  if [[ $name == "$PE_PROFILE_REPOSITORY" ]]; then
    profile_seen=1
    profile_revision=$revision
  fi

  cat >"/etc/portage/repos.conf/$name.conf" <<EOF
[$name]
location = /var/db/repos/$name
sync-type = git
sync-uri = $uri
auto-sync = no
EOF
done
[[ $gentoo_seen -eq 1 && $profile_seen -eq 1 ]]

profile_repo="/var/db/repos/$PE_PROFILE_REPOSITORY"
profile_dir="$profile_repo/profiles/$PE_PROFILE_PATH"
if [[ ! -d $profile_dir ]]; then
  echo "profile is absent from the pinned repository: $PE_PROFILE_REPOSITORY:$PE_PROFILE_PATH" >&2
  exit 1
fi
if [[ $PE_PROFILE_REPOSITORY != gentoo ]]; then
  key_name=${PE_PROFILE_REPOSITORY_KEY_NAME:-}
  [[ -n $key_name && $key_name == "$(basename -- "$key_name")" && $key_name != . && $key_name != .. ]]
  profile_gnupg_home=/tmp/pe-profile-gnupg
  install -d -m 0700 "$profile_gnupg_home"
  gpg --batch --homedir "$profile_gnupg_home" --import "/tmp/pe-repository-bundles/$key_name" >/dev/null
  GNUPGHOME="$profile_gnupg_home" git -C "$profile_repo" verify-commit "$profile_revision"
  profile_parents_json=$(python3 - "$PE_PROFILE_PARENTS" <<'PY'
import json, sys

parents = []
for value in filter(None, sys.argv[1].split(",")):
    repository, separator, profile_path = value.partition(":")
    if not separator:
        raise SystemExit("profile parent lacks repository qualifier")
    parents.append({"repository": repository, "profile_path": profile_path})
print(json.dumps(parents, separators=(",", ":")))
PY
)
  python3 /tmp/verify-profile.py "$profile_repo" "$PE_PROFILE_REPOSITORY" "$PE_PROFILE_PATH" "$profile_parents_json"
fi
if [[ -d /etc/portage/make.profile && ! -L /etc/portage/make.profile ]]; then
  echo "/etc/portage/make.profile is an unexpected directory" >&2
  exit 1
fi
rm -f /etc/portage/make.profile
ln -s "$profile_dir" /etc/portage/make.profile

touch /etc/portage/make.conf
cat >>/etc/portage/make.conf <<EOF

# portage-engine image factory
GENTOO_MIRRORS="${PE_GENTOO_MIRROR}"
FEATURES="buildpkg"
EOF

if [[ -n ${PE_BINHOST:-} ]]; then
  cat >/etc/portage/binrepos.conf/portage-engine.conf <<EOF
[portage-engine]
sync-uri = ${PE_BINHOST}
priority = 100
EOF
fi

IFS=',' read -r -a packages <<<"$PE_PACKAGES"
for atom in "${packages[@]}"; do
  [[ $atom =~ ^[a-zA-Z0-9][a-zA-Z0-9+._-]*/[a-zA-Z0-9][a-zA-Z0-9+._-]*$ ]]
done
IFS=',' read -r -a package_sets <<<"$PE_PACKAGE_SETS"
for set_id in "${package_sets[@]}"; do
  [[ $set_id =~ ^[a-zA-Z0-9][a-zA-Z0-9+._/-]+$ ]]
done

log "hydrating the content-addressed distfile closure"
python3 /tmp/hydrate-distfiles.py

install -d -m 0755 /etc/portage/sets
printf '%s\n' "${packages[@]}" >/etc/portage/sets/portage-engine-image
chmod 0644 /etc/portage/sets/portage-engine-image

log "installing ${#packages[@]} packages from locked sets: ${PE_PACKAGE_SETS}"
log "reconciling @world with the selected profile"
emerge --verbose --update --deep --newuse --with-bdeps=y @world
emerge --verbose --update --deep --newuse --with-bdeps=y @portage-engine-image

install -d -m 0755 /etc/portage-engine/evidence
emerge --info > /etc/portage-engine/evidence/emerge-info.txt
emerge --pretend --verbose --update --deep --newuse --with-bdeps=y @world \
  > /etc/portage-engine/evidence/world-depgraph.txt
emerge --pretend --verbose --update --deep --newuse --with-bdeps=y @portage-engine-image \
  > /etc/portage-engine/evidence/package-set-depgraph.txt

command -v systemctl >/dev/null
systemctl enable qemu-guest-agent.service
for unit in cloud-init-local.service cloud-init-network.service cloud-init-main.service cloud-init.service cloud-config.service cloud-final.service; do
  if systemctl list-unit-files "$unit" --no-legend 2>/dev/null | grep -q "^$unit"; then
    systemctl enable "$unit"
  fi
done

# PVE can start a clone with the template build's rendered networkd file and
# replace it only when cloud-init's network stage runs. Install a post-stage
# refresh that binds DHCP to the clone NIC rather than the sealed template's
# transient identity.
install -d -m 0755 /usr/local/libexec/portage-engine
cat >/usr/local/libexec/portage-engine/cloud-init-network-refresh <<'EOF'
#!/bin/sh
set -eu

network_file=/etc/systemd/network/10-cloud-init-eth0.network
if ! grep -q '^ClientIdentifier=mac$' "$network_file"; then
  printf '\n[DHCPv4]\nClientIdentifier=mac\n' >>"$network_file"
fi
networkctl reload
attempt=0
while [ "$attempt" -lt 30 ]; do
  if [ -e /sys/class/net/eth0 ]; then
    networkctl reconfigure eth0
    exit 0
  fi
  attempt=$((attempt + 1))
  sleep 1
done
echo 'cloud-init network refresh: eth0 did not appear' >&2
exit 1
EOF
chmod 0755 /usr/local/libexec/portage-engine/cloud-init-network-refresh

cat >/etc/systemd/system/portage-cloud-init-network-refresh.service <<'EOF'
[Unit]
Description=Refresh cloud-init networkd configuration for the current clone
After=systemd-networkd.service cloud-init-network.service
Wants=systemd-networkd.service
ConditionPathExists=/etc/systemd/network/10-cloud-init-eth0.network

[Service]
Type=oneshot
ExecStart=/usr/local/libexec/portage-engine/cloud-init-network-refresh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable portage-cloud-init-network-refresh.service
if [[ $PE_DESKTOP == true ]]; then
  desktop_user=portage-e2e
  if ! id -u "$desktop_user" >/dev/null 2>&1; then
    useradd --create-home --user-group --groups video,input --shell /bin/bash "$desktop_user"
  fi
  install -d -m 0755 /usr/libexec /etc/lightdm/lightdm.conf.d
  install -m 0755 /tmp/portage-desktop-agent.py /usr/libexec/portage-desktop-agent
  install -d -m 0755 /usr/share/portage-engine/desktop-fixtures
  install -m 0644 /tmp/portage-desktop-fixtures/editor-fixture.txt /usr/share/portage-engine/desktop-fixtures/editor-fixture.txt
  install -m 0644 /tmp/portage-desktop-fixtures/webview-fixture.html /usr/share/portage-engine/desktop-fixtures/webview-fixture.html
  cat >/etc/lightdm/lightdm.conf.d/50-portage-engine-e2e.conf <<EOF
[Seat:*]
autologin-user=$desktop_user
autologin-user-timeout=0
user-session=xfce
EOF
  install -d -o "$desktop_user" -g "$desktop_user" -m 0750 "/home/$desktop_user/.config"
  install -d -o "$desktop_user" -g "$desktop_user" -m 0750 "/home/$desktop_user/.config/autostart"
  cat >"/home/$desktop_user/.xprofile" <<'EOF'
#!/bin/sh
output=$(xrandr --query | awk '/ connected/{print $1; exit}')
if [ -n "$output" ]; then
  xrandr --output "$output" --mode 1280x720 2>/dev/null || true
fi
xset s off
xset -dpms
xfconf-query -c xsettings -p /Net/ThemeName -s Adwaita 2>/dev/null || true
xfconf-query -c xsettings -p /Net/IconThemeName -s hicolor 2>/dev/null || true
xfconf-query -c xsettings -p /Gtk/FontName -s 'DejaVu Sans 10' 2>/dev/null || true
xfconf-query -c xfwm4 -p /general/use_compositing -s false 2>/dev/null || true
EOF
  chown "$desktop_user:$desktop_user" "/home/$desktop_user/.xprofile"
  chmod 0755 "/home/$desktop_user/.xprofile"
  printf 'LANG=C.UTF-8\n' >/etc/locale.conf
  chmod 0644 /etc/locale.conf
  systemctl list-unit-files lightdm.service --no-legend 2>/dev/null | grep -q '^lightdm.service'
  systemctl enable lightdm.service
  systemctl is-enabled lightdm.service
  systemctl is-enabled display-manager.service
  systemctl set-default graphical.target
fi

install -m 0644 /tmp/pe-build-plan.json /etc/portage-engine/build-plan.json
log "provisioning complete"
