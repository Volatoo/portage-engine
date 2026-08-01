# Offline PVE image factory (IMG-1 / IMG-2 / IMG-3 / IMG-4)

This factory produces candidate `pve/amd64/native-gentoo` templates. It does
not promote them automatically:

- `base-systemd` clones an approved Gentoo seed VMID.
- `desktop-verifier` clones the accepted base candidate VMID and adds the XFCE
  verifier environment.
- `persistent-executor/` is a separate, minimal successor-image lane that
  installs the listener-free executor service and binds it to one exact
  capacity pool. It never turns the disposable `packer/` job-builder image
  into a warm worker; see [persistent-executor/README.md](persistent-executor/README.md).

`rootfs_source: "packer-base-image"` has two reviewed uses: a
security/tooling-only base successor, or the first desktop transition from an
accepted base. Its source object must be the complete `base-systemd` image
manifest. A base successor must preserve the exact profile contract; an
initial desktop transition must bind the accepted base repositories/display
and then install the reviewed desktop profile and package-set closure. This
avoids falling back to an older seed or pretending that a base image already
contains a desktop profile.

Every image-derived plan carries both `source_repositories` and
`repositories`. The former must match the accepted source image manifest
exactly; the latter is the signed repository state to install in the new
generation. This permits a reviewed desktop-profile update without pretending
that the accepted base already contained that commit. Non-image plans must not
set `source_repositories`.

`plans/*.build.json` is the single reviewed source of truth. The control binary
validates the plan, input lock, endpoints, source VMID/provenance, repository
bundle and distfile closure before it generates Packer variables or contacts
PVE. A build result becomes catalog-eligible only after `smoke-offline.sh`
clones and destroys it successfully.

## Security boundary

Both runners require `STRICT_OFFLINE=1`. Run them on a worker whose network
policy denies public egress and permits only `allowed_hosts` from the lock.
Artifact-plane endpoints may use HTTP or HTTPS, must be allowlisted, and cannot
contain userinfo, queries or fragments. HTTP is accepted only because the
BuildPlan, repository commits/signatures, closure filenames, sizes and SHA-256
digests are independently locked; it must never carry a credential. PVE, PBS,
and the NAS seed exporter use their native HTTPS control planes. The Portage
Engine server/dashboard can be brought up over HTTP on a protected LAN, and
the optional Desktop adapter has an explicit `-allow-http-control-plane` mode.
In that mode bearer tokens are plaintext on the network, redirects and URL
credentials remain forbidden, and the service must not cross an untrusted
segment. The allowlist validation complements the firewall; it does not create
one.

For Packer, install `bootstrap-offline.sh` in the trusted runner image outside
the incoming MirrorBundle and invoke it instead of invoking the bundle's
`run-offline.sh` directly. The small launcher first verifies the Linux factory
binary against an independently approved SHA-256, then uses that verifier to
check the sync-key signature, freshness, lock digest and every bundle object.
Only after that trust transition does it read execution metadata and invoke
the locked entry script; the verified entry repeats the full object preflight.
Do not calculate the approved factory digest from the incoming bundle.
`PreparePlan` also
requires every fixed Packer execution path (template, provision/sanitize,
distfile/profile helpers, smoke runner, and the Desktop guest helper when
applicable) to be present as a digest-locked `script` object.

```bash
STRICT_OFFLINE=1 /usr/local/libexec/portage-engine/bootstrap-offline.sh \
  base-systemd common.json offline-root offline-root/inputs.lock.json \
  offline-root/bundle-manifest.json offline-root/bundle-manifest.sig.json \
  /etc/portage-engine/trust/sync-public.json \
  <independently-approved-factory-sha256>
```

PVE credentials are supplied only through:

```bash
export PKR_VAR_proxmox_username='packer@pve!image-factory'
export PKR_VAR_proxmox_token='...'
```

The SSH key must be an owner-only file. No token, private key, Terraform state
or local pkrvars file is written into an image or retained as release evidence.
Before the first smoke SSH connection, the runner reads the guest's ED25519
host public key through authenticated PVE HTTPS + QEMU Guest Agent, validates
its wire encoding, writes a job-local `known_hosts`, and uses
`StrictHostKeyChecking=yes`. It never relies on TOFU/`accept-new` for a release
gate.

