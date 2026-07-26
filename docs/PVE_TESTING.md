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
SSH/SCP: deliver pinned Linux builder binary and runtime configuration
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

- the server reaches each builder on SSH and TCP 9090;
- builders authenticate with a shared `BUILDER_TOKEN`;
- builders receive only the release **public** key for a job-local verification
  keyring; neither builder nor server mounts the signing private key;
- the independent signer has no HTTP listener, pulls digest-bound tasks from
  PostgreSQL with leases/fencing, and is the only process with the private
  GPG home;
- PVE and SSH credentials are operator secrets;
- native instances are single-use and are destroyed after the pipeline.

Do not expose the server, dashboard, builder port, or PVE API directly to an
untrusted network. The target community architecture replaces inbound builder
APIs and shared tokens with outbound leases and per-worker mTLS. The release
key isolation is already implemented; worker identity and network isolation
remain future work. See
[ROADMAP_AND_DESKTOP_E2E.html](ROADMAP_AND_DESKTOP_E2E.html).

## Prerequisites

### Control plane

- Terraform available on the server `PATH`. The Compose control-plane image
  includes the repository-pinned Terraform version.
- Network access from the server to the PVE API and builder SSH/9090 ports.
- Network access from the builder VM to `SERVER_CALLBACK_URL`.
- A Linux builder binary matching the template architecture.
- PostgreSQL schema 7 with the repeatable signer-role bootstrap applied.
- Shared writable `DATA_DIR`/quarantine, `BINPKG_PATH`, public-key directory,
  and Terraform workspace paths.
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

The detailed matrix, architecture, and `IMG-0` through `IMG-4` rollout are in
[ROADMAP_AND_DESKTOP_E2E.html#profile-images](ROADMAP_AND_DESKTOP_E2E.html#profile-images).

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
one deployment. The checked-in Compose stack applies schema 7 and creates a
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
Store generated database/API/builder secrets and the signing key in a secret
manager or protected volume before restarting services. Changing
`BUILDER_TOKEN` requires rolling the builders that use it.

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
3. SSH deployment starts `portage-builder.service` on the VM.
4. Native `emerge --buildpkg` writes into a per-job PKGDIR and produces the
   requested package plus every dependency built by that task.
5. The server collects the category-preserving unsigned task closure into a
   job-private quarantine. Public `BINPKG_PATH` is unchanged.
6. Unsigned native install verification creates a fresh `PKGDIR`, downloads
   only the expected `Packages` file and exact task artifacts from the private
   capability binhost, and rejects redirects, unexpected paths, sizes, digests,
   or files. It then copies the image baseline VDB into a throwaway root,
   removes every package produced by the task, and installs with
   `--usepkgonly=y --getbinpkg=n`. No host package cache or other configured
   binhost can participate.
7. Before signing, the server submits the exact unsigned generation under the
   signer-key policy. The verifier must reject it; acceptance is a hard
   pipeline failure. This is the release Gate's required negative control.
8. The server enqueues exact input paths, sizes, and SHA-256 digests. The
   isolated signer claims the task once, rejects symlinks/path changes, creates
   embedded GPKG signatures and a clear-signed Manifest, self-verifies the
   result, and returns an output manifest.
9. A second fresh `PKGDIR` downloads only the signer's output manifest. Before
   Portage runs, the builder verifies every expected GPKG signature with a
   job-local keyring containing exactly one approved primary fingerprint. The
   throwaway-root install then enables `binpkg-request-signature`, keeps
   `--getbinpkg=n`, and rechecks every artifact digest after emerge. It must
   install the complete task closure.
10. The server immutably promotes the full signed closure, atomically
   regenerates `BINPKG_PATH/Packages`, and records `SIGNED: 1`. An existing
   destination is a hard failure; choose a package/version not already present
   when running a release-gating smoke test.
11. The job reaches `completed` with an artifact reference.
12. The native VM is destroyed after successful publication. A failed destroy
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
| Builder never becomes healthy | Linux binary path/architecture, executable bit, service logs, firewall, and port 9090 |
| Builder cannot register or fetch binpkgs | `SERVER_CALLBACK_URL` is routable from the VM and DNS resolves any mirror host |
| Terraform cannot read `<ssh-key>.pub` | Mount/provide both the configured private key and the exact sibling `.pub`; Terraform reads the public file while rendering the VM resource |
| Builder binary disappears after a local rebuild | Do not hot-replace a single-file bind mount; use an immutable URL/digest, mount its parent directory, or recreate the server container and verify the digest inside it |
| Signing fails with `invalid GPKG Manifest record` | Use the current signer: Portage emits `BLAKE2B` before `SHA512`; the parser accepts either valid order and verifies both |
| Signed verification reports `NO_PUBKEY` | Ensure `gpg_pubkey` reaches the builder and `BINPKG_GPG_VERIFY_GPG_HOME` points to the job-local trusted keyring; do not rely on the builder host `/etc/portage/gnupg` |
| Promotion reports an immutable destination conflict | The same CPV/BUILD_ID is already published. Do not overwrite it; use a new build identity/package for the smoke or apply an explicit retention policy outside the build path |
| Install verification fails | Artifact/index URL, signer fingerprint, package/profile compatibility, and the bounded verifier logs |
| Idle VM never disappears | Non-zero instance TTL, cleanup loop logs, Terraform state, and `destroy_failed` status |
