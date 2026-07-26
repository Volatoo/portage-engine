#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

if [[ ${STRICT_OFFLINE:-} != 1 || $(uname -s) != Linux || ${EUID} -ne 0 ]]; then
  echo "STRICT_OFFLINE=1 and a root Linux worker are required" >&2
  exit 2
fi
if [[ $# -ne 4 ]]; then
  echo "usage: $0 ROOTFS_TAR ROOTFS_MANIFEST OUTPUT_QCOW2 IMAGE_FACTORY_BIN" >&2
  exit 2
fi
rootfs_tar=$1
rootfs_manifest=$2
output=$3
factory_bin=$4
[[ ! -e ${output} ]] || { echo "refusing to overwrite ${output}" >&2; exit 1; }

for command in qemu-img sgdisk losetup mkfs.vfat mkfs.ext4 mount umount tar chroot blkid truncate; do
  command -v "${command}" >/dev/null || { echo "missing required command: ${command}" >&2; exit 1; }
done
[[ -x ${factory_bin} && -s ${rootfs_tar} && -s ${rootfs_manifest} ]] || { echo "missing input artifact" >&2; exit 1; }

readarray -t manifest_values < <(python3 - "${rootfs_manifest}" <<'PY'
import json, pathlib, sys
d = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
print(d["rootfs_filename"])
print(d["rootfs_digest"])
print(d["qcow2_filename"])
print(d["disk_size_gib"])
PY
)
[[ $(basename -- "${rootfs_tar}") == "${manifest_values[0]}" && $(basename -- "${output}") == "${manifest_values[2]}" ]]
actual_rootfs="sha256:$(sha256sum "${rootfs_tar}" | awk '{print $1}')"
[[ ${actual_rootfs} == "${manifest_values[1]}" ]] || { echo "rootfs digest mismatch" >&2; exit 1; }
disk_size=${manifest_values[3]}

work_root=$(mktemp -d "${TMPDIR:-/var/tmp}/portage-engine-qcow2.XXXXXXXX")
raw="${work_root}/disk.raw"
mount_root="${work_root}/root"
loop_device=""
mounted=()
cleanup() {
  set +e
  for ((index=${#mounted[@]}-1; index>=0; index--)); do umount -R -- "${mounted[index]}"; done
  [[ -z ${loop_device} ]] || losetup -d "${loop_device}"
  rm -rf -- "${work_root}"
}
trap cleanup EXIT

truncate -s "${disk_size}G" "${raw}"
sgdisk --zap-all --new=1:1MiB:+256MiB --typecode=1:EF00 --change-name=1:EFI --new=2:0:0 --typecode=2:8304 --change-name=2:rootfs "${raw}"
loop_device=$(losetup --find --show --partscan "${raw}")
efi_partition="${loop_device}p1"
root_partition="${loop_device}p2"
for _ in {1..50}; do [[ -b ${root_partition} ]] && break; sleep 0.1; done
[[ -b ${efi_partition} && -b ${root_partition} ]]
mkfs.vfat -F 32 -n EFI "${efi_partition}"
mkfs.ext4 -F -L rootfs "${root_partition}"
mkdir -p -- "${mount_root}"
mount "${root_partition}" "${mount_root}"
mounted+=("${mount_root}")
tar --numeric-owner --xattrs --xattrs-include='*' -xpf "${rootfs_tar}" -C "${mount_root}"
mkdir -p -- "${mount_root}/boot/efi" "${mount_root}/dev" "${mount_root}/proc" "${mount_root}/sys" "${mount_root}/run"
mount "${efi_partition}" "${mount_root}/boot/efi"
mounted+=("${mount_root}/boot/efi")
for source in /dev /proc /sys /run; do
  mount --rbind "${source}" "${mount_root}${source}"
  mount --make-rslave "${mount_root}${source}"
  mounted+=("${mount_root}${source}")
done
printf '%s\n' 'LABEL=rootfs / ext4 defaults,noatime 0 1' 'LABEL=EFI /boot/efi vfat umask=0077 0 2' >"${mount_root}/etc/fstab"
# The positional parameter is intentionally expanded by the chroot shell.
# shellcheck disable=SC2016
chroot "${mount_root}" /bin/sh -ceu '
  command -v grub-install >/dev/null
  command -v grub-mkconfig >/dev/null
  command -v cloud-init >/dev/null
  command -v qemu-ga >/dev/null
  kernel_count=0
  for candidate in /boot/vmlinuz-* /boot/kernel-*; do
    test -f "$candidate" || continue
    kernel_count=$((kernel_count + 1))
  done
  test "$kernel_count" -eq 1
  grub-install --target=x86_64-efi --efi-directory=/boot/efi --bootloader-id=Gentoo --removable --no-nvram
  grub-mkconfig -o /boot/grub/grub.cfg
'
: >"${mount_root}/etc/machine-id"
chmod 0644 -- "${mount_root}/etc/machine-id"
rm -f -- "${mount_root}"/etc/ssh/ssh_host_*
sync
for ((index=${#mounted[@]}-1; index>=0; index--)); do umount -R -- "${mounted[index]}"; done
mounted=()
losetup -d "${loop_device}"
loop_device=""
qemu-img convert -p -f raw -O qcow2 -o compat=1.1,lazy_refcounts=on "${raw}" "${output}"
qemu-img check "${output}"
qcow_virtual_size=$(qemu-img info --output=json "${output}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["virtual-size"])')
expected_virtual_size=$((disk_size * 1024 * 1024 * 1024))
[[ ${qcow_virtual_size} -eq ${expected_virtual_size} ]] || { echo "QCOW2 virtual size does not match rootfs manifest" >&2; exit 1; }
"${factory_bin}" qcow2-manifest -rootfs-manifest "${rootfs_manifest}" -qcow2 "${output}" -assembler "$0" -output "${output}.manifest.json"
echo "QCOW2 and manifest written to ${output} and ${output}.manifest.json"
