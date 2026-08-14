#!/usr/bin/env bash
set -euo pipefail

require_value() {
  local name=$1
  if [[ -z ${!name:-} ]]; then
    echo "missing required persistent executor value: ${name}" >&2
    exit 1
  fi
}

for name in \
  PE_EXECUTOR_TEMPLATE_GENERATION PE_CAPACITY_POOL_ID PE_CAPACITY_PROVIDER \
  PE_EXECUTION_ZONE PE_ARCHITECTURE PE_BUILD_MODE PE_PROFILE_ID \
  PE_WORKER_IMAGE_ID PE_WORKER_IMAGE_GENERATION PE_PORTAGE_SERVER_SHA256 \
  PE_PORTAGE_BUILDER_SHA256 PE_TERRAFORM_SHA256 \
  PE_TERRAFORM_PROXMOX_PROVIDER_SHA256 PE_SOURCE_IMAGE_MANIFEST_SHA256; do
  require_value "$name"
done

for value in \
  "$PE_EXECUTOR_TEMPLATE_GENERATION" "$PE_CAPACITY_PROVIDER" \
  "$PE_EXECUTION_ZONE" "$PE_ARCHITECTURE" "$PE_BUILD_MODE" \
  "$PE_WORKER_IMAGE_GENERATION"; do
  [[ $value =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]]
done
[[ $PE_CAPACITY_POOL_ID =~ ^[a-z][a-z0-9-]{2,159}$ ]]
[[ $PE_PROFILE_ID =~ ^[a-zA-Z0-9][a-zA-Z0-9+._/-]+$ ]]
[[ $PE_WORKER_IMAGE_ID =~ ^[a-zA-Z0-9][a-zA-Z0-9+._/-]+$ ]]
[[ $PE_PORTAGE_SERVER_SHA256 =~ ^[a-f0-9]{64}$ ]]
[[ $PE_PORTAGE_BUILDER_SHA256 =~ ^[a-f0-9]{64}$ ]]
[[ $PE_TERRAFORM_SHA256 =~ ^[a-f0-9]{64}$ ]]
[[ $PE_TERRAFORM_PROXMOX_PROVIDER_SHA256 =~ ^[a-f0-9]{64}$ ]]
[[ $PE_SOURCE_IMAGE_MANIFEST_SHA256 =~ ^[a-f0-9]{64}$ ]]
if [[ -n ${PE_EGRESS_CAPABILITY:-} ]]; then
  [[ $PE_EGRESS_CAPABILITY =~ ^egress:[a-zA-Z0-9][a-zA-Z0-9+._/-]*@sha256:[a-f0-9]{64}$ ]]
fi

expected_pool="${PE_CAPACITY_PROVIDER}-${PE_EXECUTION_ZONE}-${PE_ARCHITECTURE}-$(
  printf '%s\0%s\0%s\0%s\0%s\0%s\0%s' \
    "$PE_CAPACITY_PROVIDER" "$PE_EXECUTION_ZONE" "$PE_ARCHITECTURE" \
    "$PE_BUILD_MODE" "$PE_PROFILE_ID" "$PE_WORKER_IMAGE_ID" \
    "$PE_WORKER_IMAGE_GENERATION" | sha256sum | cut -c1-24
)"
if [[ $PE_CAPACITY_POOL_ID != "$expected_pool" ]]; then
  echo "capacity pool identity mismatch: got ${PE_CAPACITY_POOL_ID}, expected ${expected_pool}" >&2
  exit 1
fi

