# PVE Native Gentoo Reference Deployment

This guide describes the tested Proxmox VE path: Terraform clones a native
Gentoo VM template, the server deploys `portage-builder` over SSH, the VM runs
`emerge` directly, and the resulting GPKG artifact is collected and install-
verified before it is exposed by the binhost.

The Docker builder backend has been removed. `native-gentoo` is the only
supported build mode.

## Current flow

```text
portage-client build
        |
        v
portage-server: validate request and select PVE
        |
        v
Terraform: clone native Gentoo template and wait for guest IP
        |
        v
transient SSH: bootstrap identity/configuration; fetch the builder from an
immutable internal URL with a mandatory SHA-256 (SCP remains a lab fallback)
        |
        v
Native Gentoo VM: emerge --buildpkg -> GPKG
        |
        v
server: collect unsigned GPKG -> private quarantine -> install verification
        |
        v
PostgreSQL signing queue -> isolated outbound-pull signer -> signed GPKG
        |
        v
builder: signed install verification with a job-local public-key trust store
        |
        v
server: immutable promotion -> atomic Packages update
        |
        v
completed job -> destroy the single-use native VM
```

This path has been exercised on real PVE infrastructure. Keep a record of the
PVE version, template image digest, Gentoo profile, kernel, Portage version,
Terraform version, generated provider version, and builder binary digest for
each reproducible baseline. The generated Terraform currently pins
`telmate/proxmox` to `3.0.2-rc04`.

## Trust boundary

This is a **trusted-network alpha** deployment:

- the server uses SSH only for the initial binary/identity bootstrap;
- the builder opens no inbound build API and actively pulls through the
  dedicated TLS 1.3 Worker Gateway;
- every short-lived worker certificate binds one worker ID, job ID, scheduler
  attempt ID and fencing token; the gateway rechecks the PostgreSQL lease;
- after the outbound worker authenticates, the target PVE VM is read back with
  `policy_in=DROP` and `policy_out=DROP`; SSH and TCP 9090 are no longer
  reachable for the task lifetime;
- builders receive only the release **public** key for a job-local verification
  keyring; neither builder nor server mounts the signing private key;
- the independent signer has no HTTP listener, pulls digest-bound tasks from
  PostgreSQL with leases/fencing, and is the only process with the private
  GPG home;
- PVE and SSH credentials are operator secrets;
- native instances are single-use and are destroyed after the pipeline.

Do not expose the HTTP server/dashboard or PVE API directly to an untrusted
network. The worker gateway may bind to the builder VLAN because it requires a
CA-verified client certificate before HTTP dispatch, and additionally validates
the certificate URI against the active attempt/fence. The release key isolation,
outbound worker identity, project runtime/cost quota, failure-storm cooldown,
administrator step-up and cross-replica OIDC session lifecycle are
implemented; production identity-provider callbacks and an external workload
issuer/recovery drill remain.
See [NEXT_STEPS.md](NEXT_STEPS.md) for what is still outstanding, and
[DESIGN_DECISIONS.md](DESIGN_DECISIONS.md) for why the boundary sits here.

In a multi-replica control plane, the normal HTTP API may sit behind a load
balancer, but each replica's `WORKER_GATEWAY_ADVERTISE_URL` must route directly
back to that issuing/executing replica (or use an equivalent identity-aware
sticky route). The in-flight gateway session is deliberately process-local;
PostgreSQL remains the authority and retries with a new attempt/VM after a
replica loss. Do not round-robin a worker's long polls across replicas. Schema
8 records a minimum executor protocol on every new job and rejects attempts
from old binaries at the database boundary, so a mixed-version replica cannot
silently drop egress or mTLS fields. Rolling upgrades must still drain old
executors before removing their deployment.

The advertised Worker Gateway and callback addresses must be stable from the
builder VLAN and must match both the gateway certificate SAN and the resolved
egress allowlist. Do not advertise a laptop's DHCP address. The 2026-07-28 lab
Gate used `10.31.0.2` as a stable NAS-side L4 ingress to the local control
plane, which also removed the PVE-to-laptop routing dependency. Its reverse SSH
tunnel and small TCP relay are lab-only scaffolding: a persistent deployment
needs a supervised listener, load balancer/VIP, or routed service address with
health checks and managed certificates.

## Prerequisites

### Control plane

- Terraform available on the server `PATH`. The Compose control-plane image
  includes the repository-pinned Terraform version.
- Network access from the server to the PVE API and transient builder SSH.
- Network access from the builder VM to `SERVER_CALLBACK_URL` plus
  `WORKER_GATEWAY_ADVERTISE_URL`; the resolved profile egress policy must cover
  both exact host/CIDR and port.
- A server certificate matching the gateway advertise host, its server CA, a
  client CA/issuer pair, and an issuer private key readable only by the server.
  Generate a lab PKI with
  `WORKER_GATEWAY_HOST=<LAN-host> scripts/generate-worker-pki.sh .local/worker-pki`;
  production should use an
  operator-managed issuer and rotation procedure.
- A Linux builder binary matching the template architecture.
- PostgreSQL schema 31 with the repeatable signer-role bootstrap, executor
  protocol fence, OIDC subjects/project RBAC, durable phase execution context,
  Worker Gateway spool, attempt runtime/cost ledger, IAM session lifecycle and
  exact executor capability routing applied.
- In separated active-phase deployments, shared S3-compatible artifact storage;
  process-local `DATA_DIR`/quarantine is disposable scratch. Terraform workspace
  paths remain durable and shared with the capacity actuator.
- A separate signer-only private GPG volume. The server and builders must not
  mount it.
- For local bootstrap, `GPG_AUTO_CREATE=true` creates one key and persists an
  active-key marker inside the same signer-only volume; restarts reuse that
  exact key. For production, provision a release subkey, set `GPG_KEY_ID`
  explicitly, and set `GPG_AUTO_CREATE=false`. Back up and restore the keyring
  and active-key marker as one unit.

Build the amd64 binary with:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -o bin/portage-builder-linux-amd64 ./cmd/builder

