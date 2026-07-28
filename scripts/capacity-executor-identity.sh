#!/usr/bin/env bash
set -euo pipefail

identity_path="/sys/class/dmi/id/product_uuid"
output_dir="/run/portage-engine"
output_path="${output_dir}/capacity-executor.env"

if [[ ! -r "${identity_path}" ]]; then
  echo "capacity executor cannot read SMBIOS product UUID" >&2
  exit 1
fi

instance_id="$(tr '[:upper:]' '[:lower:]' <"${identity_path}" | tr -d '[:space:]')"
if [[ ! "${instance_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]]; then
  echo "capacity executor SMBIOS product UUID is invalid" >&2
  exit 1
fi

install -d -m 0750 -o root -g root "${output_dir}"
umask 027
tmp_path="$(mktemp "${output_dir}/capacity-executor.env.XXXXXX")"
trap 'rm -f "${tmp_path}"' EXIT
{
  echo "SERVER_RUNTIME_ROLE=executor"
  echo "CONTROL_PLANE_ID=capacity-executor-${instance_id}"
  echo "EXECUTOR_CAPACITY_INSTANCE_ID=${instance_id}"
} >"${tmp_path}"
chmod 0640 "${tmp_path}"
mv -f "${tmp_path}" "${output_path}"
trap - EXIT
