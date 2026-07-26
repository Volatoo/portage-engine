#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

if [[ ${STRICT_OFFLINE:-} != 1 || $(uname -s) != Linux || ${EUID} -ne 0 ]]; then
  echo "STRICT_OFFLINE=1 and a root Linux worker are required" >&2
  exit 2
fi
if [[ $# -ne 7 ]]; then
  echo "usage: $0 TARGET PLAN_JSON OFFLINE_ROOT INPUT_LOCK OUTPUT_DIR TRUSTED_SYNC_PUBLIC_KEY TRUSTED_FACTORY_SHA256" >&2
  exit 2
fi
for command in awk jq sha256sum stat realpath; do
  command -v "${command}" >/dev/null || { echo "missing bootstrap command: ${command}" >&2; exit 1; }
done

target=$1
plan=$2
offline_root=$(realpath -e -- "$3")
input_lock=$(realpath -e -- "$4")
output_dir=$5
trusted_sync_public_key=$(realpath -e -- "$6")
trusted_factory_sha256=$7
[[ ${trusted_factory_sha256} =~ ^[a-f0-9]{64}$ ]]
[[ ${target} =~ ^catalyst-(base|profile)-systemd$ ]]
case "${trusted_sync_public_key}" in
  "${offline_root}"/*) echo "trusted sync public key must be provisioned outside the incoming offline bundle" >&2; exit 1;;
esac
case "${plan}" in
  /*) plan=$(realpath -e -- "${plan}") ;;
  *) plan=$(realpath -e -- "${plan}") ;;
esac
case "${plan}" in "${offline_root}"/*) ;; *) echo "Catalyst plan must be inside the offline root" >&2; exit 1;; esac
case "${input_lock}" in "${offline_root}"/*) ;; *) echo "input lock must be inside the offline root" >&2; exit 1;; esac

locked_path=""
verify_locked() {
  local id=$1
  local row path digest size resolved actual_digest actual_size
  row=$(jq -er --arg id "${id}" --arg target "${target}" '
    [.objects[] | select(.id == $id and ((.required_for // []) | index($target)))] |
    if length == 1 then .[0] else error("required locked execution object is missing or duplicated") end |
    [.path, .sha256, (.size | tostring)] | @tsv
  ' "${input_lock}")
  IFS=$'\t' read -r path digest size <<<"${row}"
  [[ ${path} != /* && ${path} != .. && ${path} != ../* ]]
  resolved=$(realpath -e -- "${offline_root}/${path}")
  case "${resolved}" in "${offline_root}"/*) ;; *) echo "locked execution object escapes offline root: ${id}" >&2; exit 1;; esac
  [[ -f ${resolved} ]]
  actual_size=$(stat -c '%s' -- "${resolved}")
  actual_digest=$(sha256sum -- "${resolved}")
  actual_digest=${actual_digest%% *}
  if [[ ${actual_size} != "${size}" || ${actual_digest} != "${digest}" ]]; then
    echo "locked execution object failed bootstrap verification: ${id}" >&2
    exit 1
  fi
  locked_path=${resolved}
}

verify_locked "tool/portage-image-factory/catalyst/linux-amd64"
factory_bin=${locked_path}
[[ -x ${factory_bin} ]]
[[ $(sha256sum -- "${factory_bin}" | awk '{print $1}') == "${trusted_factory_sha256}" ]] || {
  echo "locked image-factory binary does not match the out-of-band trust digest" >&2
  exit 1
}
verify_locked "script/catalyst/run-offline-v1"
runner=${locked_path}
[[ -x ${runner} ]]
runner_dir=$(dirname -- "${runner}")
verify_locked "script/catalyst/hydrate-distfiles-v1"
hydrate_script=${locked_path}

verify_locked "script/catalyst/safe-extract-v1"
[[ ${locked_path} == "${runner_dir}/safe-extract.py" ]]
verify_locked "script/catalyst/verify-stage3-v1"
[[ ${locked_path} == "${runner_dir}/verify-stage3.py" ]]
verify_locked "script/catalyst/verify-runtime-v1"
[[ ${locked_path} == "${runner_dir}/verify-runtime.py" ]]
if [[ ${target} == catalyst-profile-systemd ]]; then
  verify_locked "script/catalyst/verify-profile-v1"
  [[ ${locked_path} == "${runner_dir}/verify-profile.py" ]]
fi

"${factory_bin}" bundle-verify \
  -manifest "${offline_root}/bundle-manifest.json" \
  -signature "${offline_root}/bundle-manifest.sig.json" \
  -public-key "${trusted_sync_public_key}" \
  -lock "${input_lock}" \
  -root "${offline_root}" >/dev/null
"${factory_bin}" preflight -lock "${input_lock}" -root "${offline_root}" -target "${target}" >/dev/null
export PORTAGE_IMAGE_FACTORY_BIN=${factory_bin}
export CATALYST_HYDRATE_SCRIPT=${hydrate_script}
exec "${runner}" "${plan}" "${offline_root}" "${input_lock}" "${output_dir}"