sha256sum bin/portage-builder-linux-amd64
```

Use the corresponding `GOARCH` and filename for another architecture.

When delivering a local binary, mount the binary's **parent directory** or
recreate the server container after rebuilding it. A single-file Docker bind
mount can remain attached to the old inode when `go build` atomically replaces
the host file. For repeatable PVE tests, the preferred path is an immutable
NAS/object URL plus mandatory SHA-256.

For a PVE cluster on the same LAN as an internal artifact server, prefer
guest-side delivery over SCP from the control plane:

```ini
CLOUD_BUILDER_BINARY_PATH=
CLOUD_BUILDER_BINARY_URL=http://10.31.0.2/local/portage-engine/tools/<sha256>/portage-builder-linux-amd64
CLOUD_BUILDER_BINARY_SHA256=<64 lowercase hexadecimal characters>
```

The path setting wins when both path and URL are present. URL delivery is
accepted only for an absolute HTTP(S) URL without embedded credentials, query
parameters, or fragments, and the SHA-256 digest is mandatory. Native Gentoo
bootstrap downloads to a temporary file, verifies the digest, and only then
installs the executable. Using plain HTTP is therefore suitable only
on a trusted internal network and still requires the pinned digest.

Store other reproducible build inputs in the same artifact service under
content-addressed paths: Packer and Terraform binaries/providers, stage3 and
QCOW2 images, profile-repository Git bundles, distfiles, and offline mirror
bundles. Record every URL and digest in the image-factory input lock; do not use
mutable `latest` paths for promoted images or tooling.

Record deployable builder/server/agent executables with input-lock kind
`service-binary`. The lock validates their exact size, SHA-256 and executable
bit before bundle signing, so a NAS object is transport only and never becomes
an implicit trust root.

### Native Gentoo VM template

`CLOUD_PVE_TEMPLATE` must name a QEMU VM template, not an LXC template. The
tested native deployment assumes:

- Gentoo with a selected profile and a synchronized Portage repository;
- systemd, because the deployment installs `portage-builder.service`;
- `cloud-init` and `qemu-guest-agent`, both enabled at boot;
- root SSH through the cloud-init-injected public key;
- `bash`, `emerge`, GnuPG, `getuto`, CA certificates, and working DNS;
- `/etc/portage/make.conf` and `/var/db/repos` already present;
- UEFI/OVMF with a Q35 machine and a SCSI system disk;
- enough disk, memory, and CPU for the packages being built.

The default machine request is 4 cores, 8 GiB RAM, and a 50 GiB disk. Large
packages should override those values. Do not bake API tokens, PVE credentials,
the shared builder token, or signing private keys into the template.

Before converting the VM to a template, verify inside the guest:

```bash
systemctl is-enabled cloud-init qemu-guest-agent
systemctl is-active qemu-guest-agent
emerge --info
gpg --version
getuto --help >/dev/null
```

### Catalog-selected profile/template matrix and image factory

The server-owned catalog now resolves `profile_id` to an approved profile
repository revision, parent chain, image generation, PVE template, mirror
bundle, and resource class. Clients cannot choose a raw template or arbitrary
profile path. `CLOUD_PVE_TEMPLATE` remains only the explicit compatibility
fallback when `CATALOG_PATH` is empty; requests using that fallback must match
the profile already baked into the single configured template and are recorded
with the `compatibility` channel in resolved job provenance.

Use the following division of responsibility for additional profiles:

| Layer | Responsibility |
| --- | --- |
| Packer | Bake, sanitize, test, and publish immutable PVE template generations |
| Terraform | Clone an already-published generation and manage the job VM lifecycle |
| Catalyst | Produce traceable Gentoo stages/root filesystems from pinned seeds, repository snapshots, profiles/specs, and internal mirrors |

Start Packer from a verified Gentoo cloud-init QCOW2 image or the existing
minimal native Gentoo PVE template. For the QCOW2 path, import it once as a
minimal PVE seed template; subsequent `proxmox-clone` builds clone that seed,
pin the Gentoo repository snapshot and profile-repository commit, select
`/etc/portage/make.profile`, reconcile `@world`, install cloud-init,
qemu-guest-agent and builder prerequisites, remove machine-specific state, run
smoke tests, and convert the result to a versioned template. Terraform must not
repeat those mutations for every package build.

#### Catalyst rootfs lane

Catalyst is a second input lane to the same image pipeline, not a replacement
for Packer or Terraform. The IMG-2 control path is now implemented for one
amd64/systemd stage4/rootfs: strict CatalystPlan/input lock, signed stage3 and
Git-commit verification, generated config/spec/fsscript/root overlay, shared
versioned package sets, a network-denied Catalyst runner, GPT/UEFI QCOW2
assembly, PVE seed import and QCOW2-manifest provenance. The final Packer and
Terraform gates are the existing IMG-1 gates, selected with
`IMAGE_FACTORY_PLAN=image-factory/plans/base-systemd.catalyst.build.json`.

The QCOW2 uses an unsigned removable-path Gentoo GRUB. The seed importer sets
`pre-enrolled-keys=0`; enabling Secure Boot requires a later signed
shim/GRUB/kernel trust chain and is not part of the IMG-2 acceptance gate.

A minimal offline-oriented `catalyst.conf` uses Catalyst's TOML syntax (paths
are examples):

```toml
digests = ["sha256", "sha512"]
compression_mode = "xz"
envscript = "/etc/catalyst/offline.env"
options = ["bindist", "sticky-config", "versioned_cache"]
jobs = 4
storedir = "/var/lib/catalyst"
distdir = "/srv/mirror/gentoo/distfiles"
target_distdir = "/var/cache/distfiles"
repos_storedir = "/srv/mirror/git"
repo_basedir = "/var/db/repos"
repo_name = "gentoo"
repo_openpgp_key_path = "/etc/portage/gnupg/gentoo-release.asc"
```

The generated spec pins `target`, `subarch`, `profile`, `snapshot_treeish`,
`source_subpath`, `portage_confdir`, the fsscript, root overlay and the packages
expanded from `pe/catalyst-boot-v1`. The snapshot ID is a stable Catalyst
artifact identifier and a local ref at the exact locked commit; the separate
full commit remains the trust anchor in the rootfs manifest. The special
`stable` treeish is rejected because Catalyst uses it for a remote repository
update. `seedcache` and `pkgcache` are disabled in the PoC so a
stale cache cannot conceal a missing input. Add either only after a cold-cache
and clean-stage equivalence test.

The sync-side distfile resolver must use the same signed stage3 archive and
target profile as the Catalyst build. It resolves the deep/`--newuse` stage3
`@world` transition plus the selected image package set; the generated
Catalyst fsscript performs that same reconciliation before sealing stage4.
This prevents inherited Python target or USE state from becoming an expensive
and previously invisible successor-Packer rebuild. Resolving against the sync worker's own
VDB is invalid because packages installed only on that worker can disappear
from the closure; resolving against an entirely empty VDB is also invalid
because it introduces bootstrap cycles already satisfied by stage3. The
closure evidence therefore records the stage3 digest, profile selector,
isolated DISTDIR and normalized resolver configuration. Run Portage inside the
stage3 chroot in a private mount namespace; setting only `ROOT` is insufficient
because Portage can still resolve and fetch build-root dependencies through the
sync worker. On the build side, the digest-checked closure is bind-mounted at
`target_distdir`; its mount root must be `0755` and its objects `0644` so the
Portage fetch user can traverse it. Keep the enclosing Catalyst work directory
`0700`. The locked hydrator path must be resolved relative to the runner's
`factory/packer/scripts` sibling so the checkout and offline-bundle layouts
exercise the same file.

For the initial desktop transition, the stage3 VDB is no longer the correct baseline.
Run `sync/generate-image-closure.sh` inside a disposable clone of the exact
accepted base template. The resolver retains that clone's installed VDB,
selects the desktop profile through a job-scoped `PORTAGE_CONFIGROOT`, uses a
clean DISTDIR, and records the base image-manifest hash plus canonical VDB
digest. The command also receives the expected signed profile-repository
commit and records its Git tree; commit mismatch or tracked worktree drift is
fatal. Do not generate the desktop closure on the long-lived sync worker or
before the base Packer + Terraform smoke/destroy/output-stamp gate passes.
The desktop BuildPlan records the accepted base revisions in
`source_repositories` and the signed revisions being installed in
`repositories`. These maps may differ, but the source map must match the base
image manifest exactly; this is the reviewed upgrade edge rather than a reason
to relabel or rebuild the accepted base.

The image smoke gate also binds SSH identity before its first connection.
`guest-host-key` reads `/etc/ssh/ssh_host_ed25519_key.pub` through authenticated
PVE HTTPS + QGA, validates the ED25519 wire form, and creates an ephemeral
`known_hosts`; SSH then runs with `StrictHostKeyChecking=yes`. A live read-only
check against the Catalyst successor VM confirmed the QGA key and local
fingerprint validation path. `accept-new` is not acceptable for new release
bundles.

When a gate-only fix lands after an otherwise accepted base build, create a
new immutable generation with `rootfs_source: "packer-base-image"`; do not
edit the signed old bundle or attach an unbound supplemental script. The new
plan locks the prior full image manifest and accepts it only when template,
ABI, repositories, exact base profile and parent chain all match. Packer can
then re-seal the already reconciled base quickly and the new signed bundle's
smoke evidence becomes the formal release input.

Image builds also reserve host capacity, not only guest capacity. Set
`proxmox_host_memory_headroom_mb` (4096 MiB in the reference site config);
source-check requires the selected PVE node to have the requested guest memory
plus that free headroom immediately before Packer clone. Ephemeral Packer VMs
carry the `image-factory-build` tag. The controller should allow only one image
build lease per small node (or schedule the next build onto a node with an
independent reservation), since two concurrent checks can otherwise race.
If the PVE host OOM killer terminates QEMU, mark the generation rejected and
start a fresh immutable build after capacity is reserved; restarting the
partial candidate is not release evidence.

Install `image-factory/bootstrap-offline.sh` outside each incoming bundle and
start Packer through it. The launcher requires the signed bundle manifest,
detached signature, independently provisioned sync public key and an
out-of-band-approved factory binary SHA-256. It verifies that binary first and
then verifies the bundle signature/freshness, lock digest and every object
before trusting the lock or executing `run-offline.sh`. A lock that merely
contains the digest of its own verifier is not a trust bootstrap.

The spec also sets `keep_repos: gentoo`, and the config uses `sticky-config`.
Together these preserve the internal runtime repository/mirror configuration
after Catalyst cleanup. Do not use `stage4/use` merely to select the GRUB
platform: it creates a stage-specific USE layer that can diverge from the
selected profile's runtime defaults and force a large Packer `--newuse`
reconciliation. Both the Catalyst build config and resulting image instead set
`GRUB_PLATFORMS="efi-64"`; the assembler independently verifies that the
installed GRUB contains the EFI target.

The executable runbook and placeholder input contracts are in
`image-factory/catalyst/`; see `image-factory/README.md` for the exact rootfs,
QCOW2, PVE import and handoff commands. The example lock is intentionally not
runnable until all zero digests, sizes, endpoints and the fake commit are
replaced. The documented PVE reference run completed the public-egress-denied
external-profile stage4, rootfs manifest, QCOW2 check and immutable seed boot.
IMG-2 remains candidate-only until successor Packer emits its image manifest
and the locked Terraform smoke/destroy/output-stamp gate succeeds.

The locked Catalyst runtime prefix must contain `bin/catalyst`,
`share/catalyst`, and exactly one Python `site-packages` or `dist-packages`
tree containing Catalyst and its dependencies. The runner discards the worker's
`PYTHONPATH` and verifies the imported module remains inside that locked tree.

Catalyst 4.1.1 source and generated-contract tests confirm the external-profile
path: `profile` accepts `repository:profile`, while `repos` imports the locked
profile repository. The IMG-3 runner verifies that repository's signed commit,
`profiles/repo_name`, selected directory and exact `parent` lines before the
network-denied build. Packer independently repeats the commit/profile checks
before switching `make.profile` and reconciling `@world`; neither lane modifies
the verified Gentoo snapshot to inject private profiles. The documented live
run remains an acceptance exercise; only its locked manifests and completed
smoke/destroy/output-stamp evidence can establish acceptance.

“Reproducible” initially means that the recorded seed, spec, profile,
repository snapshots, distfiles, Catalyst/Packer versions, and scripts can
rebuild a functionally equivalent image. It does not yet promise bit-for-bit
identical tarballs, packages, or QCOW2 images.

#### Offline mirror plane

Support two operating modes with the same content manifest:

- **mirror-backed offline:** builders have no public egress but can reach
  internal Git, distfiles, stage/QCOW2, binpkg, tool, module, and OCI mirrors;
- **strict air gap:** the build network cannot reach the synchronization
  gateway; operators import a signed, approved content bundle on controlled
  media.

The internal mirror inventory should include:

```text
/srv/mirror/git/gentoo.git
/srv/mirror/git/portage-engine-profiles.git
/srv/mirror/gentoo/snapshots/
/srv/mirror/gentoo/distfiles/
/srv/mirror/gentoo/seeds/
/srv/mirror/gentoo/binpkgs/
/srv/mirror/tools/packer/plugins/
/srv/mirror/tools/terraform/providers/
/srv/mirror/go-proxy/
/srv/mirror/oci/
```

Before exporting a bundle, a connected synchronization job resolves the exact
profile/USE/keyword dependency graph and fetches its complete distfile closure,
stage/QCOW2 seeds, repository commits, Packer plugins, Terraform providers,
Go modules, and required OCI images. It verifies Git/OpenPGP signatures,
ebuild Manifest hashes, upstream SHA-256 files, `.terraform.lock.hcl`, and
`go.sum`, then signs a content-addressed `MirrorBundle` manifest.

`RESTRICT=fetch` and `RESTRICT=mirror` packages need an explicit missing-object
and license review path; a generic mirror cannot guarantee those inputs. The
offline build must fail with a machine-readable missing-object report, not try
the original `SRC_URI`. Enforce this with network egress deny in addition to
Portage mirror settings. A configuration flag alone is not a security boundary.

Terraform must use the locked `filesystem_mirror` template without a `direct`
fallback. The template contains one provider-directory placeholder; the smoke
runner resolves it under the exact offline root and writes a job-local
owner-only CLI config. A hard-coded site path is rejected so the signed bundle
remains relocatable. Packer plugins must be preinstalled from approved
local binaries with their required SHA256SUM files. Portage uses only the
internal repository/distfile/binpkg endpoints. Project builds use an internal
GOPROXY followed by `off`, and OCI references are pinned to digests in the
internal registry.

The synchronization manifest needs a freshness timestamp and security-advisory
watermark. When the configured freshness SLO expires, development builds may
continue by policy, but stable image/binpkg promotion should stop. The sync
gateway must not hold the release signing key and must not be reachable from a
strict air-gapped build zone.

Split templates only on hard environment boundaries: architecture, libc,
multilib ABI, init/base profile family, and any security policy that changes
the base runtime. Keep package USE flags, keywords, compiler flags, and normal
dependency choices in the immutable BuildSpec. Desktop environments normally
belong to separate GUI verifier images rather than multiplying every binpkg
builder template.

Custom Portage profiles should live in a signed, protected, versioned profile
repository that inherits the chosen Gentoo parent profile. Keep this repository
separate from the public CLI installation overlay, or at minimum give it
separate ownership and review rules. Never follow its moving branch when a
template or job is created.

The target control plane needs a server-owned Profile Registry with at least:

```yaml
profile_id: pe/amd64/no-multilib/systemd/base-v1
arch: amd64
libc: glibc
init: systemd
abi: no-multilib
profile_repo_digest: sha256:...
profile_path: pe-profiles:portage-engine/amd64/23.0/no-multilib/systemd/base
gentoo_repo_snapshot: gentoo@<commit-or-snapshot>
rootfs_source: catalyst-stage4-qcow2  # or approved-pbs-snapshot / packer-base-image
rootfs_manifest_digest: sha256:...
mirror_bundle_digest: sha256:...
image_generation: g17
pve_template: pe-gentoo-amd64-base-g17
image_manifest_digest: sha256:...
channel: candidate  # candidate | stable | retired
```

Build requests should contain only an approved stable `profile_id`. The server
resolves it to the exact profile digest and template generation, and the
scheduler rejects architecture, ABI, worker capability, or image-generation
mismatches. Record the resolved values in job provenance; do not accept an
arbitrary user-supplied profile path or template name.

Before promoting a template generation, verify that:

1. a clean-cache build with public egress denied completes without attempting
   an external DNS, Git, HTTP, module, plugin, provider, or OCI fetch;
2. cloud-init is idempotent and qemu-guest-agent reports the guest IP;
3. `/etc/portage/make.profile`, its parent chain, `emerge --info`, and the
   effective repository revisions match the image manifest;
4. representative dependency graphs resolve and a small signed package can be
   built and installed in a clean verifier;
5. SSH host keys, prior `machine-id` identity, logs, caches, API tokens,
   builder tokens, and signing private keys are absent; the required empty
   `/etc/machine-id` placeholder remains mode `0644`;
6. the manifest records MirrorBundle, Catalyst seed/spec/config/output,
   source image/stage, Packer template, profile repository and Gentoo snapshot
   digests, tool versions, output PVE template ID/name, and an SBOM/checksum.

An empty `/etc/machine-id` is not sufficient by itself: it must also be mode
`0644`. Catalyst can preserve a build-only `0600` mode, which prevents the
unprivileged `systemd-networkd` service from reading the regenerated ID to
derive its DHCP DUID. The Catalyst fsscript, QCOW2 assembler, Packer sanitizer,
and live boot gate therefore all enforce an empty, world-readable file. A
successful gate must show DHCP acquisition without any manual guest mutation.
All current Packer successors refresh cloud-init's generated networkd file
after the network stage and use the clone NIC's MAC address as the DHCP client
identity. The PVE provisioner also performs the same fixed, idempotent reload
through QGA while an older approved template reports no IPv4 address. This
prevents a re-templated VM's stale renderer output or DUID/lease from being
reused by a new NIC; the live Gate must still reject any assigned address that
already answers duplicate-address detection on the PVE bridge.

Catalyst may likewise preserve its build user's ownership on an external
profile repository. Packer normalizes each locked repository to `root:root`
before Git operations. It must not add a global `safe.directory` exception,
because that would disable Git's ownership boundary instead of producing a
correct image.

For Gentoo guests, explicitly set `ciupgrade=0` on source templates and
`ciupgrade = false` on Terraform clones. Leaving it implicit causes PVE
cloud-init metadata to request a package upgrade, which maps to an uncontrolled
`emerge --sync` during first boot. A valid gate proves the sync command is
absent from both first- and second-run cloud-init logs.
Packer source templates must also set `ciuser=root` and
`ipconfig0=ip=dhcp`. An omitted `ipconfig0` can produce a valid NoCloud disk
whose network document contains only resolver data; cloud-init then completes
without managing the NIC and SSH never becomes reachable.
The output stamp writes and reads back both fields together with
`ciupgrade=0`; Packer template conversion is not allowed to leave the
successor/source contract implicit merely because Terraform sets the same
values on a later clone.

When PBS is the seed recovery store, acceptance requires more than a successful
backup task: pin the PBS certificate fingerprint, use separate write and
read-only tokens, run server-side verification, restore to a disposable
template, boot a fresh clone, and retain a signed attestation over the datastore
and snapshot identity plus decoded `index.json`, `qemu-server.conf`, and image
index checksums. Delete the restore/clone VMIDs after the gate; keep the source
template and retention-managed PBS snapshots.

Before sealing the input bundle, set the accepted PBS snapshot protection flag
to `true` and read it back. Verification alone does not prevent prune from
removing a reproducibility input. `portage-image-factory pbs-attest` rejects an
unprotected snapshot and cross-checks the decoded manifest, QEMU contract,
first/second cloud-init gates, Gentoo runtime evidence, and cleanup record. Add
its output to the lock as `pbs-source-attestation`; do not edit the generated
JSON to simulate protection.

Publish immutable generations such as
`pe-gentoo-amd64-glibc-systemd-base-g17`; move a `stable` registry alias after
tests instead of modifying a template in place. Packer does not manage old PVE
templates, so a separate image controller/CI retention job must retire a
generation only after no active VM lease refers to it.

The image matrix and the two build paths are in [CATALOG.md](CATALOG.md) and
the [image factory](../image-factory/README.md); the rule about what belongs in
a template rather than a BuildSpec is in
[DESIGN_DECISIONS.md](DESIGN_DECISIONS.md).

### PVE identity

Use a dedicated Terraform user/token and bind only the VM clone/configuration,
power, audit, and datastore privileges required by the chosen pool, storage,
template, and nodes. A full-privilege `root@pam` token is acceptable only for a
disposable lab and must not be the documented production default.

Supply the token secret through `CLOUD_PVE_TOKEN_SECRET` from the deployment
secret provider. In PostgreSQL authority mode the dashboard persists only
non-secret settings and an environment reference; attempts to submit a secret
value through the shared settings API are rejected. The generated Terraform
receives the resolved value through `TF_VAR_pve_token_secret`; it is not
embedded in `main.tf`. Protect Terraform state as a secret and include it in
neither logs nor bug reports. The instance
inventory in `DATA_DIR/instances.json` stores only non-secret lifecycle and
Terraform workspace metadata. Destroy credentials stay in memory; after a
server restart they are resolved again from the current protected cloud
settings. Older inventory files containing `destroy_env` are scrubbed and
rewritten when loaded.

## Configure the control plane

`configs/server.conf` contains bootstrap settings only. For signing-enabled
operation, start PostgreSQL/migrations, the isolated signer, and the server as
one deployment. The checked-in Compose stack applies schema 20 and creates a
least-privilege `portage_signer` role:

```bash
cp .env.compose.example .env.compose
# Replace all local-default credentials before exposing any port.
docker compose --env-file .env.compose up -d
docker compose --env-file .env.compose ps
curl --fail http://127.0.0.1:18080/api/v1/gpg/status
```

Start from `configs/catalog.example.json`, replace all example commits, digests,
mirror URLs and the PVE template with reviewed values, and validate in a
candidate channel before promotion. With `CATALOG_PATH` unset the tested legacy
template continues through compatibility mode. See [CATALOG.md](CATALOG.md).

For a non-Compose deployment, run `portage-signer` as a separate service with
`DATABASE_ENABLED=true`, `DATABASE_REQUIRED=true`, `GPG_ENABLED=true`, a
signer-only `GPG_HOME`, shared quarantine/binpkg storage, and the signer
database role. Run `portage-server` with the same public-key path and shared
storage but without the signer GPG home. The signer has no inbound listener.
Store generated database/API secrets, the worker CA issuer key, and the signing
key in a secret manager or protected volume before restarting services.
`BUILDER_TOKEN` is only for legacy push-mode static builders; changing it
requires rolling those legacy builders. Reference PVE pull workers instead use
an attempt-bound short-lived certificate and must leave the shared token empty.

Operational non-secret PVE settings are managed in the dashboard and
versioned/audited in PostgreSQL. Inject credentials before starting the server:

```bash
export CLOUD_PVE_TOKEN_SECRET='from-secret-provider'
# Optional password-auth fallback:
# export CLOUD_PVE_PASSWORD='from-secret-provider'
```

Database-disabled standalone compatibility mode still uses a mode-0600
`DATA_DIR/cloud-settings.json`; do not use that mode for a multi-replica
deployment. In **Settings**, configure:

| Setting | Required value or constraint |
| --- | --- |
| Provider | `pve` |
| Build mode | `native-gentoo` |
| PVE endpoint | `https://pve.example:8006` |
| Node | explicit node, or `auto` for scheduler placement |
| Template | the native Gentoo QEMU template name |
| Storage / bridge | values valid on the selected PVE node |
| SSH user | `root` for the current deployment script |
| SSH key path | private key on the server; the matching `<path>.pub` must also exist and be readable |
| Host-key policy | known-hosts file preferred; insecure mode only on an isolated lab LAN |
| Server callback URL | server URL reachable from the VM; never `localhost` |
| Builder delivery | local Linux binary path, or an internal HTTP(S) URL plus mandatory SHA-256 |
| Instance TTL | orphan/destroy retry safety deadline; avoid `0` unless manual cleanup is intentional |
| Install verification | enabled |

