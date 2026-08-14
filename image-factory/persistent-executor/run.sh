#!/usr/bin/env bash
# shellcheck disable=SC2154 # PKR_VAR_* values are validated external inputs.
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
packer_bin=${PACKER_BIN:-packer}

fail() {
  echo "persistent executor build: $*" >&2
  exit 1
}

require_env() {
  local name=$1
  [[ -n ${!name:-} ]] || fail "missing required environment value ${name}"
}

for name in \
  PKR_VAR_proxmox_url PKR_VAR_proxmox_username \
  PKR_VAR_proxmox_node PKR_VAR_proxmox_pool PKR_VAR_proxmox_storage \
  PKR_VAR_source_vmid PKR_VAR_source_template_name \
  PKR_VAR_source_image_manifest PKR_VAR_source_image_manifest_sha256 \
  PKR_VAR_template_name PKR_VAR_template_description \
  PKR_VAR_executor_template_generation \
  PKR_VAR_capacity_provider PKR_VAR_execution_zone PKR_VAR_architecture \
  PKR_VAR_build_mode PKR_VAR_profile_id PKR_VAR_worker_image_id \
  PKR_VAR_worker_image_generation PKR_VAR_portage_server_binary \
  PKR_VAR_portage_server_sha256 PKR_VAR_portage_builder_binary \
  PKR_VAR_portage_builder_sha256 PKR_VAR_terraform_binary \
  PKR_VAR_terraform_sha256 PKR_VAR_terraform_proxmox_provider \
  PKR_VAR_terraform_proxmox_provider_sha256; do
  require_env "$name"
done

template_name=$PKR_VAR_template_name
source_vmid=$PKR_VAR_source_vmid
source_template_name=$PKR_VAR_source_template_name
source_manifest=$PKR_VAR_source_image_manifest
source_manifest_sha256=$PKR_VAR_source_image_manifest_sha256
proxmox_url=$PKR_VAR_proxmox_url
proxmox_username=$PKR_VAR_proxmox_username
proxmox_token=${PKR_VAR_proxmox_token:-}
proxmox_password=${PKR_VAR_proxmox_password:-}
proxmox_node=$PKR_VAR_proxmox_node
proxmox_insecure=${PKR_VAR_proxmox_insecure:-false}
server_sha256=$PKR_VAR_portage_server_sha256
builder_sha256=$PKR_VAR_portage_builder_sha256
terraform_sha256=$PKR_VAR_terraform_sha256
terraform_provider_sha256=$PKR_VAR_terraform_proxmox_provider_sha256
ssh_private_key_file=${PKR_VAR_ssh_private_key_file:-}
capacity_provider=$PKR_VAR_capacity_provider
execution_zone=$PKR_VAR_execution_zone
architecture=$PKR_VAR_architecture
build_mode=$PKR_VAR_build_mode
profile_id=$PKR_VAR_profile_id
worker_image_id=$PKR_VAR_worker_image_id
worker_image_generation=$PKR_VAR_worker_image_generation
egress_capability=${PKR_VAR_egress_capability:-}

command -v "$packer_bin" >/dev/null || fail "Packer is unavailable: ${packer_bin}"
command -v curl >/dev/null || fail "curl is unavailable for PVE source readback"
command -v jq >/dev/null || fail "jq is unavailable for source validation"
[[ $template_name == *persistent-executor* ]] ||
  fail "target template name must contain persistent-executor"
[[ $source_vmid =~ ^[1-9][0-9]*$ ]] || fail "source_vmid must be positive"
[[ $proxmox_url =~ ^https://[a-zA-Z0-9._:-]+/api2/json/?$ ]] ||
  fail "PVE URL must be an HTTPS /api2/json endpoint without credentials or query"
[[ $proxmox_username =~ ^[a-zA-Z0-9@.!_-]+$ ]] ||
  fail "PVE username or token ID is invalid"
if [[ -n $proxmox_token && -n $proxmox_password ]]; then
  fail "set exactly one of PKR_VAR_proxmox_token or PKR_VAR_proxmox_password"
fi
if [[ -z $proxmox_token && -z $proxmox_password ]]; then
  fail "missing PVE credential: set PKR_VAR_proxmox_token or PKR_VAR_proxmox_password"
fi
if [[ -n $proxmox_token ]]; then
  [[ $proxmox_username == *'!'* ]] ||
    fail "PVE token authentication requires user@realm!token-id"
  [[ $proxmox_token =~ ^[a-zA-Z0-9._~-]+$ ]] || fail "PVE token secret is invalid"
else
  [[ $proxmox_username != *'!'* ]] ||
    fail "PVE password authentication requires user@realm without a token ID"
fi
[[ $proxmox_node =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]+$ ]] || fail "PVE node is invalid"
[[ $proxmox_insecure == true || $proxmox_insecure == false ]] ||
  fail "PKR_VAR_proxmox_insecure must be true or false"
[[ $source_template_name =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]+$ ]] ||
  fail "source template name is invalid"
[[ -f $source_manifest && ! -L $source_manifest ]] ||
  fail "source image manifest must be a regular non-symlink file"
[[ $source_manifest_sha256 =~ ^[a-f0-9]{64}$ ]] ||
  fail "source image manifest SHA-256 is invalid"
[[ $server_sha256 =~ ^[a-f0-9]{64}$ ]] ||
  fail "portage server SHA-256 is invalid"