Set `proxmox_host_memory_headroom_mb` in the site-local common config (4096 MiB
is the reference minimum). Immediately before cloning, source-check reads the
selected node's current free memory and requires the candidate allocation plus
that headroom. Packer marks ephemeral candidates with
`image-factory-build`; the production controller must additionally serialize
or lease builds per node because a capacity check alone cannot close a
simultaneous-start race. A host OOM that kills QEMU invalidates that candidate;
do not restart it and attach the partial disk to release evidence.

## Offline root

Prepare this tree in a connected sync zone, verify upstream signatures there,
then transfer it as an immutable bundle:

```text
offline-root/
├── inputs.lock.json
├── packer/
│   ├── bin/packer
│   └── plugins/github.com/hashicorp/proxmox/
│       ├── packer-plugin-proxmox_v1.2.3_x5.0_linux_amd64
│       └── packer-plugin-proxmox_v1.2.3_x5.0_linux_amd64_SHA256SUM
├── terraform/
│   ├── bin/terraform
│   ├── .terraform.lock.hcl
│   ├── terraform.rc
│   └── providers/registry.terraform.io/telmate/proxmox/
│       └── terraform-provider-proxmox_3.0.2-rc04_linux_amd64.zip
├── plans/{base-systemd,desktop-verifier}.build.json
├── package-sets/catalog.json
├── images/base-systemd.image-manifest.json  # required by desktop
├── tools/portage-image-factory-linux-amd64
├── factory/                            # every consumed script is lock-listed
├── seeds/pbs-vm-<vmid>-<snapshot>.attestation.json
├── repositories/{gentoo,pe-profiles}-<full-commit>.bundle
├── keys/pe-profiles-release.asc
└── distfiles/{base-systemd,desktop-verifier}.MANIFEST.json
```

Start from a reviewed draft lock whose objects name the intended paths, kinds,
platforms, executable bits and target scope. Materialize byte sizes and
SHA-256 values from the completed tree before sealing it:

```bash
portage-image-factory lock-materialize \
  -draft inputs.lock.draft.json \
  -root /srv/portage-engine-sync/offline-root \
  -output /srv/portage-engine-sync/offline-root/inputs.lock.json
```

The command rejects symlinks, path escapes, missing files and executable-bit
drift. It does not make the bundle trusted; `bundle-seal` and an independent
sync signing key remain required afterward.

Packer is pinned to `1.15.4`; the Proxmox plugin is pinned to `1.2.3`. Packer
1.11+ requires the adjacent `_SHA256SUM` file. Install a downloaded plugin into
the staging tree with `packer plugins install --path`, then lock both files.
The runner never calls `packer init` and performs a full plugin-backed
`packer validate`, not only an HCL syntax check.

Terraform uses the packed filesystem-mirror layout and locks the original
provider zip. Do not copy only the extracted provider executable: Terraform's
dependency lock authenticates the complete release package and will reject a
partial unpacked directory during offline `init -lockfile=readonly`.

The BuildPlan also fixes PVE display hardware: `display_model` is `std` for
base images and `qxl` for desktop verifier images. Packer writes that exact VGA
model, records it in custom data, and the output stamp verifies PVE read-back.
Image-derived plans also declare `source_display_model`; source-check compares
it with the actual PVE source config (treating an omitted PVE VGA as `std`) so
a mutable hardware edit cannot hide behind an otherwise valid manifest digest.
The first desktop generation uses `rootfs_source: packer-base-image`.
Subsequent desktop-only successors use `rootfs_source:
packer-desktop-image`, so execution-surface or hardware-only changes can wrap
an accepted desktop template without recompiling its complete package set;
the source image manifest must still match the exact desktop profile, parent
chain and repository revisions.

Every executable/tool object also declares a `platform` such as
`linux-amd64`. Preflight compares Packer, its plugin/checksum, Terraform, its
provider and Catalyst runtime with the actual `GOOS-GOARCH` of the runner. A
macOS/arm64 control workstation therefore cannot accidentally validate and
attempt to execute a Linux/amd64 offline bundle. `service-binary` records its
target platform but is not compared with the image-factory runner because it
runs inside the guest.