Use the dashboard's **Test PVE connection** action before submitting a build.
If the dashboard is not running, the same settings API can be used, but the
dashboard is the supported operator workflow because it redacts stored secrets
and retains omitted secret values on update.

## Run the smoke test

Use a small package first:

```bash
export PORTAGE_ENGINE_API_KEY="<the server API key>"

./bin/portage-client build \
  -server=http://127.0.0.1:8080 \
  -package=app-misc/jq \
  -wait
```

Expected evidence:

1. The request passes ConfigBundle validation and enters provisioning.
2. Terraform clones the configured template and reports a guest-agent IP.
3. SSH deployment copies the short-lived identity and starts
   `portage-builder.service` in pull mode; no process binds TCP 9090.
4. The worker authenticates outbound, the gateway verifies
   worker/job/attempt/fence, and PVE closes the transient SSH window with
   per-VM `policy_in=DROP` readback. Every VM-level inbound rule is removed
   and a second rules readback must contain no `type=in` entry; Cluster, Node
   and every other VM firewall object are untouched.
5. Native `emerge --buildpkg` writes into a per-job PKGDIR and produces the
   requested package plus every dependency built by that task.
6. The worker streams the category-preserving unsigned task closure through
   one-shot, size-limited, SHA-256-bound upload slots into a
   job-private quarantine. Public `BINPKG_PATH` is unchanged.