for pair in \
  "/tmp/portage-server:$PE_PORTAGE_SERVER_SHA256" \
  "/tmp/portage-builder:$PE_PORTAGE_BUILDER_SHA256" \
  "/tmp/terraform:$PE_TERRAFORM_SHA256" \
  "/tmp/terraform-provider-proxmox.zip:$PE_TERRAFORM_PROXMOX_PROVIDER_SHA256" \
  "/tmp/source-image-manifest.json:$PE_SOURCE_IMAGE_MANIFEST_SHA256"; do
  path=${pair%%:*}
  expected=${pair#*:}
  actual=$(sha256sum "$path" | awk '{print $1}')
  if [[ $actual != "$expected" ]]; then
    echo "service binary digest mismatch for ${path}" >&2
    exit 1
  fi
done

provider_mirror=/opt/portage-engine/terraform/providers/registry.terraform.io/telmate/proxmox
install -d -m 0755 \
  /usr/local/libexec/portage-engine /etc/portage-engine /etc/terraform.d \
  "$provider_mirror"
install -m 0755 /tmp/portage-server /usr/local/bin/portage-server
install -m 0755 /tmp/portage-builder \
  "/usr/local/libexec/portage-engine/portage-builder-linux-${PE_ARCHITECTURE}"
install -m 0755 /tmp/capacity-executor-identity \
  /usr/local/libexec/portage-engine/capacity-executor-identity
install -m 0644 /tmp/portage-capacity-executor.service \
  /etc/systemd/system/portage-capacity-executor.service

# A re-templated cloud-init guest can briefly boot with the previous Packer
# VM's generated networkd file. If networkd evaluates that stale MAC before
# cloud-init replaces the file, the new NIC remains unmanaged even though
# qemu-guest-agent is already responding. Refresh networkd after cloud-init's
# network stage so the current clone MAC is always applied before the executor
# service can start.
cat >/usr/local/libexec/portage-engine/cloud-init-network-refresh <<'EOF'
#!/bin/sh
set -eu

network_file=/etc/systemd/network/10-cloud-init-eth0.network
if ! grep -q '^ClientIdentifier=mac$' "$network_file"; then
  printf '\n[DHCPv4]\nClientIdentifier=mac\n' >>"$network_file"
fi
networkctl reload
attempt=0
while [ "$attempt" -lt 30 ]; do
  if [ -e /sys/class/net/eth0 ]; then
    networkctl reconfigure eth0
    exit 0
  fi
  attempt=$((attempt + 1))
  sleep 1
done
echo 'cloud-init network refresh: eth0 did not appear' >&2
exit 1
EOF
chmod 0755 /usr/local/libexec/portage-engine/cloud-init-network-refresh

cat >/etc/systemd/system/portage-cloud-init-network-refresh.service <<'EOF'
[Unit]
Description=Refresh cloud-init networkd configuration for the current clone
After=systemd-networkd.service cloud-init-network.service
Wants=systemd-networkd.service
Before=portage-capacity-executor.service
ConditionPathExists=/etc/systemd/network/10-cloud-init-eth0.network

[Service]
Type=oneshot
ExecStart=/usr/local/libexec/portage-engine/cloud-init-network-refresh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
install -m 0755 /tmp/terraform /usr/local/bin/terraform
install -m 0644 /tmp/terraform-provider-proxmox.zip \
  "$provider_mirror/terraform-provider-proxmox_3.0.2-rc04_linux_${PE_ARCHITECTURE}.zip"
cat >/etc/terraform.d/portage-engine.tfrc <<'EOF'
provider_installation {
  filesystem_mirror {
    path    = "/opt/portage-engine/terraform/providers"
    include = ["registry.terraform.io/telmate/proxmox"]
  }
}
EOF
chmod 0644 /etc/terraform.d/portage-engine.tfrc
install -d -m 0755 /etc/portage-engine/evidence
install -m 0644 /tmp/source-image-manifest.json \
  /etc/portage-engine/evidence/source-image-manifest.json

capabilities="capacity-pool:${PE_CAPACITY_POOL_ID},provider:${PE_CAPACITY_PROVIDER},zone:${PE_EXECUTION_ZONE},arch:${PE_ARCHITECTURE},build-mode:${PE_BUILD_MODE},profile:${PE_PROFILE_ID},image:${PE_WORKER_IMAGE_ID}@${PE_WORKER_IMAGE_GENERATION},phase:provision,phase:build,phase:verify,phase:publish"
if [[ -n ${PE_EGRESS_CAPABILITY:-} ]]; then
  capabilities="${capabilities},${PE_EGRESS_CAPABILITY}"
fi

umask 027
{
  echo "PHASE_EXECUTOR_MODE=active"
  echo "WORKER_GATEWAY_ENABLED=true"
  echo "SCHEDULER_AUTOSCALE_MODE=off"
  echo "MAX_WORKERS=1"
  echo "CLOUD_DEFAULT_PROVIDER=${PE_CAPACITY_PROVIDER}"
  echo "BUILD_MODE=${PE_BUILD_MODE}"
  echo "EXECUTOR_ZONES=${PE_EXECUTION_ZONE}"
  echo "EXECUTOR_CAPABILITIES=${capabilities}"
  echo "CLOUD_BUILDER_BINARY_PATH=/usr/local/libexec/portage-engine/portage-builder-linux-${PE_ARCHITECTURE}"
  echo "TF_CLI_CONFIG_FILE=/etc/terraform.d/portage-engine.tfrc"
} >/etc/portage-engine/executor.env
chmod 0640 /etc/portage-engine/executor.env

cat >/usr/share/portage-engine-executor-runtime.example <<'EOF'
# Runtime-only settings. Provision this file outside the image as
# /etc/portage-engine/executor.conf before starting the service.
DATABASE_ENABLED=true
DATABASE_REQUIRED=true
WORKER_GATEWAY_ADVERTISE_URL=https://worker-gateway.example.internal:9443
WORKER_GATEWAY_SERVER_CA=/run/portage-engine/server-ca.pem
WORKER_GATEWAY_CLIENT_CA=/run/portage-engine/worker-client-ca.pem
WORKER_GATEWAY_ISSUER_ID=executor-vault
WORKER_GATEWAY_ISSUER_PROVIDER=vault
WORKER_GATEWAY_VAULT_ADDRESS=https://vault.example.internal:8200
WORKER_GATEWAY_VAULT_MOUNT=pki
WORKER_GATEWAY_VAULT_ROLE=portage-worker
WORKER_GATEWAY_VAULT_TOKEN_FILE=/run/credentials/portage-capacity-executor/vault-token
EOF
chmod 0644 /usr/share/portage-engine-executor-runtime.example

systemctl daemon-reload
systemctl enable \
  portage-cloud-init-network-refresh.service \
  portage-capacity-executor.service