Populate the Terraform mirror with `terraform providers mirror`, create a
Linux provider lock, and copy `offline/terraform.rc.example` unchanged to
`offline-root/terraform/terraform.rc`. The smoke runner expands its single
provider-directory placeholder into a job-local owner-only CLI config. Plan
preparation rejects a hard-coded path, additional provider-installation rules,
or a `direct` fallback.

## Locked repositories, profile chain, and distfile closure

Every repository in the BuildPlan has an explicit lock object, internal URI and
full commit. The bundles are copied into the guest and used as fetch sources;
the image build does not fetch commits from the network. The Gentoo commit has
a locked developer public key and must pass `git verify-commit`; a source
template's `metadata/timestamp.commit` selects the revision but is not itself a
signature. When a webrsync seed has no `.git`, the disposable candidate
unbundles the reviewed commit/tree into a fresh repository, records the
explicit shallow boundary when applicable, verifies the signature, and only
then replaces the seed tree. An external profile
also declares its repository, exact ordered parent chain and independent
verification-key object. Packer verifies the profile commit signature and the
repository's `profiles/repo_name` plus `parent` file before changing
`make.profile`.

Each target has one `distfile-manifest` object. Its strict JSON schema is shown
in `offline/*.MANIFEST.example.json`. Every entry contains an allowlisted
HTTP(S) URI, filename, byte size and lowercase SHA-256. Redirects are disabled,
and the guest verifies the complete closure before `emerge`; an incomplete or
modified closure fails when public egress is denied. Resolve `RESTRICT=fetch`
inputs before approving the lock. When HTTPS uses a private CA, add its PEM
certificate as a non-executable `.crt` `ca-bundle` object and reference it with
`trusted_ca_object_id` in the BuildPlan. Plain HTTP needs no CA object, but it
does not relax digest/signature, allowlist, firewall or no-credential rules.

Generate the closure on a disposable Gentoo sync worker whose repositories
are already pinned to the reviewed commits. Pass the exact Portage profile,
not merely the target name:

```bash
sudo image-factory/sync/generate-closure.sh \
  catalyst-profile-systemd \
  562f512d37e15e97551769962528e12ceea8714b \
  pe-profiles:portage-engine/amd64/23.0/no-multilib/systemd/base \
  stage3-amd64-nomultilib-systemd-20260719T170103Z.tar.xz \
  http://mirror.internal/gentoo \
  catalyst-packages.txt \
  catalyst-profile-systemd.MANIFEST.json \
  evidence/closure
```

The resolver safely extracts the same signed stage3 consumed by Catalyst,
selects the exact profile with a minimal Portage config, then runs Portage
inside that stage3 chroot. A private mount namespace binds only the reviewed
repositories, `/dev`, `/proc`, `/sys` and a clean DISTDIR into the chroot, so
both target and build dependencies use the stage3 VDB instead of the sync
worker. It fetches the complete deep/`--newuse` `@world` transition and
selected package-set graphs with build dependencies. The generated Catalyst
fsscript consumes that same closure and reconciles both graphs before sealing
the stage4, so inherited stage3 Python targets or USE state cannot be deferred
silently to Packer. It
must not resolve against the sync worker's installed VDB: doing so can omit a
dependency that exists on that worker but not in the stage3. A completely
empty VDB is not equivalent either, because it introduces bootstrap cycles
that the stage3 has already resolved. `evidence/closure/resolver.json` records
the profile, stage3 digest, stage3-VDB mode and normalized resolver-config
digest.

The initial desktop transition is different: resolve it from a disposable
clone of the exact base candidate that already passed Packer and Terraform
smoke, not from the Catalyst stage3 and not from the sync worker VDB. Copy the
accepted base image manifest into that clone and run:

```bash
sudo image-factory/sync/generate-image-closure.sh \
  desktop-verifier \
  562f512d37e15e97551769962528e12ceea8714b \
  pe-profiles:portage-engine/amd64/23.0/no-multilib/systemd/desktop-verifier \
  aacfbcf0d1735821c3647e8c9ce0b0e8ed0ff92c \
  base-systemd.image-manifest.json \
  http://mirror.internal/gentoo \
  desktop-packages.txt \
  desktop-verifier.MANIFEST.json \
  evidence/desktop-closure
```