7. Unsigned native install verification creates a fresh `PKGDIR`, downloads
   only the expected `Packages` file and exact task artifacts from the private
   capability binhost, and rejects redirects, unexpected paths, sizes, digests,
   or files. It then copies the image baseline VDB into a throwaway root,
   removes every package produced by the task, and installs with
   `--usepkgonly=y --getbinpkg=n`. No host package cache or other configured
   binhost can participate.
8. Before signing, the server submits the exact unsigned generation under the
   signer-key policy. The verifier must reject it; acceptance is a hard
   pipeline failure. This is the release Gate's required negative control.
9. The server enqueues exact input paths, sizes, and SHA-256 digests. The
   isolated signer claims the task once, rejects symlinks/path changes, creates
   embedded GPKG signatures and a clear-signed Manifest, self-verifies the
   result, and returns an output manifest.
10. A second fresh `PKGDIR` downloads only the signer's output manifest. Before
   Portage runs, the builder verifies every expected GPKG signature with a
   job-local keyring containing exactly one approved primary fingerprint. The
   throwaway-root install then enables `binpkg-request-signature`, keeps
   `--getbinpkg=n`, and rechecks every artifact digest after emerge. It must
   install the complete task closure.
11. The server immutably promotes the full signed closure, atomically
   regenerates `BINPKG_PATH/Packages`, and records `SIGNED: 1`. An existing
   destination is a hard failure; choose a package/version not already present
   when running a release-gating smoke test.
