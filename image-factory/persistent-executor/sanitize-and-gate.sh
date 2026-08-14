#!/usr/bin/env bash
set -euo pipefail

fail() {
  echo "persistent executor image gate: $*" >&2
  exit 1
}

for command_name in cloud-init systemctl sha256sum qemu-ga terraform; do
  command -v "$command_name" >/dev/null || fail "missing ${command_name}"
done

systemctl is-enabled portage-capacity-executor.service >/dev/null
systemctl is-enabled portage-cloud-init-network-refresh.service >/dev/null
test -x /usr/local/bin/portage-server
test -x "/usr/local/libexec/portage-engine/portage-builder-linux-${PE_ARCHITECTURE}"
test -x /usr/local/libexec/portage-engine/capacity-executor-identity
test -x /usr/local/libexec/portage-engine/cloud-init-network-refresh
test -r /etc/terraform.d/portage-engine.tfrc
grep -Fq 'filesystem_mirror {' /etc/terraform.d/portage-engine.tfrc
if grep -Fq 'direct {' /etc/terraform.d/portage-engine.tfrc; then
  fail "Terraform provider configuration permits a direct registry fallback"
fi
test ! -e /etc/portage-engine/executor.conf
test ! -e /etc/portage-engine/executor-runtime.env

grep -Fxq 'WORKER_GATEWAY_ENABLED=true' /etc/portage-engine/executor.env
grep -Fxq "EXECUTOR_ZONES=${PE_EXECUTION_ZONE}" /etc/portage-engine/executor.env
grep -Fq "capacity-pool:${PE_CAPACITY_POOL_ID}" /etc/portage-engine/executor.env
grep -Fq "provider:${PE_CAPACITY_PROVIDER}" /etc/portage-engine/executor.env
grep -Fq "zone:${PE_EXECUTION_ZONE}" /etc/portage-engine/executor.env
grep -Fq "arch:${PE_ARCHITECTURE}" /etc/portage-engine/executor.env
grep -Fq "build-mode:${PE_BUILD_MODE}" /etc/portage-engine/executor.env
grep -Fq "profile:${PE_PROFILE_ID}" /etc/portage-engine/executor.env
grep -Fq "image:${PE_WORKER_IMAGE_ID}@${PE_WORKER_IMAGE_GENERATION}" \
  /etc/portage-engine/executor.env
if [[ -n ${PE_EGRESS_CAPABILITY:-} ]]; then
  grep -Fq ",${PE_EGRESS_CAPABILITY}" /etc/portage-engine/executor.env ||
    fail "bound egress capability is absent from executor environment"
fi
if grep -Fq 'capacity-instance:' /etc/portage-engine/executor.env; then
  fail "capacity instance identity was baked instead of derived from SMBIOS"
fi