This path keeps the accepted clone's `/var/db/pkg` as the resolver VDB, uses a
job-scoped `PORTAGE_CONFIGROOT` for the desktop profile, and fetches into a
clean ephemeral DISTDIR. Its evidence binds the full base image-manifest hash,
image digest, template/generation and a canonical pre-resolution VDB digest.
It also requires and records the exact signed profile-repository commit and
Git tree, and rejects tracked worktree drift before dependency resolution.
The clone is disposable; the source template is never modified.

## Shared package sets

Do not copy the same package list into every profile. Copy
`package-sets/catalog.example.json` into the offline root as
`package-sets/catalog.json`, review it, and lock it as the single
`package-set-catalog` object. A BuildPlan selects stable IDs:

- `pe/runtime-v1`: cloud-init, qemu-agent, SSH and image trust/runtime tools;
- `pe/build-test-v1`: common CMake/Meson/Ninja/pkg-config/Git build tooling and
  the small smoke package;
- `pe/desktop-verifier-v1`: includes the two common sets and adds XFCE/Xorg/VNC,
  AT-SPI/Python GI accessibility support, deterministic keyboard/mouse input and
  screenshot capture
  evidence tools.
- `pe/catalyst-boot-v1`: includes the runtime/build-test sets and adds the
  kernel plus UEFI GRUB needed by the Catalyst disk-assembly lane.

The factory validates includes, rejects cycles and duplicate IDs, expands and
deduplicates the selected sets, and binds the catalog digest into the Packer
result and image manifest. The guest writes the resolved atoms to
`/etc/portage/sets/portage-engine-image` and installs
`@portage-engine-image`. The BuildPlan `packages` list is only for small,
target-specific additions; an empty list is normal. Any set membership change
requires a new set ID such as `pe/build-test-v2`, a rebuilt distfile closure and
a new image generation.

## Source-template provenance

The BuildPlan uses `source_vmid`, not a mutable name lookup. Before importing a
seed into PVE, calculate the locked seed digest and set the template description
marker:

```text
portage-engine-provenance=sha256:<locked-seed-digest>
```

When bootstrapping from an existing PVE template, first create a stopped-mode
VMA backup on directory storage, then run `export-pve-seed.sh` as root on the
node that owns that backup. The exporter:

1. verifies that the VMID is a stopped QEMU template and the backup belongs to
   that VMID;
2. hashes the exact VMA archive and uploads it under its content digest;
3. downloads the complete object again and verifies size plus SHA-256;
4. uploads a seed manifest and reads it back; and only then
5. writes the provenance marker to the source template.

```bash
sudo env \
  NAS_API_URL=https://mirror.internal \
  NAS_USERNAME="$NAS_USERNAME" \
  NAS_PASSWORD="$NAS_PASSWORD" \
  NAS_DIRECTORY=portage-engine/image-factory/seeds \
  image-factory/export-pve-seed.sh \
  9200 local:backup/vzdump-qemu-9200-YYYY_MM_DD-HH_MM_SS.vma.zst \
  /var/lib/vz/dump/seed-9200.manifest.json
```

`NAS_API_URL` must use HTTPS; `NAS_CA_BUNDLE` can supply an internal CA. The
exporter intentionally refuses plaintext HTTP because it would expose the NAS
password, session cookie and seed. PVE's generic API can create a local VMA
backup but cannot download or hash that file; use this node-local exporter, a
PBS-backed workflow, or another reviewed HTTPS artifact-storage integration.
Never substitute a digest of PVE metadata for the archive digest.

For PBS, add a dedicated PVE storage with a least-privilege backup token and a
pinned PBS certificate fingerprint. Back up the stopped source template, run a
PBS server-side verify, restore it to a disposable template, and boot a fresh
linked clone before accepting the source. The locked source evidence must bind
the datastore, backup group and timestamp, verification task/state, decoded
`index.json` digest, `qemu-server.conf` digest, and every image-index checksum.
The decoded PBS index is the manifest trust anchor; it is not itself the raw
disk image. Do not record a PVE resource-list or backup-list digest as image
content provenance.