12. The job reaches `completed` with an artifact reference.
13. The native VM is destroyed after successful publication. A failed destroy
   remains tracked as `destroy_failed` for retry; it is never returned to the
   warm pool.

For a release-gating regression, also record whether the artifact is signed,
its SHA-256 digest, the builder binary digest, job logs, template identity, and
both install-verification results. Confirm that:

```bash
curl --fail http://127.0.0.1:18080/api/v1/gpg/status
tar -tf /var/cache/binpkgs/<category>/<package>/<artifact>.gpkg.tar
grep -A40 '^CPV: <category>/<package>-' /var/cache/binpkgs/Packages
```

The status must report `private_key_here: false`; the tar must contain
`Manifest` plus payload `.sig` members; independently verify the clear-signed
Manifest and every detached payload signature with the public key. The
published SHA-256 must equal the signed-generation digest recorded by the
second install, and must differ from the unsigned-generation digest. Job logs
must contain `[verify-negative] unsigned generation rejected`, must show
`generation=signed signature_required=true`, and must not contain requests to
an unrelated binhost. The index stanza must contain `SIGNED: 1`. Also verify
the signing task is `completed`, quarantine is empty, and the matching
`infra_instances` row has `state=destroyed` and `deleted_at` set.

## SEC-0B/B packet-layer builder egress Gate

The catalog allowlist is not sufficient by itself. The reference PVE Gate must
prove that the hypervisor, rather than the guest, enforces the boundary.

Prerequisites:

1. Enable the PVE Datacenter firewall. Portage Engine deliberately refuses to
   change this cluster-wide setting and fails if it reads as disabled.
2. Give the provisioning identity only the PVE VM/firewall permissions needed
   to read cluster firewall options, manage the selected VM's firewall
   options/rules, start the VM and perform its existing clone/destroy flow.
3. Put every runtime endpoint in the selected catalog policy: control-plane
   callback, binhost/upload mirror, builder binary URL, repository URLs,
   Gentoo mirror/Portage sync URI and DNS resolver. Each hostname needs the
   immutable internal CIDR used by the PVE rule.

Run a representative cloud build with an enforced stable policy. The job log
must show:

```text
[policy] applying PVE default-deny egress policy ... before VM boot
[policy] verified policy_out=DROP with ... allow rules; VM started
```

During the build, inspect the VM in PVE and record:

