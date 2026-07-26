#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

if [[ $# -ne 7 ]]; then
  echo "usage: $0 QCOW2 MANIFEST VMID NAME STORAGE BRIDGE IMAGE_FACTORY_BIN" >&2
  exit 2
fi
qcow2=$1; manifest=$2; vmid=$3; name=$4; storage=$5; bridge=$6; factory_bin=$7
[[ ${vmid} =~ ^[1-9][0-9]{2,8}$ && ${name} =~ ^[a-zA-Z0-9._-]{1,128}$ && ${storage} =~ ^[a-zA-Z0-9._-]{1,128}$ && ${bridge} =~ ^[a-zA-Z0-9._-]{1,128}$ ]]
[[ -x ${factory_bin} && -s ${qcow2} && -s ${manifest} ]]
for command in qm sha256sum; do command -v "${command}" >/dev/null || { echo "missing ${command}" >&2; exit 1; }; done
if qm status "${vmid}" >/dev/null 2>&1; then
  echo "refusing to modify existing VMID ${vmid}" >&2
  exit 1
fi
created=0
success=0
report_recovery() {
  if [[ ${created} -eq 1 && ${success} -ne 1 ]]; then
    echo "PVE import failed; VMID ${vmid} was retained for explicit recovery" >&2
  fi
}
trap report_recovery EXIT
check_output=$("${factory_bin}" qcow2-check -manifest "${manifest}" -qcow2 "${qcow2}")
manifest_digest=${check_output#manifest_digest=}
[[ ${manifest_digest} =~ ^sha256:[a-f0-9]{64}$ ]]

qm create "${vmid}" --name "${name}" --machine q35 --bios ovmf --agent enabled=1 --net0 "virtio,bridge=${bridge}" --description "Catalyst seed | portage-engine-provenance=${manifest_digest}"
created=1
# IMG-2 installs an unsigned removable-path GRUB. Do not enable the Microsoft
# Secure Boot trust store until a reviewed shim/GRUB/kernel signing chain exists.
qm set "${vmid}" --efidisk0 "${storage}:0,efitype=4m,pre-enrolled-keys=0"
qm importdisk "${vmid}" "${qcow2}" "${storage}"
unused_line=$(qm config "${vmid}" | awk '/^unused[0-9]+: / { print }')
[[ -n ${unused_line} && ${unused_line} != *$'\n'* ]] || { echo "expected exactly one imported unused disk" >&2; exit 1; }
unused_key=${unused_line%%:*}
imported_volume=${unused_line#*: }
[[ ${unused_key} =~ ^unused[0-9]+$ && -n ${imported_volume} ]]
qm set "${vmid}" --scsihw virtio-scsi-pci --scsi0 "${imported_volume}" --ide2 "${storage}:cloudinit" --boot order=scsi0 --ciupgrade 0 --ciuser root --ipconfig0 ip=dhcp
qm template "${vmid}"
success=1
echo "Imported immutable PVE seed ${name} (${vmid}); run the existing source-check/Packer/Terraform Gate next."