Protect the accepted snapshot before generating provenance. PBS protection is
not the same as verification: verification checks chunks now, while protection
prevents a later prune from deleting the source generation. Supply the token
secret through `PBS_PASSWORD`, never a command-line argument:

```bash
export PBS_REPOSITORY='pve-backup@pbs!pve@10.31.0.3:8007:portage-engine'
export PBS_FINGERPRINT='<pinned-sha256-fingerprint>'
export PBS_PASSWORD='<token-secret-from-protected-store>'
proxmox-backup-client snapshot protected update \
  'vm/9200/2026-07-22T15:44:32Z' true
```

After downloading a fresh snapshot API record showing `protected: true`, build
the source attestation from the original decoded manifest/config and retained
restore-gate evidence:

```bash
portage-image-factory pbs-attest \
  -pbs-url https://10.31.0.3:8007 \
  -fingerprint "$PBS_FINGERPRINT" -datastore portage-engine \
  -snapshot pbs-snapshot.json -index index.json \
  -qemu-config qemu-server.conf \
  -first-boot-log guest-first-boot.log \
  -runtime-log guest-runtime.log \
  -second-cloud-init-log guest-second-cloud-init.log \
  -cleanup pve-cleanup.json -restore-vmid 9300 -smoke-vmid 9401 \
  -output pbs-vm-9200-20260722T154432Z.attestation.json
```

The command fails closed on an unprotected or unverified snapshot, identity or
checksum drift, encrypted image indexes, `ciupgrade != 0`, incomplete boot
evidence, implicit `emerge --sync`, or leftover temporary VMIDs. The resulting
file is locked with kind `pbs-source-attestation`; the BuildPlan must use
`rootfs_source: approved-pbs-snapshot` and match its VMID and template name.

Finally bind the stopped PVE source template to the exact attestation file.
`pbs-stamp-source` checks the live name/template/`ciupgrade` contract, writes
the provenance marker, and reads it back before producing stamp evidence:

```bash
export PKR_VAR_proxmox_username='packer@pve!image-factory'
export PKR_VAR_proxmox_token='<token-secret-from-protected-store>'
portage-image-factory pbs-stamp-source \
  -common image-factory/common.local.json \
  -attestation pbs-vm-9200-20260722T154432Z.attestation.json \
  -output pbs-vm-9200.source-stamp.json
```

Hash the attestation only after it is final, add it to `inputs.lock.json`, then
seal a new bundle. The stamp evidence is operational audit data; the locked
attestation digest is the value checked by `source-check`.

Every Gentoo cloud-init template and clone must explicitly set `ciupgrade=0`
(`ciupgrade = false` in the Telmate provider). PVE otherwise emits package
upgrade metadata and cloud-init invokes `emerge --sync` on first boot, creating
an uncontrolled network mutation. Source preflight rejects the implicit state,
and output stamping writes then reads back the disabled value.

The base candidate initially carries the locked BuildPlan digest. After its
Terraform smoke succeeds and the disposable VM is destroyed, the smoke runner
stamps the PVE template with the exact generated image-manifest file digest.
Before building `desktop-verifier`, copy that base manifest to
`offline-root/images/`, set the desktop plan's `source_vmid` to the accepted
base VMID, and rebuild the lock. The desktop plan references
`image-manifest/base-systemd-g1`, so the factory verifies template flag, name,
VMID and the actual base-output manifest digest before cloning.

## Build

1. Copy `common.example.json` to `common.local.json` and replace site values.
2. Replace every placeholder commit, VMID, generation, hostname, digest and
   size in the plans and input lock. The plan files in the repository and
   offline root must be byte-identical.
3. Build and run the base target:

```bash
make build-image-factory
STRICT_OFFLINE=1 image-factory/run-offline.sh \
  base-systemd \
  image-factory/common.local.json \
  /srv/portage-engine-offline \
  /srv/portage-engine-offline/inputs.lock.json
```

Before Packer runs, the factory emits `plan-evidence.json` and generated
pkrvars under `packer/output/`. The guest then:

- checks out every full repository commit from its locked bundle, verifies the
  signed external profile commit and removes ignored or untracked files;
- verifies the profile repository identity and exact parent chain, selects the
  reviewed profile, and reconciles `@world` with
  `--update --deep --newuse --with-bdeps=y`;