- the virtual NIC has `firewall=1`;
- VM Firewall Options show `Enable=Yes`, Input Policy `ACCEPT`, and Output
  Policy `DROP`; the explicit input policy preserves controller SSH/bootstrap
  while the Gate remains scoped to outbound traffic;
- every enabled outbound rule is an exact Portage Engine rule to an approved
  CIDR/protocol/port; there is no `0.0.0.0/0` or `::/0` allow;
- the Terraform workspace contains owner-only `egress-policy.json` and
  `egress-policy-evidence.json`, and runtime metadata contains the same policy
  ID/digest plus `pe_egress_enforced=true`;
- an approved internal fetch/build succeeds while a request to a public test
  endpoint fails at the network layer.

Then run three negative cases: remove the callback host from the policy
(the provision request must fail before Terraform), disable the PVE cluster
firewall (the stopped VM must be rolled back without booting), and alter
readback rules in a test double or isolated VM (verification must fail closed).
A successful build without these negative cases is not completion evidence for
SEC-0B/B.

### Reference Gate result — 2026-07-27

The reference cluster passed the VM-scoped Gate with the Datacenter firewall
engine enabled and no Cluster or Node rules:

- the preflight found 72 QEMU guests, zero existing guests with VM firewall
  enabled, zero Cluster rules and zero rules on each of the six nodes;
- enabling only the Datacenter engine left all 16 running QEMU guests and the
  representative LAN/PBS/SSH endpoints reachable;
- the disposable builder was created stopped, then received
  `policy_in=ACCEPT`, `policy_out=DROP`, DHCP/NDP helpers and four exact rules:
  NAS HTTP, control-plane/binhost HTTP, and internal DNS over TCP/UDP;
- the first live attempt exposed an important default mismatch: enabling the
  VM firewall without an explicit input policy blocked SSH bootstrap. The
  adapter now sets and reads back `policy_in=ACCEPT`; evidence schema v2 and a
  regression test reject an implicit/missing input policy;
- from the final builder VM, both approved internal endpoints returned HTTP
  200 while direct public IPv4 requests on ports 80 and 443 timed out;
- `app-misc/editor-wrapper-4-r1` completed unsigned install verification,
  rejected the unsigned generation under the signer key, completed signed
  install verification, published an artifact whose index has `SIGNED: 1`,
  and destroyed the single-use VM;
- after cleanup the cluster again had 72 QEMU guests, zero guests with VM
  firewall enabled, zero Cluster/Node rules, and the same representative
  connectivity baseline.

The Datacenter engine remains enabled as the enforcement prerequisite.
Portage Engine owns only the selected builder VM's options and rules; it does
not add Cluster/Node rules or modify another VM ID.

## SEC-1 outbound worker Gate result — 2026-07-27

Job `e3a308a1-28ad-4cc0-be49-618018c2c926` completed the stricter outbound
worker and signed-publication Gate on disposable VM 142:

- the per-attempt client certificate bound worker, job, attempt and fence 1;
- after the worker connected, the control plane set both VM policies to
  `DROP`, removed every VM-level inbound allow rule, read the rules back as
  empty, and external probes confirmed TCP 22 and 9090 were unreachable;
- `sys-process/htop-3.5.1` produced a 225,280-byte unsigned GPKG with SHA-256
  `2e32ff3a7f11764f9a7c1aa06bf92f50ad4620de6b37fd133cadfebeb985e59b`;
- unsigned install passed and the mandatory signed-policy negative control
  rejected that generation;
- the isolated signer produced a 226,816-byte generation with SHA-256
  `b23f513231f14a39bc4de6f93d6b38c4ceefcafc439b84f6edaecaa2e38d4099`
  and fingerprint `31069C2591541344976994527B0D1E08E82197B0`;
- a new throwaway root installed that exact generation with
  `signature_required=true`, the official-style `Packages` index was updated,
  and Terraform destroyed the single-use VM.

The exercise also caught an old secondary control-plane binary claiming a new
job during a rolling upgrade. Schema 8 now stores a minimum executor protocol
on each new job, records every scheduler worker's protocol, and rejects a
legacy executor in the database trigger. Stable control-plane worker IDs carry
the `sec1-v1` generation so an older binary cannot reuse a current identity.
Drain old replicas during deployment even with this fail-closed fence.

## IAM-1B2b2c active phase restart Gate — 2026-07-27

Schema 16 and executor protocol 3 were exercised with two active control-plane
replicas and one disposable PVE VM at a time. Job
`78372ded-3c34-464d-9e04-3d7779212017` paused the primary after the stable
`sys-process/btop` build command was delivered. The 45-second build-phase
lease expired, the secondary reclaimed the same work item at claim fence 2,
and the VM continued its original emerge. PostgreSQL retained one build
command ID at delivery fence 1, four deterministic collect commands, one
signing task and one publish phase. Four signed GPKGs passed fresh-root
installation and the VM was destroyed.

Job `a0be1bcb-b431-455f-bee2-b3663d311337` repeated the failure during
verification of `app-misc/sl`. The secondary reclaimed verify at fence 2,
reused the same three stable verification command IDs, ran exactly one signer
claim, installed the 30,208-byte signed generation with fingerprint
`31069C2591541344976994527B0D1E08E82197B0`, and wrote exactly one CPV and PATH
entry to the official-style `Packages` index. PVE API read-back found no live
VM 142 after cleanup.

The exercise found and fixed four deployment/execution gaps:

- an empty Worker Gateway poll now commits its connection heartbeat instead
  of rolling it back with the expected no-work result;
- exact completion replay is accepted after a database commit followed by a
  lost HTTP response, while a different result/fence remains rejected;
- a long-running result is phase-fenced again before logs, collection,
  signing or publication, so the stale executor cannot create visible
  duplicate side effects;
- schema v20 now permits intentionally heterogeneous replicas only through
  exact phase/provider/zone/arch/profile/image labels. A replica still needs
  all material implied by the labels it advertises; do not claim a capability
  merely because another replica has the credential, binary, or shared state.

## IAM-1B3 runtime/cost Gate — 2026-07-28

Schema 17 and executor protocol 4 passed the PostgreSQL concurrency Gate, the
two-replica Compose Gate, and a complete real-PVE build. Job
`226ae3f5-2642-402c-9683-5fdb01f36ead` cloned disposable VM 142 on
`infra-node4` and built `app-misc/figlet-2.2.5-r2`.

- admission reserved 3,600 build seconds and 120,000 cloud-cost microunits at
  the catalog rate of 2,000 microunits/minute;
- the VM booted with `policy_out=DROP` and four exact allow rules; after its
  outbound mTLS session connected, `policy_in=DROP` was read back;
- the worker fetched the builder once from the NAS content-addressed object
  whose SHA-256 was
  `38f13734b44071a4d90beab8a3a896bf711d9a4b6a2ac603a9ed241ea73ff8fd`;
  the guest verified that digest before execution;
- unsigned install verification passed, then the mandatory signature policy
  rejected that unsigned generation;
- the isolated signer produced a 179,200-byte generation with SHA-256
  `d3fe22f5d64f5225ba95a016584a1938d52cb73ca06c31f67095216366c93f9f`
  and fingerprint `31069C2591541344976994527B0D1E08E82197B0`;
