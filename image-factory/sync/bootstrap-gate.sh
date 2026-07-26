#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 GENTOO_COMMIT PROFILE_COMMIT CLOSURE_MANIFEST PROFILE_GNUPGHOME" >&2
  exit 2
fi

gentoo_commit=$1
profile_commit=$2
closure_manifest=$3
profile_gnupg_home=$4
[[ ${gentoo_commit} =~ ^([a-f0-9]{40}|[a-f0-9]{64})$ ]]
[[ ${profile_commit} =~ ^([a-f0-9]{40}|[a-f0-9]{64})$ ]]
[[ -f ${closure_manifest} && -d ${profile_gnupg_home} ]]

actual_gentoo_commit=$(awk 'NR == 1 {print $1}' /var/db/repos/gentoo/metadata/timestamp.commit)
actual_profile_commit=$(git -C /var/db/repos/pe-profiles rev-parse HEAD)
[[ ${actual_gentoo_commit} == "${gentoo_commit}" ]]
[[ ${actual_profile_commit} == "${profile_commit}" ]]
[[ $(cat /etc/portage/make.profile/parent) == gentoo:default/linux/amd64/23.0/no-multilib/systemd ]]
portageq match / dev-vcs/git | grep -q '^dev-vcs/git-'

date -u +checked_at=%Y-%m-%dT%H:%M:%SZ
printf 'gentoo_timestamp_commit=%s\n' "${actual_gentoo_commit}"
printf 'git_version='; git --version
printf 'git_cpv='; portageq match / dev-vcs/git
printf 'profile='; eselect profile show | tail -n 1 | sed 's/^[[:space:]]*//'
printf 'profile_parent='; cat /etc/portage/make.profile/parent
printf 'profile_commit=%s\n' "${actual_profile_commit}"
GNUPGHOME=${profile_gnupg_home} git -C /var/db/repos/pe-profiles verify-commit "${profile_commit}"
printf 'closure_sha256='; sha256sum "${closure_manifest}" | awk '{print $1}'
python3 - "${closure_manifest}" <<'PY'
import json
import pathlib
import sys

data = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
print(f"closure_objects={len(data['objects'])}")
print(f"closure_bytes={sum(item['size'] for item in data['objects'])}")
PY