- hydrates the content-addressed distfile closure and installs the locked
  `@portage-engine-image` set;
- records `emerge --info`, the post-reconcile world depgraph and package-set
  depgraph;
- validates systemd/cloud-init/qemu-agent/desktop commands and removes host
  keys, machine identity, cloud-init state, leases, random seed, journals and
  secret-like configuration.

## Disposable Terraform smoke gate

Run the smoke gate against the generated candidate manifest:

```bash
STRICT_OFFLINE=1 image-factory/smoke-offline.sh \
  base-systemd \
  image-factory/common.local.json \
  /srv/portage-engine-offline \
  /srv/portage-engine-offline/inputs.lock.json \
  image-factory/packer/output/base-systemd.image-manifest.json
```

The smoke runner uses the locked Terraform binary, provider mirror and provider
lock. It clones a disposable VM, obtains its address through qemu-guest-agent,
waits for cloud-init, checks the depgraph, builds `app-misc/hello`, runs
cloud-init a second time, performs the desktop/Xvfb smoke when applicable, and
always runs `terraform destroy`. If destroy fails, the recovery directory is
retained and promotion is blocked.

Current PVE cloud-init data uses the deprecated scalar `user` key. Cloud-init
25 reports `status: done` with exit code 2 even when there are no hard errors.
The smoke gate parses JSON status and permits only that exact deprecation;
unknown recoverable errors and all hard errors still fail the run. Both runs
also reject any `emerge --quiet --sync` in the cloud-init logs.

Evidence is written to `packer/output/<target>.smoke-evidence/`. Only a result
with `terraform_destroyed: true` and `output_provenance_stamped: true` permits
review of the generated `<target>.catalog-candidate.json`. Treat that file as
a review fragment only. After all release-group images pass, use
`catalog-assemble` with `operations/candidate-catalog.example.json`; do not
hand-merge fragments or invent repository/MirrorBundle metadata.

## Catalyst rootfs lanes (IMG-2 / IMG-3)

The Catalyst implementation under `catalyst/` has two explicit targets. IMG-2
uses the official `default/linux/amd64/23.0/systemd` profile. IMG-3 uses one
signed external profile repository. Catalyst 4.1.1 accepts the generated
`repo:profile` selector and `repos:` source; the runner verifies the external
commit, repository identity and exact parent chain before Catalyst starts.

The IMG-2 rootfs manifest records nine semantic inputs: plan, complete
Catalyst runtime archive, stage3, signed stage3 DIGESTS, Gentoo stage-release
key, Gentoo Git bundle, the exact Gentoo commit-signing public key, distfile
closure and package-set catalog. The stage-release and repository keys are
separate trust roots and are never substituted for one another. Its input-lock
digest additionally binds the Linux image-factory binary and every execution
script. `bootstrap-offline.sh` hashes those objects before it executes the
locked runner, then the runner performs the normal full preflight. The runtime archive
must expand to `bin/catalyst`, `share/catalyst`, and exactly one
`lib/pythonX.Y/site-packages` (or `dist-packages`) containing Catalyst and all
Python dependencies. The runner uses Python `-S`, discards host/user package
directories and verifies Catalyst, Portage, Gemato, snakeoil, fasteners,
tomli and the `DeComp` module from pydecomp all import from the locked
runtime (stdlib remains allowed).
`safe-extract.py`
rejects path escapes, special files and oversized archives. Replace all zero digests,
sizes, fake commits and example endpoints in `catalyst/inputs.lock.example.json`
before use. The example runtime ID follows the official `4.1.1` tag; the
archive digest, not the mutable version label, is the actual trust anchor.
The IMG-3 target adds exactly two inputs: the external profile bundle and its
independent public verification key. Use
`catalyst-profile-systemd.plan.json`; its rootfs manifest retains the profile
repository commit and ordered parents.

Create the runtime on a dedicated minimal Gentoo sync worker after installing
the reviewed Catalyst ebuild and its runtime dependencies:

```bash
SOURCE_DATE_EPOCH=0 image-factory/catalyst/build-runtime.sh \
  /srv/portage-engine-sync/catalyst-runtime-4.1.1-python3.14.tar.xz \
  image-factory/catalyst/verify-runtime.py
```