- a new throwaway root installed that exact signed generation with
  `signature_required=true`; the official-style `Packages` index contained
  exactly one matching CPV and one matching PATH, and the published HTTP
  object had the same SHA-256;
- terminal settlement charged 630 seconds and 22,000 microunits, released the
  unused reservation, and left runtime, resource, worker-session, and phase
  lease usage at zero;
- the infra ledger recorded cleanup and direct PVE API read-back confirmed
  that VM 142 no longer existed.

This Gate also verified that settlement is capped by the amount admitted even
when deadline recovery is delayed: post-deadline wall time cannot create
unapproved project usage. Estimated cloud cost is currently a scheduling and
abuse-control unit, not a provider invoice; provider reconciliation remains a
later milestone.

## IAM-1C session/step-up Gate — 2026-07-28

Schema 18 is an additive control-plane migration; it does not alter the PVE
worker protocol or guest image. The local two-replica Compose Gate applied the
migration with the DDL owner and verified that both API replicas use
PostgreSQL session authority, while the signer role cannot read the session
tables.

- verified OIDC credentials are represented only by SHA-256 fingerprints and
  bounded lifecycle metadata; bearer bytes are never persisted or returned;
- per-session revoke, idle expiry, issuer expiry, maximum lifetime and
  per-subject revoke-all watermarks are covered by PostgreSQL integration
  tests, including an unseen pre-watermark token;
- high-risk legacy writes return HTTP 428 without a distinct step-up key and
  reject reuse of the primary API key;
- OIDC step-up requires recent `auth_time` and, when configured, accepted AMR
  and ACR values; the Dashboard initiates `prompt=login`/`max_age=0`;
- the Dashboard session view passed a real browser login/render check with no
  console errors. In legacy mode it correctly reports that OIDC session
  authority is unavailable.

No additional PVE VM was created for this Gate because the data-plane contract
was unchanged. The prior signed-GPKG real-PVE Gate remains the worker evidence;
schema 20 must be included in the next PostgreSQL backup/restore drill.

## SCHED-2B persistent executor Gate

Persistent capacity uses a different PVE template and lifecycle from the
single-use native builder described above. Build its candidate with
`image-factory/persistent-executor/run.sh`; do not point the capacity actuator
at a job-builder template. The build inputs and exact environment are in
[the persistent-executor image runbook](../image-factory/persistent-executor/README.md).

The candidate contains the listener-free executor binary, the exact immutable
pool labels, a pinned builder binary used only for subsequent disposable VMs,
the DMI identity helper, and its systemd unit. It contains no
`executor.conf`, signing private key, PVE/PBS management credential,
Terraform state, Worker Gateway listener key, or Packer SSH authorization.
Provision `/etc/portage-engine/executor.conf` and its referenced credential
files after clone through the site's protected boot-time secret mechanism.
That file needs the least-privilege PostgreSQL, artifact, PVE and workload
issuer settings required by the four executor phases. It must not set
`WORKER_GATEWAY_TLS_KEY`: the executor has no API or Worker Gateway listener.

For the `pve-dmi-v1` actuator this mechanism is a custom cloud-init user-data
document. Put it on a PVE storage whose `content` includes `snippets` and which
is shared by every candidate node, restrict the file to the PVE service
account, and set the template allowlist's `spec.cicustom` to
`user=<storage>:snippets/<file>`. The actuator rejects a missing user-data
reference before it can create a VM. The document must write `executor.conf`
and every referenced runtime credential, then enable and start
`portage-capacity-executor.service`; the immutable template itself remains
secret-free. Rotate or remove one-off Gate snippets after their instances are
deleted.

The actuator reserves the PostgreSQL capacity instance before Terraform. Its
provider identity is exactly `portage-capacity-<uuid>` and Terraform writes
that UUID through the Telmate 3.x `smbios { uuid = ... }` block. The pre-start
helper accepts only the normalized DMI
UUID and creates `CONTROL_PLANE_ID`, `SERVER_RUNTIME_ROLE=executor`, and
`EXECUTOR_CAPACITY_INSTANCE_ID` in `/run`. Startup rejects a statically baked
`capacity-instance` capability, ambiguous provider/zone/architecture/build
mode/profile/image dimensions, a missing image generation, or any missing
phase label. The scheduler adds the DMI-derived instance label to every slot.

### Repository and PostgreSQL Gate

Run the repeatable repository entry first:

```bash
scripts/persistent-executor-gate.sh repo
```

It syntax-checks the image scripts and Packer HCL, runs the executor startup
boundary tests, exercises scale-up/scale-down replay and exact deletion
identity/generation, and runs the PVE absence-readback HTTP tests. When
`PORTAGE_TEST_DATABASE_URL` is set, the PostgreSQL integration case also
proves:

- repeated reservation and drain selection return the same durable instance;
- a stale action fence cannot persist provider state;
- draining changes only workers carrying that capacity instance label;
- a live admission/attempt worker lease rejects deletion;
- a live phase lease rejects deletion;
- stale instance generations cannot begin or complete deletion; and
- the exact owner token/generation can complete only once.

Without `PORTAGE_TEST_DATABASE_URL`, the integration test prints its explicit
skip reason. Without Packer, only Packer syntax validation is skipped. Repo
mode always prints that it did not run a live PVE Gate; its passing output is
never live-cluster evidence.

### Live PVE cycle

The live entry deliberately consumes only an already requested scheduler
action. It refuses to choose among concurrent production actions, so use an
isolated Gate deployment/database and require exactly one `requested` action.
The actuator's `-once` mode claims one fenced action and exits only after the
heartbeat or drain/delete lifecycle completes.

Prepare the candidate and runtime secret injection, then configure the
actuator allowlist with the exact worker `image_id`, worker
`image_generation`, persistent template name and `pve-dmi-v1` contract. Enable
an enforced egress policy, and set `spec.cicustom` to the protected shared
user-data snippet described above. Start the API/admission role with the same immutable
catalog and:

Disposable builders normally use DHCP. An isolated lab whose DHCP leases are
exhausted may configure the runtime cloud settings `pve_ip_config` and
`pve_gateway` (or the startup defaults `CLOUD_PVE_IP_CONFIG` and
`CLOUD_PVE_GATEWAY`) with a reserved IPv4 CIDR and a same-subnet gateway. The
settings API rejects a gateway without a static CIDR, a static CIDR without a
gateway, IPv6, and an out-of-subnet gateway. This is an operator-owned default,
not a catalog machine-spec field. Use it only with external address reservation
and a concurrency limit that prevents two builders from claiming the same
address; DHCP remains the production default.

```ini
SCHEDULER_AUTOSCALE_MODE=actuate
SCHEDULER_AUTOSCALE_PROVIDER_MAX_SLOTS=pve:1
```

For scale-up, submit or retain matched backlog until the scheduler writes one
scale-up action for the pool. Do not insert an action or instance row by hand.
Export the protected Gate inputs without copying credentials into the repo:

