#!/usr/bin/env bash
# Minimal trusted launcher for the Packer lane. Install this file outside the
# incoming bundle and review/update it as part of the factory runner image.
set -euo pipefail

if [[ $# -ne 8 ]]; then
  echo "usage: STRICT_OFFLINE=1 $0 <base-systemd|desktop-verifier> COMMON_JSON OFFLINE_ROOT INPUT_LOCK BUNDLE_MANIFEST BUNDLE_SIGNATURE SYNC_PUBLIC_KEY APPROVED_FACTORY_SHA256" >&2
  exit 2
fi
if [[ ${STRICT_OFFLINE:-} != 1 ]]; then
  echo "STRICT_OFFLINE=1 is required" >&2
  exit 2
fi

target=$1
common=$2
root=$3
lock=$4
bundle_manifest=$5
bundle_signature=$6
sync_public_key=$7
approved_factory_sha256=$8
case ${target} in
  base-systemd|desktop-verifier) ;;
  *) echo "unsupported Packer target" >&2; exit 2 ;;
esac
root=$(cd -- "${root}" && pwd -P)
common=$(cd -- "$(dirname -- "${common}")" && pwd -P)/$(basename -- "${common}")
lock=$(cd -- "$(dirname -- "${lock}")" && pwd -P)/$(basename -- "${lock}")
bundle_manifest=$(cd -- "$(dirname -- "${bundle_manifest}")" && pwd -P)/$(basename -- "${bundle_manifest}")
bundle_signature=$(cd -- "$(dirname -- "${bundle_signature}")" && pwd -P)/$(basename -- "${bundle_signature}")
sync_public_key=$(cd -- "$(dirname -- "${sync_public_key}")" && pwd -P)/$(basename -- "${sync_public_key}")
[[ -f ${common} && -f ${lock} && -f ${bundle_manifest} && -f ${bundle_signature} && -f ${sync_public_key} ]]

if [[ ! ${approved_factory_sha256} =~ ^[a-f0-9]{64}$ ]]; then
  echo "approved factory SHA-256 must be 64 lowercase hexadecimal characters" >&2
  exit 2
fi
factory_bin="${root}/tools/portage-image-factory-linux-amd64"
actual_factory_sha256=$(sha256sum "${factory_bin}" | awk '{print $1}')
if [[ ${actual_factory_sha256} != "${approved_factory_sha256}" ]]; then
  echo "factory binary does not match the independently approved SHA-256" >&2
  exit 1
fi

# The lock is not trusted until an out-of-band-approved verifier checks the
# independent sync signature, freshness window, lock digest and every object.
"${factory_bin}" bundle-verify \
  -manifest "${bundle_manifest}" \
  -signature "${bundle_signature}" \
  -public-key "${sync_public_key}" \
  -lock "${lock}" \
  -root "${root}" >/dev/null

python3 - "${target}" "${root}" "${lock}" <<'PY'
import hashlib
import json
import os
import pathlib
import stat
import sys

target, root_raw, lock_raw = sys.argv[1:]
root = pathlib.Path(root_raw).resolve(strict=True)
lock_path = pathlib.Path(lock_raw).resolve(strict=True)
with lock_path.open("r", encoding="utf-8") as stream:
    lock = json.load(stream)
if not isinstance(lock, dict) or lock.get("strict_offline") is not True or not isinstance(lock.get("objects"), list):
    raise SystemExit("invalid strict offline lock")

required = {
    "tools/portage-image-factory-linux-amd64": ("service-binary", True),
    "factory/run-offline.sh": ("script", True),
}
matches = {path: [] for path in required}
for item in lock["objects"]:
    if not isinstance(item, dict):
        continue
    path = item.get("path")
    if path in matches and (not item.get("required_for") or target in item.get("required_for", [])):
        matches[path].append(item)

for relative, (kind, executable) in required.items():
    entries = matches[relative]
    if len(entries) != 1:
        raise SystemExit(f"expected one locked {relative}, found {len(entries)}")
    item = entries[0]
    if item.get("kind") != kind or bool(item.get("executable")) != executable:
        raise SystemExit(f"locked execution metadata mismatch for {relative}")
    expected_digest = item.get("sha256")
    expected_size = item.get("size")
    if not isinstance(expected_digest, str) or len(expected_digest) != 64 or not isinstance(expected_size, int) or expected_size < 0:
        raise SystemExit(f"invalid locked digest metadata for {relative}")
    path = root / relative
    info = path.lstat()
    resolved = path.resolve(strict=True)
    try:
        resolved.relative_to(root)
    except ValueError:
        raise SystemExit(f"locked execution path escaped offline root: {relative}")
    if not stat.S_ISREG(info.st_mode) or path.is_symlink() or info.st_size != expected_size or not os.access(path, os.X_OK):
        raise SystemExit(f"locked execution file metadata mismatch: {relative}")
    digest = hashlib.sha256()
    with resolved.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    if digest.hexdigest() != expected_digest:
        raise SystemExit(f"locked execution digest mismatch: {relative}")
PY

export IMAGE_FACTORY_BIN="${factory_bin}"
export IMAGE_FACTORY_PLAN="${root}/plans/${target}.build.json"
exec "${root}/factory/run-offline.sh" "${target}" "${common}" "${root}" "${lock}"
