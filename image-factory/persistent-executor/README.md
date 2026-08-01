# Persistent executor candidate template

This directory is the dedicated Gentoo persistent-executor image lane. It is
not the disposable job-builder lane in `image-factory/packer/`: the resulting
VM runs the listener-free `portage-server` executor role and carries a pinned
`portage-builder` binary only so phase execution can provision a fresh,
single-use builder VM.

The candidate is bound to exactly one provider/zone/architecture/build-mode/
profile/worker-image generation. `run.sh` derives the same capacity-pool hash
as the scheduler, while `provision.sh` recomputes it inside the guest before it
installs anything. The four phase labels are explicit. The
`capacity-instance:<uuid>` label is deliberately absent: the actuator places
the database-reserved UUID in SMBIOS and the systemd pre-start helper derives
that label on every boot.

## Image boundary

The build installs only binaries, public metadata, the systemd unit, and
non-secret immutable capability labels. It also installs the digest-locked
Terraform binary and Telmate Proxmox provider in a filesystem-only mirror with
no direct registry fallback. It intentionally leaves
`/etc/portage-engine/executor.conf` absent. Provision that runtime file and its
referenced credentials outside the image, then start the service. The image
gate rejects PVE/PBS management secrets, signing key material, Terraform
state, Worker Gateway listener keys, a pre-baked capacity UUID, SSH host keys,
and Packer's bootstrap authorization.

Before Packer runs, `run.sh` hashes the accepted source image manifest and
reads the source VMID back through authenticated PVE HTTPS. The VM must be the
expected template with `ciupgrade=0`, `ciuser=root`, `ipconfig0=ip=dhcp`, and a
`portage-engine-provenance=sha256:<manifest-digest>` description stamp. This
prevents a mutable VMID from silently replacing the reviewed Gentoo base.

The runtime executor needs the existing least-privilege PostgreSQL, artifact,
PVE, and workload-issuer authorities required by its selected phases. Supply
them through a boot-time secret mechanism such as a PVE cloud-init snippet
owned by the deployment, a read-only secret disk, or a host-integrated secret
agent. None of those values may be added to this directory or to the template.
The example at `/usr/share/portage-engine-executor-runtime.example` contains
names and paths only. An executor never receives `WORKER_GATEWAY_TLS_KEY`; it
uses the advertise URL and public CA bundles so the disposable worker it
creates can connect outbound to the control-plane Worker Gateway.

## Build

Build Linux binaries first and record their digests:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -o bin/portage-server-linux-amd64 ./cmd/server
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -o bin/portage-builder-linux-amd64 ./cmd/builder
sha256sum bin/portage-{server,builder}-linux-amd64
```

Export site values without writing a `.pkrvars` file. In particular, PVE and
SSH credentials remain runner-only inputs:

```bash
export PKR_VAR_proxmox_url=https://pve.example.internal:8006/api2/json
export PKR_VAR_proxmox_username='packer@pve!persistent-executor'
export PKR_VAR_proxmox_token='<from-secret-provider>'
export PKR_VAR_proxmox_node=pve01
export PKR_VAR_proxmox_pool=portage-engine
export PKR_VAR_proxmox_storage=ceph-vm
export PKR_VAR_source_vmid=9200
export PKR_VAR_source_template_name=pe-gentoo-amd64-base-g17
export PKR_VAR_source_image_manifest=/srv/offline/images/base-systemd.image-manifest.json
export PKR_VAR_source_image_manifest_sha256='<64 lowercase hex>'
export PKR_VAR_template_name=pe-gentoo-amd64-persistent-executor-g1
export PKR_VAR_template_description='Portage persistent executor g1; pve-dmi-v1'
export PKR_VAR_ssh_private_key_file=/run/secrets/packer-ssh
export PKR_VAR_executor_template_generation=g1
export PKR_VAR_capacity_provider=pve
export PKR_VAR_execution_zone=zone-a
export PKR_VAR_architecture=amd64
export PKR_VAR_build_mode=native-gentoo
export PKR_VAR_profile_id=pe/amd64/base-v1
export PKR_VAR_worker_image_id=pe/amd64/base
export PKR_VAR_worker_image_generation=g17
export PKR_VAR_egress_capability='egress:egress/pve-zone-a@sha256:<digest>'
export PKR_VAR_portage_server_binary="$PWD/bin/portage-server-linux-amd64"
export PKR_VAR_portage_server_sha256='<64 lowercase hex>'
export PKR_VAR_portage_builder_binary="$PWD/bin/portage-builder-linux-amd64"
export PKR_VAR_portage_builder_sha256='<64 lowercase hex>'
export PKR_VAR_terraform_binary=/srv/offline/terraform/bin/terraform
export PKR_VAR_terraform_sha256='<64 lowercase hex>'
export PKR_VAR_terraform_proxmox_provider=/srv/offline/terraform/providers/registry.terraform.io/telmate/proxmox/terraform-provider-proxmox_3.0.2-rc04_linux_amd64.zip
export PKR_VAR_terraform_proxmox_provider_sha256='<64 lowercase hex>'

image-factory/persistent-executor/run.sh
```

Use the generated template name only in a `capacity-actuator.json` allowlist
entry whose `image_id`, `image_generation`, and `bootstrap_contract` match the
pool (`pve-dmi-v1`). A Packer success is candidate evidence, not SCHED-2B Gate
completion; the live scale-up/drain/delete command is documented in
`docs/PVE_TESTING.md`.