```bash
export PORTAGE_CAPACITY_GATE_CONFIRM=destroy-real-pve-capacity
export PORTAGE_CAPACITY_GATE_DATABASE_URL='<isolated PostgreSQL DSN>'
export PORTAGE_CAPACITY_GATE_POOL_ID='<exact pool ID from scheduler status>'
export PORTAGE_CAPACITY_GATE_KIND=scale-up
export PORTAGE_CAPACITY_ACTUATOR_CONFIG="$PWD/configs/capacity-actuator.json"
export PORTAGE_CAPACITY_SERVER_CONFIG="$PWD/configs/server.conf"
export CLOUD_PVE_TOKEN_ID='capacity@pve!actuator'
export CLOUD_PVE_TOKEN_SECRET='<from-secret-provider>'

scripts/persistent-executor-gate.sh live-once
```

The command fails if the sole action is for a different pool or direction. A
successful scale-up requires the exact provider name, positive database
generation, fresh DMI-bound worker heartbeat, complete immutable selector and
`active` instance readback.

Next remove the Gate backlog, allow active work to finish, and wait for the
configured scale-down dwell to produce exactly one scale-down action. Keep a
real admission or phase lease live for a negative run if desired: the action
must time out/retry and the VM must remain. After all leases are gone, run:

```bash
export PORTAGE_CAPACITY_GATE_KIND=scale-down
scripts/persistent-executor-gate.sh live-once
```

Scale-down marks only the instance's slots draining. Deletion begins only
after both lease classes disappear. Terraform destroy is followed by PVE API
discovery and exact-name configuration readback; a missing local workspace is
also accepted only after the same PVE absence check. The Gate reports `PASS`
only when the database action is `completed`, the same instance generation is
`deleted`, and `deleted_at` is present. A cluster VM count, tag count, manually
edited JSON file, or a generic `terraform destroy` success is not evidence.

Repeat the complete scale-up/scale-down cycle to prove a new database UUID is
allocated without reusing the deleted identity. If no complete PVE credential
pair, PostgreSQL DSN, exact pool, readable configs, `psql`, or explicit
destructive confirmation is present, `live-once` exits with code 2 and a
specific `SKIP live PVE SCHED-2B Gate` reason. Preserve the two action IDs,
instance UUIDs/generations, executor heartbeat timestamps, actuator logs and
PVE API audit records as the external release packet; never replace them with
repository test output.

### 2026-08-14 live evidence

The isolated real-PVE Gate completed with persistent-executor candidate g7
(template VMID 159). The capacity VM received reserved `10.31.0.101/24`; the
disposable builder's final PVE readback was
`ip=10.31.0.105/24,gw=10.31.0.1`, with the internal resolver set separately.
No guest mutation was used in the final cycle.

- job `a34d113e-42db-4fe1-a4ac-dcf2cbaf83d4` completed provision, build,
  verify and publish for `app-misc/hello`;
- Worker Gateway completed a 112640-byte upload with SHA-256
  `5fa98b20f4d0d322cf467b6b566e03d727a7c13a6b044985634770a421713f37`,
  and the published artifact ledger recorded the same digest and size;
- scale-up `d557f438-2af8-46a8-8da6-57fdb1de7e2e` and scale-down
  `868ec433-36cc-4d0e-a1f8-94f922fc57f9` completed for capacity instance
  `202c0e03-dd2a-4d6e-8f92-32dc37f11023`, generation 1;
- the instance row ended `deleted`, and direct PVE readback found no disposable
  or capacity VM after the cycle;
- one-off credential-bearing cloud-init snippets and the rejected g6 candidate
  were removed; g7 remains secret-free.

The run first caught a malformed g6 egress label whose digest was not bound to
its policy ID. Executor startup failed closed. The image runner, provisioner and
sanitizer now require `egress:<policy-id>@sha256:<64 lowercase hex>`, and the
rejected g6 PVE candidate was deleted before the clean g7 cycle. The redacted
machine-readable packet is
`evidence/pve/persistent-executor-gate-20260814.json`.

## Scaling behavior

Package-level parallelism is the current scaling model. Up to `MAX_WORKERS`
jobs may run concurrently, each on a separate native VM. Native VMs are not
warm-reused because emerge mutates their root; scale by cloning multiple clean
instances from the immutable template. No build VM is returned to a warm pool.

With node set to `auto`, the PVE scheduler queries live cluster resources and
selects the least-loaded eligible online node. Eligibility comes from nodes
hosting the template, or from the explicit candidate-node list when shared
storage allows every listed node to clone it. A request-level `machine_spec`
can pin a node or override cores, memory, disk, storage, network, IP config, and
VLAN.

Successful artifacts converge into the server's single `BINPKG_PATH`, followed
by an atomic `Packages` index refresh. Artifact retrieval failure makes the job
fail; it must not be reported as a successful build.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Terraform waits for an IP | `qemu-guest-agent` is installed, enabled, running, and allowed by the PVE VM config |
| Provider has no matching release | Do not replace the exact `3.0.2-rc04` pin with a `~> 3.0` prerelease constraint |
| PVE cannot find a VM config/template | Template name and selected node do not match an existing QEMU template |
| Native VM does not boot | Template uses OVMF/Q35/SCSI and retains its cloud-init drive |
| SSH host-key verification fails | Populate the configured known-hosts file; use insecure host-key mode only for an isolated lab |
| Deployment fails on permissions/systemd | Current native deployment requires root SSH and a systemd-based template |
| Pull worker never connects | Linux binary path/architecture, executable bit, service logs, gateway CA/SAN, certificate lifetime, active attempt/fence, and exact gateway egress CIDR/port |
| Pull worker cannot fetch binpkgs | `SERVER_CALLBACK_URL` is routable from the VM, DNS resolves every approved mirror host, and the catalog egress policy covers each exact URL |
| Terraform cannot read `<ssh-key>.pub` | Mount/provide both the configured private key and the exact sibling `.pub`; Terraform reads the public file while rendering the VM resource |
| Queued/ready work is `unschedulable` | Compare the job/work-item all-of requirements with live protocol-5 worker labels: phase, provider, execution zone, architecture, build mode, profile, image generation and optional egress-policy digest |
| Provision works on one replica but fails on another | Verify that only the capable replica advertises the matching provider/zone/profile/image labels, then compare its catalog, cloud credentials, SSH key pair, builder binary/digest, Terraform state and binpkg mounts |
| Builder binary disappears after a local rebuild | Do not hot-replace a single-file bind mount; use an immutable URL/digest, mount its parent directory, or recreate the server container and verify the digest inside it |
| Signing fails with `invalid GPKG Manifest record` | Use the current signer: Portage emits `BLAKE2B` before `SHA512`; the parser accepts either valid order and verifies both |
| Signed verification reports `NO_PUBKEY` | Ensure `gpg_pubkey` reaches the builder and `BINPKG_GPG_VERIFY_GPG_HOME` points to the job-local trusted keyring; do not rely on the builder host `/etc/portage/gnupg` |
| Promotion reports an immutable destination conflict | The same CPV/BUILD_ID is already published. Do not overwrite it; use a new build identity/package for the smoke or apply an explicit retention policy outside the build path |
| Install verification fails | Artifact/index URL, signer fingerprint, package/profile compatibility, and the bounded verifier logs |
| Idle VM never disappears | Non-zero instance TTL, cleanup loop logs, Terraform state, and `destroy_failed` status |
