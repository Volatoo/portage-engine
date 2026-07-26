#!/usr/bin/env bash
set -Eeuo pipefail
umask 022

if [[ ${EUID} -ne 0 || $(uname -s) != Linux ]]; then
  echo "closure generation requires a root Linux Gentoo sync worker" >&2
  exit 2
fi
for command in chroot emerge mount python3 realpath stat tar unshare; do
  command -v "${command}" >/dev/null || { echo "missing closure command: ${command}" >&2; exit 1; }
done
if [[ $# -ne 8 ]]; then
  echo "usage: $0 TARGET REPOSITORY_COMMIT PROFILE_SELECTOR STAGE3_ARCHIVE MIRROR PACKAGE_FILE OUTPUT_JSON EVIDENCE_DIR" >&2
  exit 2
fi

target=$1
repository_commit=$2
profile_selector=$3
stage3_archive=$4
mirror=$5
package_file=$6
output=$7
evidence_dir=$8
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)

[[ ${target} =~ ^[a-zA-Z0-9][a-zA-Z0-9+._/-]{0,255}$ ]]
[[ ${repository_commit} =~ ^([a-f0-9]{40}|[a-f0-9]{64})$ ]]
[[ ${profile_selector} =~ ^[a-zA-Z0-9][a-zA-Z0-9+._-]{0,63}:[a-zA-Z0-9][a-zA-Z0-9+._/-]{0,255}$ ]]
[[ ${mirror} =~ ^https?://[a-zA-Z0-9._:-]+(/[a-zA-Z0-9._~:/%+-]*)?$ ]]
[[ -f ${package_file} && -x ${script_dir}/write-closure.py ]]
if [[ ! -f ${stage3_archive} || -L ${stage3_archive} ]]; then
  echo "stage3 archive must be a regular non-symlink file" >&2
  exit 1
fi
stage3_size=$(stat -c %s -- "${stage3_archive}")
if (( stage3_size < 104857600 || stage3_size > 4294967296 )); then
  echo "stage3 archive size is outside the reviewed bounds" >&2
  exit 1
fi

actual_commit=$(awk 'NR == 1 {print $1}' /var/db/repos/gentoo/metadata/timestamp.commit)
if [[ ${actual_commit} != "${repository_commit}" ]]; then
  echo "Gentoo repository commit ${actual_commit} does not match ${repository_commit}" >&2
  exit 1
fi

profile_repository=${profile_selector%%:*}
profile_path=${profile_selector#*:}
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
work_root=$(mktemp -d /var/tmp/portage-engine-closure.XXXXXXXX)
chmod 0755 "${work_root}"
success=0
cleanup() {
  if [[ ${success} -eq 1 ]]; then
    rm -rf -- "${work_root}"
  else
    echo "closure generation failed; retained ${work_root}" >&2
  fi
}
trap cleanup EXIT
distdir=${work_root}/distfiles
install -d -m 0775 -o portage -g portage -- "${distdir}"
resolver_root=${work_root}/resolver-root
resolver_config=${resolver_root}
install -d -m 0755 -- "${resolver_root}"
python3 - "${stage3_archive}" <<'PY'
import pathlib
import posixpath
import sys
import tarfile

archive = pathlib.Path(sys.argv[1])
members = 0
expanded = 0
with tarfile.open(archive, mode="r:xz") as source:
    for member in source:
        members += 1
        expanded += max(member.size, 0)
        if members > 1_000_000 or expanded > 32 * 1024**3:
            raise SystemExit("stage3 archive exceeds extraction bounds")
        name = member.name.removeprefix("./")
        normalized = posixpath.normpath(name)
        if normalized == ".":
            continue
        if not name or name.startswith("/") or normalized in {"", ".."} or normalized.startswith("../"):
            raise SystemExit(f"unsafe stage3 member: {member.name!r}")
        if member.islnk():
            target = posixpath.normpath(member.linkname)
            if member.linkname.startswith("/") or target == ".." or target.startswith("../"):
                raise SystemExit(f"unsafe stage3 hardlink: {member.name!r}")
if members == 0:
    raise SystemExit("stage3 archive is empty")
PY
# The official stage contains legitimate absolute systemd symlinks and /dev
# nodes. GNU tar safely preserves those symlinks without following them; the
# resolver does not need device nodes, so exclude /dev entirely.
tar --extract --xz --file "${stage3_archive}" --directory "${resolver_root}" \
  --no-same-owner --exclude='./dev/*'
if [[ ! -d ${resolver_root}/var/db/pkg || ! -x ${resolver_root}/usr/bin/emerge ]]; then
  echo "stage3 archive lacks a Portage VDB or emerge" >&2
  exit 1
fi
rm -rf -- "${resolver_config}/etc/portage/repos.conf"
rm -f -- "${resolver_config}/etc/portage/make.profile"
install -d -m 0755 -- \
  "${resolver_config}/etc/portage/repos.conf" \
  "${resolver_config}/etc/portage/package.use" \
  "${resolver_root}/dev" \
  "${resolver_root}/proc" \
  "${resolver_root}/sys" \
  "${resolver_root}/var/cache/distfiles" \
  "${resolver_root}/var/db/repos"
if [[ ! -d /etc/portage/repos.conf ]]; then
  echo "sync worker requires /etc/portage/repos.conf" >&2
  exit 1
fi
cp -a -- /etc/portage/repos.conf/. "${resolver_config}/etc/portage/repos.conf/"
ln -s -- "${profile_source}" "${resolver_config}/etc/portage/make.profile"
cat >"${resolver_config}/etc/portage/make.conf" <<EOF
LC_MESSAGES=C.UTF-8
FEATURES="force-mirror"
GENTOO_MIRRORS="${mirror}"
DISTDIR="/var/cache/distfiles"
USE="grub_platforms_efi-64"
EOF
# This is part of the Catalyst image contract: gentoo-kernel-bin must obtain
# its initramfs through installkernel/dracut. Keep it in the resolver as well
# as the generated offline build configuration.
cat >"${resolver_config}/etc/portage/package.use/portage-engine-image" <<'EOF'
sys-kernel/installkernel dracut
EOF

unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy no_proxy NO_PROXY
resolver_exec=${work_root}/resolver-exec.sh
cat >"${resolver_exec}" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
root=$1
distdir=$2
shift 2
mount --bind /var/db/repos "${root}/var/db/repos"
mount --rbind /dev "${root}/dev"
mount --make-rslave "${root}/dev"
mount -t proc proc "${root}/proc"
mount --rbind /sys "${root}/sys"
mount --make-rslave "${root}/sys"
mount --bind "${distdir}" "${root}/var/cache/distfiles"
exec chroot "${root}" /usr/bin/env -i \
  HOME=/root \
  PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  LC_ALL=C.UTF-8 \
  "$@"
EOF
chmod 0755 "${resolver_exec}"

resolve() {
  unshare --mount --propagation private -- \
    "${resolver_exec}" "${resolver_root}" "${distdir}" emerge "$@"
}

# Resolve against the same signed stage3 VDB consumed by Catalyst. Using the
# sync worker's installed VDB silently omitted dependencies that happened to
# exist on that worker but were absent from the stage3 (for example hwdata).
# A fully empty VDB is also wrong: it invents bootstrap cycles that Catalyst
# never encounters because the stage3 already contains @system. Run Portage
# inside that stage3 so both its target and build roots use the same VDB;
# private bind mounts expose only the reviewed repositories and clean DISTDIR.
resolve --pretend --verbose --update --deep --newuse --with-bdeps=y @world \
  >"${evidence_dir}/world.depgraph.log" 2>&1
resolve --pretend --verbose --update --deep --newuse --with-bdeps=y "${packages[@]}" \
  >"${evidence_dir}/package-sets.depgraph.log" 2>&1
resolve --fetchonly --verbose --update --deep --newuse --with-bdeps=y @world \
  >"${evidence_dir}/world.fetch.log" 2>&1
resolve --fetchonly --verbose --update --deep --newuse --with-bdeps=y "${packages[@]}" \
  >"${evidence_dir}/package-sets.fetch.log" 2>&1

python3 - "${resolver_config}/etc/portage" "${profile_selector}" "${stage3_archive}" >"${evidence_dir}/resolver.json" <<'PY'
import hashlib
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
digest = hashlib.sha256()
for path in sorted(root.rglob("*"), key=lambda item: item.as_posix()):
    relative = path.relative_to(root).as_posix()
    digest.update(relative.encode("utf-8") + b"\0")
    if path.is_symlink():
        digest.update(b"symlink\0" + path.readlink().as_posix().encode("utf-8") + b"\0")
    elif path.is_file():
        contents = path.read_bytes()
        if relative == "make.conf":
            contents = re.sub(rb"(?m)^DISTDIR=.*$", b'DISTDIR="<ephemeral-distdir>"', contents)
        digest.update(b"file\0" + contents)
stage3_digest = hashlib.sha256()
with pathlib.Path(sys.argv[3]).open("rb") as stream:
    for chunk in iter(lambda: stream.read(1024 * 1024), b""):
        stage3_digest.update(chunk)
print(json.dumps({
    "schema_version": 1,
    "profile_selector": sys.argv[2],
    "stage3_sha256": stage3_digest.hexdigest(),
    "signed_stage3_chroot_vdb": True,
    "with_bdeps": True,
    "ephemeral_distdir_normalized": True,
    "config_sha256": digest.hexdigest(),
}, indent=2))
PY

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
echo "wrote ${output} with ${#packages[@]} requested package atoms"