for pair in \
  "/usr/local/bin/portage-server:$PE_PORTAGE_SERVER_SHA256" \
  "/usr/local/libexec/portage-engine/portage-builder-linux-${PE_ARCHITECTURE}:$PE_PORTAGE_BUILDER_SHA256" \
  "/usr/local/bin/terraform:$PE_TERRAFORM_SHA256" \
  "/opt/portage-engine/terraform/providers/registry.terraform.io/telmate/proxmox/terraform-provider-proxmox_3.0.2-rc04_linux_${PE_ARCHITECTURE}.zip:$PE_TERRAFORM_PROXMOX_PROVIDER_SHA256" \
  "/etc/portage-engine/evidence/source-image-manifest.json:$PE_SOURCE_IMAGE_MANIFEST_SHA256"; do
  path=${pair%%:*}
  expected=${pair#*:}
  actual=$(sha256sum "$path" | awk '{print $1}')
  [[ $actual == "$expected" ]] || fail "installed runtime digest drifted: ${path}"
done

secret_scan_roots=(/root /etc/portage /etc/portage-engine /usr/local/libexec/portage-engine)
if [[ -d /var/lib/portage-engine ]]; then
  secret_scan_roots+=(/var/lib/portage-engine)
fi
if find "${secret_scan_roots[@]}" -xdev -type f \
  \( -name '*.key' -o -name '*.secret' -o -name '*token*' -o \
     -name 'terraform.tfstate*' \) -print -quit | grep -q .; then
  fail "private key, token, secret, or Terraform state is present"
fi
if grep -ERIl \
  '(CLOUD_PVE_TOKEN_SECRET|PM_API_TOKEN_SECRET|PBS_PASSWORD|WORKER_GATEWAY_TLS_KEY|GPG_HOME|GPG_PRIVATE)' \
  /etc/portage-engine 2>/dev/null | grep -q .; then
  fail "forbidden management, listener, or signing credential setting is present"
fi
if find "${secret_scan_roots[@]}" -xdev -type f \
  \( -name 'secring.gpg' -o -path '*/private-keys-v1.d/*' \) \
  -print -quit | grep -q .; then
  fail "private OpenPGP key material is present"
fi

cloud-init clean --logs --seed || cloud-init clean --logs
# cloud-init clean does not remove renderer output on every distribution.
# Never seal the Packer VM's MAC-specific networkd file into the successor
# template; the boot-time refresh unit will apply the newly rendered clone
# configuration after cloud-init-network completes.
find /etc/systemd/network -maxdepth 1 -type f \
  -name '10-cloud-init-*.network' -delete
find /etc/systemd/network -maxdepth 1 -type d \
  -name '10-cloud-init-*.network.d' -exec rm -rf -- {} +
rm -f /etc/ssh/ssh_host_*
find /root /home -xdev -type d -name .ssh -prune -exec rm -rf -- {} +
rm -f /root/.bash_history
rm -rf /var/lib/cloud/instance /var/lib/cloud/instances /var/lib/cloud/sem
rm -f /var/lib/systemd/random-seed
rm -f /var/lib/dhcpcd/*.lease /var/lib/dhcp/*.leases /var/lib/NetworkManager/*lease*
rm -rf /var/log/journal/*
find /var/log -type f -exec truncate -s 0 {} +
rm -f /etc/machine-id /var/lib/dbus/machine-id
install -m 0644 /dev/null /etc/machine-id

install -d -m 0755 /etc/portage-engine/evidence
cat >/etc/portage-engine/evidence/persistent-executor-image.json <<EOF
{
  "schema_version": 1,
  "bootstrap_contract": "pve-dmi-v1",
  "executor_template_generation": "${PE_EXECUTOR_TEMPLATE_GENERATION}",
  "capacity_pool_id": "${PE_CAPACITY_POOL_ID}",
  "provider": "${PE_CAPACITY_PROVIDER}",
  "execution_zone": "${PE_EXECUTION_ZONE}",
  "architecture": "${PE_ARCHITECTURE}",
  "build_mode": "${PE_BUILD_MODE}",
  "profile_id": "${PE_PROFILE_ID}",
  "worker_image_id": "${PE_WORKER_IMAGE_ID}",
  "worker_image_generation": "${PE_WORKER_IMAGE_GENERATION}",
  "portage_server_sha256": "${PE_PORTAGE_SERVER_SHA256}",
  "portage_builder_sha256": "${PE_PORTAGE_BUILDER_SHA256}",
  "terraform_sha256": "${PE_TERRAFORM_SHA256}",
  "terraform_proxmox_provider_sha256": "${PE_TERRAFORM_PROXMOX_PROVIDER_SHA256}",
  "source_image_manifest_sha256": "${PE_SOURCE_IMAGE_MANIFEST_SHA256}",
  "credentials_embedded": false,
  "api_listener_enabled": false
}
EOF
chmod 0644 /etc/portage-engine/evidence/persistent-executor-image.json

test ! -s /etc/machine-id
test "$(stat -c '%a' /etc/machine-id)" = 644
test -z "$(find /etc/systemd/network -maxdepth 1 -type f \
  -name '10-cloud-init-*.network' -print -quit)"
test -z "$(find /etc/systemd/network -maxdepth 1 -type d \
  -name '10-cloud-init-*.network.d' -print -quit)"
test ! -e /root/.ssh/authorized_keys
test ! -e /etc/ssh/ssh_host_ed25519_key
echo "persistent executor image gate passed"