[[ $builder_sha256 =~ ^[a-f0-9]{64}$ ]] ||
  fail "portage builder SHA-256 is invalid"
[[ $terraform_sha256 =~ ^[a-f0-9]{64}$ ]] || fail "Terraform SHA-256 is invalid"
[[ $terraform_provider_sha256 =~ ^[a-f0-9]{64}$ ]] ||
  fail "Terraform Proxmox provider SHA-256 is invalid"
if [[ -n $egress_capability ]]; then
  [[ $egress_capability =~ ^egress:[a-zA-Z0-9][a-zA-Z0-9+._/-]*@sha256:[a-f0-9]{64}$ ]] ||
    fail "egress capability must bind policy ID and digest as egress:<id>@sha256:<hex>"
fi
if [[ -n $ssh_private_key_file ]]; then
  [[ -f $ssh_private_key_file && ! -L $ssh_private_key_file ]] ||
    fail "SSH private key must be a regular non-symlink file"
  [[ $(stat -c '%a' "$ssh_private_key_file" 2>/dev/null || stat -f '%Lp' "$ssh_private_key_file") =~ ^(400|600)$ ]] ||
    fail "SSH private key must be owner-only"
fi

verify_binary_digest() {
  local binary_name=$1
  local path_name=$2
  local digest_name=$3
  [[ -f ${!path_name} && ! -L ${!path_name} ]] ||
    fail "${binary_name} must be a regular non-symlink file"
  actual=$(sha256sum "${!path_name}" 2>/dev/null | awk '{print $1}') ||
    actual=$(shasum -a 256 "${!path_name}" | awk '{print $1}')
  [[ $actual == "${!digest_name}" ]] || fail "${binary_name} digest mismatch"
}

verify_binary_digest portage_server \
  PKR_VAR_portage_server_binary PKR_VAR_portage_server_sha256
verify_binary_digest portage_builder \
  PKR_VAR_portage_builder_binary PKR_VAR_portage_builder_sha256
verify_binary_digest terraform \
  PKR_VAR_terraform_binary PKR_VAR_terraform_sha256
verify_binary_digest terraform_proxmox_provider \
  PKR_VAR_terraform_proxmox_provider PKR_VAR_terraform_proxmox_provider_sha256

source_manifest_actual=$(sha256sum "$source_manifest" 2>/dev/null | awk '{print $1}') ||
  source_manifest_actual=$(shasum -a 256 "$source_manifest" | awk '{print $1}')
[[ $source_manifest_actual == "$source_manifest_sha256" ]] ||
  fail "source image manifest digest mismatch"
jq -e 'type == "object"' "$source_manifest" >/dev/null ||
  fail "source image manifest is not a JSON object"

source_url="${proxmox_url%/}/nodes/${proxmox_node}/qemu/${source_vmid}/config"
curl_insecure=()
if [[ $proxmox_insecure == true ]]; then
  curl_insecure=(--insecure)
fi
if [[ -n $proxmox_token ]]; then
  source_config=$(
    curl --silent --show-error --fail "${curl_insecure[@]}" --config - <<EOF
url = "${source_url}"
header = "Authorization: PVEAPIToken=${proxmox_username}=${proxmox_token}"
EOF
  )
else
  encoded_username=$(jq -nr --arg value "$proxmox_username" '$value | @uri')
  encoded_password=$(jq -nr --arg value "$proxmox_password" '$value | @uri')
  login_response=$(
    printf 'username=%s&password=%s' "$encoded_username" "$encoded_password" |
      curl --silent --show-error --fail "${curl_insecure[@]}" \
        --header 'Content-Type: application/x-www-form-urlencoded' \
        --data-binary @- "${proxmox_url%/}/access/ticket"
  )
  pve_ticket=$(jq -er '.data.ticket | select(type == "string" and length > 0)' \
    <<<"$login_response") || fail "PVE password authentication returned no ticket"
  source_config=$(
    curl --silent --show-error --fail "${curl_insecure[@]}" --config - <<EOF
url = "${source_url}"
header = "Cookie: PVEAuthCookie=${pve_ticket}"
EOF
  )
  unset encoded_password login_response pve_ticket
fi
jq -e \
  --arg name "$source_template_name" \
  --arg marker "portage-engine-provenance=sha256:${source_manifest_sha256}" \
  '.data.template == 1 and .data.name == $name and
   (.data.ciupgrade | tostring) == "0" and .data.ciuser == "root" and
   .data.ipconfig0 == "ip=dhcp" and (.data.description | contains($marker))' \
  <<<"$source_config" >/dev/null ||
  fail "PVE source template identity/provenance/cloud-init readback mismatch"

pool_digest=$(
  printf '%s\0%s\0%s\0%s\0%s\0%s\0%s' \
    "$capacity_provider" "$execution_zone" "$architecture" "$build_mode" \
    "$profile_id" "$worker_image_id" "$worker_image_generation" |
    { sha256sum 2>/dev/null || shasum -a 256; } | awk '{print substr($1, 1, 24)}'
)
export PKR_VAR_capacity_pool_id="${capacity_provider}-${execution_zone}-${architecture}-${pool_digest}"

mkdir -p "$script_dir/output"
cd "$script_dir"
"$packer_bin" validate template.pkr.hcl
"$packer_bin" build template.pkr.hcl

echo "persistent executor candidate built: ${template_name}"
echo "capacity pool: ${PKR_VAR_capacity_pool_id}"
