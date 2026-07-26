#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

if [[ ${STRICT_OFFLINE:-} != 1 ]]; then
  echo "STRICT_OFFLINE=1 is required" >&2
  exit 2
fi
if [[ $(uname -s) != Linux || ${EUID} -ne 0 ]]; then
  echo "Catalyst requires a root Linux worker" >&2
  exit 2
fi
if [[ $# -ne 4 ]]; then
  echo "usage: $0 PLAN_JSON OFFLINE_ROOT INPUT_LOCK OUTPUT_DIR" >&2
  exit 2
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
repo_root=$(cd -- "${script_dir}/../.." && pwd -P)
factory_bin=${PORTAGE_IMAGE_FACTORY_BIN:-${repo_root}/bin/portage-image-factory}
plan=$1
offline_root=$2
input_lock=$3
output_dir=$4

for command in python3 git gpg gpgv unshare cp install find mount umount tar2sqfs rsync tar tee xz; do
  command -v "${command}" >/dev/null || { echo "missing required command: ${command}" >&2; exit 1; }
done
[[ -x ${factory_bin} ]] || { echo "missing image-factory binary: ${factory_bin}" >&2; exit 1; }
mkdir -p -- "${output_dir}"
output_dir=$(cd -- "${output_dir}" && pwd -P)
if [[ -n $(find "${output_dir}" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
  echo "Catalyst output directory must be empty: ${output_dir}" >&2
  exit 1
fi
work_root=$(mktemp -d "${output_dir}/.catalyst-work.XXXXXXXX")
prepared="${output_dir}/catalyst.prepared.json"
gate="${output_dir}/catalyst.gate.json"
rootfs_manifest="${output_dir}/catalyst.rootfs-manifest.json"
success=0

cleanup() {
  if [[ ${success} -eq 1 ]]; then
    rm -rf -- "${work_root}"
  else
    echo "Catalyst failed; recovery workdir retained at ${work_root}" >&2
  fi
}
trap cleanup EXIT

json_value() {
  python3 - "$prepared" "$1" <<'PY'
import json, pathlib, sys
data = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
value = data[sys.argv[2]]
if not isinstance(value, (str, int)):
    raise SystemExit("prepared value has unexpected type")
print(value)
PY
}

json_optional() {
  python3 - "$prepared" "$1" <<'PY'
import json, pathlib, sys
data = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
value = data.get(sys.argv[2], "")
if not isinstance(value, (str, int)):
    raise SystemExit("prepared value has unexpected type")
print(value)
PY
}

json_compact() {
  python3 - "$prepared" "$1" <<'PY'
import json, pathlib, sys
data = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
print(json.dumps(data.get(sys.argv[2], []), separators=(",", ":")))
PY
}

"${factory_bin}" catalyst-plan -plan "${plan}" -lock "${input_lock}" -root "${offline_root}" -work "${work_root}" -output "${prepared}"

runtime_archive=$(json_value runtime_archive_path)
runtime_root=$(json_value runtime_root)
catalyst_bin=$(json_value catalyst_binary_path)
repository_bundle=$(json_value repository_bundle_path)
repository_commit=$(json_value repository_commit)
repository_key=$(json_value gentoo_repository_key_path)
repos_storedir=$(json_value repos_storedir)
stage3=$(json_value stage3_path)
stage3_digests=$(json_value stage3_digests_path)
release_key=$(json_value release_key_path)
distfile_manifest=$(json_value distfile_manifest_path)
distdir=$(json_value distdir)
seed_destination=$(json_value seed_destination)
config_path=$(json_value config_path)
spec_path=$(json_value spec_path)
snapshot_id=$(json_value snapshot_id)
expected_rootfs=$(json_value expected_rootfs_path)

python3 "${script_dir}/safe-extract.py" "${runtime_archive}" "${runtime_root}"
[[ -x ${catalyst_bin} && -d ${runtime_root}/share/catalyst ]] || { echo "locked Catalyst runtime lacks bin/catalyst or share/catalyst" >&2; exit 1; }
mapfile -d '' -t runtime_python_paths < <(
  find "${runtime_root}/lib" -type d \( -name site-packages -o -name dist-packages \) -print0
)
if [[ ${#runtime_python_paths[@]} -ne 1 ]]; then
  echo "locked Catalyst runtime must contain exactly one lib/pythonX.Y/(site|dist)-packages directory" >&2
  exit 1
fi
runtime_python_path=${runtime_python_paths[0]}

gnupg_home="${work_root}/gnupg"
repository_gnupg_home="${work_root}/repository-gnupg"
mkdir -m 0700 -- "${gnupg_home}" "${repository_gnupg_home}" "${repos_storedir}"
gpg --batch --homedir "${gnupg_home}" --import "${release_key}" >/dev/null
gpgv --homedir "${gnupg_home}" --keyring "${gnupg_home}/pubring.kbx" "${stage3_digests}"
python3 "${script_dir}/verify-stage3.py" "${stage3}" "${stage3_digests}"
gpg --batch --homedir "${repository_gnupg_home}" --import "${repository_key}" >/dev/null

gentoo_repo="${repos_storedir}/gentoo.git"
git init --bare "${gentoo_repo}"
git -C "${gentoo_repo}" symbolic-ref HEAD refs/heads/master
git -C "${gentoo_repo}" bundle verify "${repository_bundle}"
# A snapshot bundle may intentionally contain only the signed tip commit. A
# normal fetch rejects that pack because the commit's parent is not present.
# Import the pack first, verify the requested object, then record the shallow
# boundary before exposing the commit through a ref.
git -C "${gentoo_repo}" bundle unbundle "${repository_bundle}" >/dev/null
git -C "${gentoo_repo}" cat-file -e "${repository_commit}^{commit}"
if ! git -C "${gentoo_repo}" cat-file -e "${repository_commit}^" 2>/dev/null; then
  printf '%s\n' "${repository_commit}" >"${gentoo_repo}/shallow"
fi
git -C "${gentoo_repo}" update-ref refs/heads/master "${repository_commit}"
[[ $(git -C "${gentoo_repo}" rev-parse 'refs/heads/master^{commit}') == "${repository_commit}" ]]
GNUPGHOME="${repository_gnupg_home}" git -C "${gentoo_repo}" verify-commit "${repository_commit}"
git -C "${gentoo_repo}" update-ref "refs/heads/${snapshot_id}" "${repository_commit}"
[[ $(git -C "${gentoo_repo}" rev-parse "refs/heads/${snapshot_id}^{commit}") == "${repository_commit}" ]]

profile_repository=$(json_optional profile_repository)
if [[ -n ${profile_repository} && ${profile_repository} != gentoo ]]; then
  profile_repository_bundle=$(json_value profile_repository_bundle_path)
  profile_repository_key=$(json_value profile_repository_key_path)
  profile_repository_source=$(json_value profile_repository_source_path)
  profile_repository_commit=$(json_value profile_repository_commit)
  profile_path=$(json_value profile_path)
  profile_parents=$(json_compact profile_parents)
  profile_gnupg_home="${work_root}/profile-gnupg"

  mkdir -m 0700 -- "${profile_gnupg_home}"
  gpg --batch --homedir "${profile_gnupg_home}" --import "${profile_repository_key}" >/dev/null
  git init "${profile_repository_source}"
  git -C "${profile_repository_source}" bundle verify "${profile_repository_bundle}"
  git -C "${profile_repository_source}" bundle unbundle "${profile_repository_bundle}" >/dev/null
  git -C "${profile_repository_source}" cat-file -e "${profile_repository_commit}^{commit}"
  if ! git -C "${profile_repository_source}" cat-file -e "${profile_repository_commit}^" 2>/dev/null; then
    printf '%s\n' "${profile_repository_commit}" >"${profile_repository_source}/.git/shallow"
  fi
  git -C "${profile_repository_source}" checkout --detach "${profile_repository_commit}"
  [[ $(git -C "${profile_repository_source}" rev-parse 'HEAD^{commit}') == "${profile_repository_commit}" ]]
  [[ -z $(git -C "${profile_repository_source}" status --porcelain --untracked-files=all) ]]
  GNUPGHOME="${profile_gnupg_home}" git -C "${profile_repository_source}" verify-commit "${profile_repository_commit}"
  python3 "${script_dir}/verify-profile.py" "${profile_repository_source}" "${profile_repository}" "${profile_path}" "${profile_parents}"

  # The runner's umask deliberately makes new files private. Catalyst exposes
  # this verified checkout as /var/db/repos/<name>, where Portage performs
  # fetches as its unprivileged user. Normalize only the signed worktree after
  # rejecting types that could redirect access; keep .git private.
  unsafe_profile_entry=$(find "${profile_repository_source}" -path "${profile_repository_source}/.git" -prune -o \! -type d \! -type f -print -quit)
  [[ -z ${unsafe_profile_entry} ]] || { echo "verified profile worktree contains an unsafe entry: ${unsafe_profile_entry}" >&2; exit 1; }
  find "${profile_repository_source}" -path "${profile_repository_source}/.git" -prune -o -type d -exec chmod 0755 {} +
  find "${profile_repository_source}" -path "${profile_repository_source}/.git" -prune -o -type f -exec chmod 0644 {} +
fi

# Hydration may reach only the allowlisted internal mirror directly. Inherited
# proxy variables would otherwise move the trust boundary to an unreviewed
# proxy before the build enters its network namespace.
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy no_proxy NO_PROXY
# Resolve the locked sibling from this script, not from a checkout root.  The
# same layout is used at image-factory/{catalyst,packer} in source and at
# offline/factory/{catalyst,packer} in a materialized bundle.
hydrate_script=${CATALYST_HYDRATE_SCRIPT:-${script_dir}/../packer/scripts/hydrate-distfiles.py}
[[ -f ${hydrate_script} ]] || { echo "missing locked distfile hydration script" >&2; exit 1; }
python3 "${hydrate_script}" --manifest "${distfile_manifest}" --distdir "${distdir}"
# Catalyst bind-mounts this directory at target_distdir and Portage reads it as
# its unprivileged fetch user.  mktemp creates work_root with mode 0700 and the
# hydrator cannot widen an already-created child directory, so normalize the
# bind-mount root explicitly after every integrity-checked hydration.  The
# locked objects themselves remain read-only to non-root users.
chmod 0755 -- "${distdir}"
find "${distdir}" -mindepth 1 -maxdepth 1 -type f -exec chmod 0644 -- {} +
mkdir -p -- "$(dirname -- "${seed_destination}")"
install -m 0644 -- "${stage3}" "${seed_destination}"
install -m 0644 -- "${repository_key}" "${work_root}/gentoo-repository.asc"

export PATH="${runtime_root}/bin:/usr/sbin:/usr/bin:/sbin:/bin"
# Do not inherit a host PYTHONPATH: the locked runtime is the only accepted
# source for Catalyst and its Python dependencies.
export PYTHONPATH="${runtime_python_path}"
export PYTHONNOUSERSITE=1

python3 -S "${script_dir}/verify-runtime.py" "${runtime_python_path}" "${runtime_root}"
catalyst_module_path=$(python3 -S - <<'PY'
import pathlib
import catalyst

print(pathlib.Path(catalyst.__file__).resolve())
PY
)
[[ ${catalyst_module_path} == "${runtime_python_path}"/* ]] || {
  echo "Catalyst Python module escaped the locked runtime: ${catalyst_module_path}" >&2
  exit 1
}

# Internal mirror hydration occurs above. Both snapshot generation and stage4
# build run in a fresh namespace with no network devices or public fallback.
unshare --net --mount-proc -- python3 -S "${catalyst_bin}" -c "${config_path}" -s "${snapshot_id}" 2>&1 | tee "${output_dir}/catalyst-snapshot.log"
unshare --net --mount-proc -- python3 -S "${catalyst_bin}" -c "${config_path}" -f "${spec_path}" 2>&1 | tee "${output_dir}/catalyst-stage4.log"
[[ -s ${expected_rootfs} ]] || { echo "Catalyst did not produce the exact planned rootfs: ${expected_rootfs}" >&2; exit 1; }

"${factory_bin}" catalyst-gate -prepared "${prepared}" -output "${gate}"
"${factory_bin}" catalyst-manifest -plan "${plan}" -lock "${input_lock}" -prepared "${prepared}" -gate "${gate}" -rootfs "${expected_rootfs}" -output "${rootfs_manifest}"
install -m 0600 -- "${config_path}" "${output_dir}/catalyst.conf"
install -m 0600 -- "${spec_path}" "${output_dir}/stage4.spec"
install -m 0700 -- "$(json_value fsscript_path)" "${output_dir}/stage4-fsscript.sh"
cp --reflink=auto -- "${expected_rootfs}" "${output_dir}/$(basename -- "${expected_rootfs}")"
success=1
echo "Catalyst rootfs and evidence written to ${output_dir}"
