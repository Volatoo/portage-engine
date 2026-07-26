#!/usr/bin/env bash
set -Eeuo pipefail
umask 022

if [[ ${EUID} -ne 0 || $(uname -s) != Linux ]]; then
  echo "image closure generation requires a root Linux Gentoo disposable clone" >&2
  exit 2
fi
for command in emerge git python3 realpath stat unshare; do
  command -v "${command}" >/dev/null || { echo "missing image closure command: ${command}" >&2; exit 1; }
done
if [[ $# -ne 9 ]]; then
  echo "usage: $0 TARGET REPOSITORY_COMMIT PROFILE_SELECTOR PROFILE_REPOSITORY_COMMIT BASE_IMAGE_MANIFEST MIRROR PACKAGE_FILE OUTPUT_JSON EVIDENCE_DIR" >&2
  exit 2
fi

target=$1
repository_commit=$2
profile_selector=$3
profile_repository_commit=$4
base_image_manifest=$5
mirror=$6
package_file=$7
output=$8
evidence_dir=$9
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)

[[ ${target} =~ ^[a-zA-Z0-9][a-zA-Z0-9+._/-]{0,255}$ ]]
[[ ${repository_commit} =~ ^([a-f0-9]{40}|[a-f0-9]{64})$ ]]
[[ ${profile_repository_commit} =~ ^([a-f0-9]{40}|[a-f0-9]{64})$ ]]
[[ ${profile_selector} =~ ^[a-zA-Z0-9][a-zA-Z0-9+._-]{0,63}:[a-zA-Z0-9][a-zA-Z0-9+._/-]{0,255}$ ]]
[[ ${mirror} =~ ^https?://[a-zA-Z0-9._:-]+(/[a-zA-Z0-9._~:/%+-]*)?$ ]]
[[ -f ${package_file} && -x ${script_dir}/write-closure.py ]]
if [[ ! -f ${base_image_manifest} || -L ${base_image_manifest} ]]; then
  echo "base image manifest must be a regular non-symlink file" >&2
  exit 1
fi
manifest_size=$(stat -c %s -- "${base_image_manifest}")
if (( manifest_size < 256 || manifest_size > 16777216 )); then
  echo "base image manifest size is outside the reviewed bounds" >&2
  exit 1
fi
if [[ ! -d /var/db/pkg || ! -x /usr/bin/emerge ]]; then
  echo "disposable clone lacks a Portage VDB or emerge" >&2
  exit 1
fi

if [[ ! -d /var/db/repos/gentoo/.git ]]; then
  echo "accepted base lacks the locked Gentoo Git checkout" >&2
  exit 1
fi
actual_commit=$(git -C /var/db/repos/gentoo rev-parse HEAD)
if [[ ${actual_commit} != "${repository_commit}" ]]; then
  echo "Gentoo repository commit ${actual_commit} does not match ${repository_commit}" >&2
  exit 1
fi
if [[ -n $(git -C /var/db/repos/gentoo status --porcelain --untracked-files=no) ]]; then
  echo "accepted base Gentoo repository has tracked worktree drift" >&2
  exit 1
fi

profile_repository=${profile_selector%%:*}
profile_path=${profile_selector#*:}
profile_repository_root=/var/db/repos/${profile_repository}
if [[ ! -d ${profile_repository_root}/.git ]]; then
  echo "accepted base lacks the locked ${profile_repository} Git checkout" >&2
  exit 1
fi
actual_profile_commit=$(git -C "${profile_repository_root}" rev-parse HEAD)
if [[ ${actual_profile_commit} != "${profile_repository_commit}" ]]; then
  echo "profile repository commit ${actual_profile_commit} does not match ${profile_repository_commit}" >&2
  exit 1
fi
if [[ -n $(git -C "${profile_repository_root}" status --porcelain --untracked-files=no) ]]; then
  echo "accepted profile repository has tracked worktree drift" >&2
  exit 1
fi
profile_repository_tree=$(git -C "${profile_repository_root}" rev-parse 'HEAD^{tree}')
case /${profile_path}/ in
  *//*|*/./*|*/../*)
    echo "profile selector ${profile_selector} is not canonical" >&2
    exit 1
    ;;
esac
profile_source=/var/db/repos/${profile_repository}/profiles/${profile_path}
if [[ ! -d ${profile_source} || -L ${profile_source} ]]; then
  echo "profile selector ${profile_selector} does not resolve to a regular profile directory" >&2
  exit 1
fi
profile_source=$(realpath -e -- "${profile_source}")
case ${profile_source} in
  /var/db/repos/${profile_repository}/profiles/*) ;;
  *)
    echo "profile selector ${profile_selector} escapes its repository" >&2
    exit 1
    ;;
esac

mapfile -t packages < <(sed -e '/^[[:space:]]*$/d' -- "${package_file}")
if [[ ${#packages[@]} -eq 0 || ${#packages[@]} -gt 512 ]]; then
  echo "package file must contain 1..512 atoms" >&2
  exit 1
fi
for atom in "${packages[@]}"; do
  [[ ${atom} =~ ^[a-zA-Z0-9][a-zA-Z0-9+._-]*/[a-zA-Z0-9][a-zA-Z0-9+._-]*$ ]]
done

mkdir -p -- "${evidence_dir}" "$(dirname -- "${output}")"
work_root=$(mktemp -d /var/tmp/portage-engine-image-closure.XXXXXXXX)
chmod 0755 "${work_root}"
success=0
cleanup() {
  if [[ ${success} -eq 1 ]]; then
    rm -rf -- "${work_root}"
  else
    echo "image closure generation failed; retained ${work_root}" >&2
  fi
}
trap cleanup EXIT

config_root=${work_root}/config-root
distdir=${work_root}/distfiles
install -d -m 0755 -- "${config_root}/etc" "${config_root}/etc/portage" "${config_root}/etc/portage/package.use"
install -d -m 0775 -o portage -g portage -- "${distdir}"
cp -a -- /etc/portage/. "${config_root}/etc/portage/"
rm -f -- "${config_root}/etc/portage/make.profile"
if [[ -d ${config_root}/etc/portage/make.profile ]]; then
  rm -rf -- "${config_root}/etc/portage/make.profile"
fi
ln -s -- "${profile_source}" "${config_root}/etc/portage/make.profile"
cat >>"${config_root}/etc/portage/make.conf" <<EOF

# portage-engine image-derived closure
FEATURES="force-mirror"
GENTOO_MIRRORS="${mirror}"
DISTDIR="${distdir}"
EOF

# Bind the exact installed VDB before resolving the desktop profile transition.
# Promotion/smoke acceptance remains a host-side gate; this script is limited
# to a disposable clone of that accepted template.
python3 - "${base_image_manifest}" /var/db/pkg "${config_root}/etc/portage" "${profile_selector}" "${profile_repository_commit}" "${profile_repository_tree}" >"${evidence_dir}/resolver.json" <<'PY'
import hashlib
import json
import pathlib
import re
import stat
import sys

manifest_path = pathlib.Path(sys.argv[1])
manifest_bytes = manifest_path.read_bytes()
manifest = json.loads(manifest_bytes)
required = {
    "schema_version", "created_at", "target", "image_id", "generation",
    "provider", "arch", "build_mode", "template", "profile_id",
    "profile_path", "profile_repository", "repositories", "channel",
    "input_lock_digest", "build_plan_digest", "packer_manifest_digest",
    "packer_artifact_id", "image_digest", "rootfs_manifest_digest",
}
if not required.issubset(manifest) or manifest["schema_version"] != 1:
    raise SystemExit("base image manifest is incomplete")
if manifest["target"] != "base-systemd" or manifest["channel"] != "candidate":
    raise SystemExit("closure source must be a base-systemd candidate manifest")
digest_pattern = re.compile(r"^sha256:[a-f0-9]{64}$")
for field in ("input_lock_digest", "build_plan_digest", "packer_manifest_digest", "image_digest", "rootfs_manifest_digest"):
    if not digest_pattern.fullmatch(manifest[field]):
        raise SystemExit(f"base image manifest has invalid {field}")

def tree_digest(root: pathlib.Path, normalize_make_conf: bool = False) -> str:
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*"), key=lambda item: item.as_posix()):
        relative = path.relative_to(root).as_posix()
        mode = path.lstat().st_mode
        digest.update(relative.encode() + b"\0" + oct(stat.S_IMODE(mode)).encode() + b"\0")
        if path.is_symlink():
            digest.update(b"symlink\0" + path.readlink().as_posix().encode() + b"\0")
        elif path.is_file():
            contents = path.read_bytes()
            if normalize_make_conf and relative == "make.conf":
                contents = re.sub(rb'(?m)^DISTDIR=.*$', b'DISTDIR="<ephemeral-distdir>"', contents)
            digest.update(b"file\0" + contents)
        elif path.is_dir():
            digest.update(b"directory\0")
        else:
            raise SystemExit(f"unsupported evidence object: {path}")
    return digest.hexdigest()

print(json.dumps({
    "schema_version": 1,
    "profile_selector": sys.argv[4],
    "profile_repository_commit": sys.argv[5],
    "profile_repository_tree": sys.argv[6],
    "base_image_manifest_sha256": hashlib.sha256(manifest_bytes).hexdigest(),
    "base_image_digest": manifest["image_digest"],
    "base_image_id": manifest["image_id"],
    "base_generation": manifest["generation"],
    "base_template": manifest["template"],
    "accepted_base_clone_vdb": True,
    "vdb_sha256": tree_digest(pathlib.Path(sys.argv[2])),
    "with_bdeps": True,
    "ephemeral_distdir_normalized": True,
    "config_sha256": tree_digest(pathlib.Path(sys.argv[3]), True),
}, indent=2))
PY

unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy no_proxy NO_PROXY
resolve() {
  unshare --mount --propagation private -- /usr/bin/env -i \
    HOME=/root \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    LC_ALL=C.UTF-8 \
    PORTAGE_CONFIGROOT="${config_root}" \
    emerge "$@"
}

resolve --pretend --verbose --update --deep --newuse --with-bdeps=y @world \
  >"${evidence_dir}/world.depgraph.log" 2>&1
resolve --pretend --verbose --with-bdeps=y "${packages[@]}" \
  >"${evidence_dir}/package-sets.depgraph.log" 2>&1
resolve --fetchonly --verbose --update --deep --newuse --with-bdeps=y @world \
  >"${evidence_dir}/world.fetch.log" 2>&1
resolve --fetchonly --verbose --with-bdeps=y "${packages[@]}" \
  >"${evidence_dir}/package-sets.fetch.log" 2>&1

"${script_dir}/write-closure.py" \
  --target "${target}" \
  --repository-commit "${repository_commit}" \
  --mirror "${mirror}" \
  --distdir "${distdir}" \
  --output "${output}"

python3 - "${output}" >"${evidence_dir}/closure.summary.json" <<'PY'
import hashlib
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text(encoding="utf-8"))
print(json.dumps({
    "target": data["target"],
    "repository_commit": data["repository_commit"],
    "objects": len(data["objects"]),
    "bytes": sum(item["size"] for item in data["objects"]),
    "manifest_sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
}, indent=2))
PY

success=1
echo "wrote ${output} from the accepted base VDB with ${#packages[@]} requested package atoms"