The builder copies the worker's complete system `site-packages` tree so lazy
imports cannot fall back to the offline worker, then runs the same `python -S`
import-origin gate used by the factory. Keep this sync worker minimal: extra
Python distributions enlarge the reviewed runtime and its attack surface.

Distfile hydration disables inherited proxy settings as well as redirects, so
an allowlisted URI is contacted directly rather than through an unreviewed
proxy. The Catalyst snapshot and stage4 processes then run in a network
namespace with no interfaces. The runner resolves the locked hydrator through
the sibling `factory/packer/scripts` path, which is identical in the source
tree and materialized offline bundle. After every object has passed size and
SHA-256 verification, it sets only the bind-mounted DISTDIR root to `0755` and
the files to `0644`; Catalyst/Portage's unprivileged fetch user must be able to
traverse that mount, while the rest of the private work directory stays
`0700`.

On a disposable root Linux worker with `python3`, `git`, `gpg`, `gpgv`,
`unshare`, `jq`, `sha256sum`, `tar2sqfs`, `rsync`, `tar`, and `xz` installed:

```bash
make build-image-factory
sudo STRICT_OFFLINE=1 image-factory/catalyst/bootstrap-offline.sh \
  catalyst-base-systemd \
  image-factory/catalyst/catalyst-base-systemd.plan.json \
  /srv/portage-engine-catalyst \
  /srv/portage-engine-catalyst/inputs.lock.json \
  /var/lib/portage-engine/catalyst-output \
  /etc/portage-engine/trust/sync-public.json \
  <approved-factory-sha256>
```

The final factory digest in that example must come from an independently
approved, out-of-band deployment record; do not calculate it from the incoming
bundle at execution time. The trusted bootstrap checks that digest first, then
uses the pinned factory binary to verify the bundle signature and freshness
before trusting the input lock. The runner then verifies the signed stage3 hash
with the stage-release key and the signed repository commit with its separately
locked commit-signing key,
hydrates only the locked internal distfile closure, creates a bare repository
whose `master` and plan-named snapshot ref both resolve to the exact commit,
then runs both Catalyst invocations in a fresh network namespace. The special
snapshot value `stable` is rejected because Catalyst uses it to fetch upstream.
The sync zone should resolve and bundle the signed commit from Gentoo's
`stable` branch; the offline plan uses a non-special local ref for that commit.
It generates and hashes `catalyst.conf`, stage4 spec,
offline envscript, fsscript, root overlay and expanded package set before the
build. A failure retains its work directory; a success emits a rootfs and
`catalyst.rootfs-manifest.json`.

Assemble and import the seed on the appropriate Linux/PVE hosts:

```bash
sudo STRICT_OFFLINE=1 image-factory/catalyst/assemble-qcow2.sh \
  /var/lib/portage-engine/catalyst-output/stage4-amd64-pe-g1.tar.xz \
  /var/lib/portage-engine/catalyst-output/catalyst.rootfs-manifest.json \
  /var/lib/portage-engine/catalyst-output/pe-gentoo-amd64-systemd-catalyst-g1.qcow2 \
  ./bin/portage-image-factory

sudo image-factory/catalyst/import-pve-seed.sh \
  /var/lib/portage-engine/catalyst-output/pe-gentoo-amd64-systemd-catalyst-g1.qcow2 \
  /var/lib/portage-engine/catalyst-output/pe-gentoo-amd64-systemd-catalyst-g1.qcow2.manifest.json \
  9001 gentoo-amd64-catalyst-seed-g1 local-lvm vmbr0 ./bin/portage-image-factory
```

The import refuses an existing VMID and discovers the actual imported PVE
volume instead of assuming a disk number. It stamps the seed with the exact
QCOW2-manifest digest. The seed contract also fixes `ciupgrade=0`,
`ciuser=root`, and `ipconfig0=ip=dhcp`; without the last field PVE can emit a
NoCloud network document containing only DNS, leaving the guest interface
unmanaged and making Packer wait for SSH until timeout. `source-check`
revalidates all three fields before cloning. IMG-2 installs an unsigned
removable-path Gentoo GRUB,
so the importer creates
the OVMF vars disk with `pre-enrolled-keys=0`. Secure Boot must remain disabled
until a separate, reviewed shim/GRUB/kernel signing and key-rotation chain is
implemented and gated.

