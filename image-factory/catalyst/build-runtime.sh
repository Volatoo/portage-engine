#!/usr/bin/env bash
# Build a relocatable, digest-lockable Catalyst Python runtime from a dedicated
# minimal Gentoo sync worker. This script does not install or update packages.
set -Eeuo pipefail
umask 022

if [[ $# -ne 2 || $(uname -s) != Linux ]]; then
  echo "usage: $0 OUTPUT_ARCHIVE VERIFY_RUNTIME_PY" >&2
  exit 2
fi
output=$1
verify_script=$2
for command in python3 catalyst tar xz sha256sum stat; do
  command -v "${command}" >/dev/null || { echo "missing runtime build command: ${command}" >&2; exit 1; }
done
[[ -f ${verify_script} ]]
output_dir=$(cd -- "$(dirname -- "${output}")" && pwd -P)
output=${output_dir}/$(basename -- "${output}")
[[ ${output} == *.tar.xz ]]

work=$(mktemp -d /var/tmp/portage-catalyst-runtime.XXXXXXXX)
success=0
cleanup() {
  if [[ ${success} -eq 1 ]]; then
    rm -rf -- "${work}"
  else
    echo "runtime build failed; retained ${work}" >&2
  fi
}
trap cleanup EXIT
root=${work}/root
install -d -m 0755 "${root}/bin" "${root}/lib" "${root}/share"

readarray -t python_values < <(python3 -S - <<'PY'
import pathlib
import sys
import sysconfig

print(f"python{sys.version_info.major}.{sys.version_info.minor}")
print(pathlib.Path(sysconfig.get_path("purelib")).resolve())
PY
)
python_dir=${python_values[0]}
site_packages=${python_values[1]}
[[ -d ${site_packages} && ${site_packages} == /usr/lib/python*/site-packages ]]

# The worker is intentionally minimal. Copying its complete system
# site-packages tree captures lazy Catalyst dependencies as well as immediate
# imports; Python -S and verify-runtime.py prevent host/user package fallback.
install -d -m 0755 "${root}/lib/${python_dir}"
cp -a --no-preserve=ownership "${site_packages}" "${root}/lib/${python_dir}/site-packages"
install -m 0755 "$(command -v catalyst)" "${root}/bin/catalyst"
[[ -d /usr/share/catalyst ]]
cp -a --no-preserve=ownership /usr/share/catalyst "${root}/share/catalyst"

runtime_site=${root}/lib/${python_dir}/site-packages
PYTHONPATH=${runtime_site} PYTHONNOUSERSITE=1 python3 -S "${verify_script}" "${runtime_site}" "${root}"

cat >"${root}/RUNTIME.json" <<EOF
{
  "schema_version": 1,
  "python": "${python_dir}",
  "catalyst_version": "$(catalyst --version 2>&1 | head -n 1 | sed 's/["\\]/_/g')"
}
EOF
chmod 0644 "${root}/RUNTIME.json"

epoch=${SOURCE_DATE_EPOCH:-0}
[[ ${epoch} =~ ^[0-9]+$ ]]
temporary=${output}.tmp.$$
tar --sort=name --mtime="@${epoch}" --owner=0 --group=0 --numeric-owner \
  --pax-option=delete=atime,delete=ctime -C "${root}" -cJf "${temporary}" .
chmod 0644 "${temporary}"
mv -f -- "${temporary}" "${output}"
success=1
printf 'runtime=%s\nsize=%s\nsha256=%s\n' \
  "${output}" "$(stat -c '%s' -- "${output}")" "$(sha256sum "${output}" | awk '{print $1}')"