Copy that manifest into the successor IMG-1 lock as a `qcow2-manifest` object,
update the real VMID/digests in `plans/base-systemd.catalyst.build.json`, then
run the existing gates with the alternate reviewed plan:

```bash
export IMAGE_FACTORY_PLAN="$PWD/image-factory/plans/base-systemd.catalyst.build.json"
STRICT_OFFLINE=1 image-factory/run-offline.sh base-systemd \
  image-factory/common.local.json /srv/portage-engine-offline \
  /srv/portage-engine-offline/inputs.lock.json
STRICT_OFFLINE=1 image-factory/smoke-offline.sh base-systemd \
  image-factory/common.local.json /srv/portage-engine-offline \
  /srv/portage-engine-offline/inputs.lock.json \
  image-factory/packer/output/base-systemd.image-manifest.json
```

## Operations and promotion (IMG-4)

The operational control plane is documented in `operations/README.md`. It
adds two independent Ed25519 trust roots: the sync-zone key seals a complete
offline bundle, while the release key signs promotion receipts and alias
changes. Bundle verification re-hashes every locked object and enforces the
advisory watermark plus `fresh_until` at use time.

Promotion is release-group atomic. Every image in the candidate catalog must
have a matching image manifest, successful Terraform smoke/destroy result and
PVE output stamp. `catalog-assemble` produces that catalog from verified image,
BuildPlan, input-lock and signed-bundle evidence; operators do not hand-merge
per-image JSON fragments. A base image and its later desktop derivative may
use different immutable MirrorBundle IDs and different signed revisions of a
logical repository. Repository catalog IDs are revision-scoped, while each
profile still resolves exactly one revision for each Portage repository name.
Promotion checks every bundle with
the controller-owned sync public key and binds each image's input-lock digest
to the matching bundle before writing a new immutable release directory and
atomically replacing one signed alias envelope. Rollback can select only a
previously signed release receipt/catalog pair whose complete bundle set is
still fresh.

Periodic rebuild and generation cleanup commands emit decisions only. Cleanup
requires a signed lease snapshot and never marks an aliased, newest, unretired,
or actively leased generation for deletion. Applying PVE template deletion is
kept as a separate reviewed operation.

## Operator status and WebUI

The dashboard's **Image Factory** page is a read-only view of the active
server catalog and an operator-produced milestone snapshot. It does not scan a
checkout, call PVE/PBS, or accept credentials. Compile a strict snapshot and
point the server at it:

```bash
./bin/portage-image-factory status-compile \
  -input image-factory/status.example.json \
  -output /var/lib/portage-engine/image-factory-status.json

export IMAGE_FACTORY_STATUS_PATH=/var/lib/portage-engine/image-factory-status.json
```

The schema rejects unknown fields, duplicate milestone/step IDs, invalid
states, reversed step timestamps, oversized collections and malformed log
references. Each milestone may provide bounded `steps` with state, start/end
times, an operator-reviewed summary and an optional digest-bound log artifact.
Raw Packer, Terraform, Catalyst, PVE/PBS or GUI command output is never embedded
in the status document: scrub it, store it as an evidence artifact, and publish
only its label, relative path, SHA-256, time and size. The WebUI intentionally
has no promotion, rollback, PVE or PBS mutation endpoint: those operations
retain the independent signature, evidence and review gates described above.

The checked-in examples remain placeholders; do not infer the active release
from example generation names. The documented PVE reference runs established
two image invariants: an empty
`/etc/machine-id` must be mode `0644` so `systemd-networkd` can generate a DHCP
DUID, and repositories inherited from Catalyst must be normalized to
`root:root` before Git operations rather than bypassing ownership checks with
`safe.directory`. Read the generated image-factory status endpoint and signed
stable alias for current generations and gates; historical rejected candidates
in the roadmap are audit evidence, not active blockers.

[Packer plugin installation]: https://developer.hashicorp.com/packer/docs/plugins/install
[Terraform provider mirror]: https://developer.hashicorp.com/terraform/cli/commands/providers/mirror
