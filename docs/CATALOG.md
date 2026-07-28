# Build catalog and profile registry

The server-owned build catalog is the control-plane boundary between a build
request and infrastructure. A client names stable IDs; the server chooses the
actual Portage profile, repositories, image generation, PVE template, mirror
bundle, provider and resource limits.

This is the implemented `IMG-0` contract. The candidate-only Packer workflow
and physical offline-object probing are implemented in `IMG-1`; the
amd64/systemd Catalyst→rootfs→QCOW2 handoff is implemented in `IMG-2`.
A live PVE bake is never release-accepted merely because it booted: consumers
only resolve images selected by a signed stable alias after bundle, smoke,
output-stamp and target-specific verification gates pass. Operational progress
belongs in the generated image-factory status document, not in this guide.

## Enable the catalog

Copy the example, replace every example hostname, commit and digest with values
from your environment, then point the server at it:

```bash
install -m 0640 configs/catalog.example.json /etc/portage-engine/catalog.json
export CATALOG_PATH=/etc/portage-engine/catalog.json
./bin/portage-server -config configs/server.conf
```

The JSON decoder is strict. Unknown fields, invalid references, duplicate IDs
and incomplete stable trust metadata stop server initialization. The catalog
is loaded at startup; restart the server after a reviewed atomic replacement.
Candidate entries may be loaded for review, but build resolution rejects them;
only `stable` entries (or the explicitly marked legacy `compatibility` mode)
can provision a builder.

If `CATALOG_PATH` is empty, the server creates a visibly marked
`compatibility` catalog for the existing single PVE template. This preserves
current deployments but is not a stable multi-profile release configuration.

## Request model

Developers submit IDs rather than infrastructure values:

```bash
./bin/portage-client build \
  -server=https://portage.example.org \
  -package=app-editors/vim \
  -profile-id=pe/amd64/glibc/systemd/base-v1 \
  -repositories=gentoo \
  -resource-class=medium \
  -wait
```

The legacy `-profile` path remains in the ConfigBundle for compatibility, but
it must map to the selected catalog profile. It cannot select a PVE template.

Every resource class must provide positive, bounded `cores`, `memory` (MiB),
`disk_size` (GiB), `max_runtime_minutes` and
`cloud_cost_microunits_per_minute`. Schema v11 uses the first three as the
durable resource reservation; schema v17 reserves the maximum runtime and
estimated cloud cost at attempt claim, then settles actual wall time at the
terminal transition. The cost rate is an operator-owned scheduling estimate,
not a live cloud billing API response. Approved cloud instance types and
`preemptible` remain optional operator-owned mappings. Template, node, storage,
network and provider endpoints are operator-owned. Unknown or incomplete
profile, repository or resource IDs are rejected before a job ID or VM is
allocated.

## Catalog ownership

| Object | Required role |
| --- | --- |
| Profile | Maps a stable ID to arch, profile path, binhost namespace, repository allowlist, image, mirror bundle and egress policy |
| Repository | Owns name, location, transport, immutable commit/digest and channel |
| Image | Owns provider, execution zone, build mode, generation, template, display model, installed package-set IDs and provenance digests |
| MirrorBundle | Names the approved offline input set, digest, freshness and advisory watermark |
| Resource class | Maps a public size ID to bounded machine resources |
| Egress policy | Binds approved runtime host labels to immutable destination CIDRs, protocols and ports |

Build jobs persist a `resolved_context` containing this complete selection.
The server replaces client repository URLs with catalog values. Before
`emerge`, native builders run `git rev-parse HEAD` for every
revision-pinned Git repository and fail closed on a mismatch. The build job
does not fetch or check out a different commit; image/mirror preparation owns
repository mutation.

Schema v20 turns that resolved selection into an exact executor-routing
contract. Job and phase requirements include provider, `execution_zone`,
architecture, build mode, profile ID, immutable image generation and the
egress-policy digest. `execution_zone` defaults to `default` for compatibility,
but multi-site/PVE deployments should assign stable operator-owned IDs such as
`lan-a` or `isolated-desktop`; it is a scheduling boundary, not a user input.
Each replica advertises only catalog entries matching its
`CLOUD_DEFAULT_PROVIDER` and `EXECUTOR_ZONES`. Missing labels leave the work
explicitly `unschedulable` rather than falling back to a similar image.

## Builder egress contract

Every non-compatibility profile must select an enforced `egress_policy_id`.
The policy is operator-owned and travels in `resolved_context` with a canonical
SHA-256 digest:

```json
{
  "id": "egress/stable-internal",
  "mode": "enforce",
  "dns_resolvers": ["10.31.0.1"],
  "rules": [
    {
      "id": "artifact-and-control",
      "hosts": ["nas.example.internal", "portage.example.internal"],
      "cidrs": ["10.31.0.2/32"],
      "protocol": "tcp",
      "ports": [80, 443]
    }
  ],
  "channel": "stable"
}
```

Hostnames are audit labels used to reject runtime URL drift; the PVE firewall
is generated only from canonical CIDRs. Wildcards, non-canonical CIDRs,
unbounded ports, URL credentials and unlisted callback, binhost, builder,
Gentoo mirror, Portage sync or repository endpoints are rejected before a VM
is allocated. DNS is allowed only to the listed resolver addresses.

For an enforced PVE job, Terraform creates the VM stopped and enables firewall
filtering on its virtual NIC. The controller requires the PVE cluster firewall
to already be enabled, applies VM `policy_in=ACCEPT` plus `policy_out=DROP`,
removes all prior VM-level outbound rules, writes the exact allow rules, reads
them back and persists
`egress-policy.json` plus `egress-policy-evidence.json` in the protected
Terraform workspace. Only then is the VM started. A mismatch or API failure
rolls the VM back. The Web UI shows the policy ID/digest on job details and the
profile-to-policy binding in Image Factory.

`compatibility` catalogs retain an explicit `disabled` policy so old trusted
deployments continue to start, but their logs contain a warning and they are
not release evidence. Enforcement currently supports PVE only; selecting an
enforced catalog with another provider fails closed.

## Binhost namespace ownership

Every profile must declare a unique `binhost_path`:

```json
{
  "id": "pe/amd64/glibc/systemd/base-v1",
  "arch": "amd64",
  "binhost_path": "releases/amd64/binpackages/23.0/x86-64_pe-systemd-base-v1"
}
```

The validator accepts only the official-style shape
`releases/<arch>/binpackages/<profile-generation>/<target>`, requires the
architecture component to equal the profile architecture, rejects traversal
and requires uniqueness across the catalog. The target suffix is an
operator-owned compatibility namespace; it should change whenever profile,
ABI, toolchain or other binary-compatibility policy changes.

The resolved build context carries this value. After signed-generation
verification, the server promotes artifacts only through the store registered
for that path and atomically regenerates that store's `Packages`. It also
uploads the same namespaced artifacts and index to the configured internal
mirror. A request, builder or artifact filename cannot select a different
namespace. `GET /api/v1/binhosts` exposes the safe profile-to-consume-path
mapping used by `portage-client configure`.

Image manifests may contain `package_sets` and a
`package_set_catalog_digest`. They must appear together. These fields describe
the immutable, reusable Portage sets already installed in the image; clients
cannot add or replace them. The resolved job context retains both fields so a
failed build can be correlated with the exact tool baseline. Set definitions
are owned by the image factory's locked package-set catalog, while target-only
packages remain in its BuildPlan.

`display_model` is part of the immutable image record when present. Current
base images use `std`; desktop verifier images use `qxl`. The image factory
binds the same value through BuildPlan, Packer custom data, PVE output readback,
candidate catalog and resolved job context.

Stable entries have stronger validation:

- stable Git repositories require a full immutable commit;
- stable non-Git repositories are rejected until a later milestone implements
  snapshot digest verification in the builder;
- stable images require a template and image digest;
- stable mirror bundles require a SHA-256 digest.

Use internal HTTP or HTTPS endpoints for control-plane services according to
the deployment trust boundary; the network policy and content digest/signature
checks remain mandatory in either case. A read-only
HTTP repository/artifact mirror is also accepted when the catalog binds a full
Git revision and snapshot SHA-256; HTTP entries cannot carry credentials,
query strings, redirects to another host, arbitrary filesystem paths or
`file://` repositories. A fully disconnected deployment also mirrors Go
modules, OCI images, Packer plugins, Terraform providers, stage/QCOW2 inputs,
distfiles and binpkgs as described in the roadmap.

## Assemble a release-group candidate

Packer writes one image manifest and a review fragment per target. Do not
hand-merge those fragments into the server catalog. Put the base and desktop
evidence under one confined evidence root and use the strict assembly spec in
`image-factory/operations/candidate-catalog.example.json`:

```bash
portage-image-factory catalog-assemble \
  -spec /srv/promotion/candidate-catalog.json \
  -output /srv/promotion/candidate-catalog.generated.json
```

The command revalidates each image against its BuildPlan, common config and
input lock, verifies the signed MirrorBundle, derives repository
revision/digest metadata from locked snapshot objects, sorts the result and
runs the normal catalog validator. Base and desktop may use different
immutable MirrorBundle IDs and different commits of the same logical
repository. Assembly gives each repository object a revision-scoped catalog
ID while retaining its Portage repository name; a profile selects exactly one
revision per name. Developer requests may continue to use the logical name
such as `gentoo`, while resolved job provenance records the immutable scoped
ID. The later `promote` gate verifies every bundle
again with the release controller's configured sync public key and requires
the image input-lock digest to match its bundle; the assembly spec cannot
replace that trust anchor.

## Missing-object report contract

`IMG-1` implements the following report shape in
`portage-image-factory preflight`. It verifies every lock object applicable to the target and returns
the complete failure set. Portage fetch-closure generation remains mirror-side
work; the signed/content-addressed closure manifest is itself a locked object.

```json
{
  "catalog_version": 1,
  "target": "base-systemd",
  "mirror_bundle_id": "mirror/2026-07-22",
  "strict_offline": true,
  "checked_at": "2026-07-22T00:00:00Z",
  "verified": 7,
  "missing": [
    {
      "kind": "distfile",
      "id": "distfiles/example-1.0.tar.xz",
      "path": "distfiles/example-1.0.tar.xz",
      "expected_digest": "sha256:...",
      "reason": "not present in offline root",
      "fallback_allowed": false
    }
  ]
}
```

Strict-offline preflight must return the whole missing set and must not fall
back to public DNS, Git, HTTP, OCI or plugin registries. `RESTRICT=fetch` and
`RESTRICT=mirror` objects require an explicit operator workflow.

## Remaining security work

The registry, PVE egress adapter, and attempt-bound outbound Worker Gateway
remove request-controlled infrastructure selection, network fallback, the
long-lived inbound builder API, and the shared builder token from the
disposable-PVE path. Every enforced catalog must include the exact HTTPS
gateway host/CIDR/port in addition to the HTTP binhost callback and mirrors.
These controls do not by themselves make the service safe for arbitrary public
tenants: OIDC/project RBAC is now enforced; the next security milestone is
per-project quota/admission and abuse
controls, followed by production issuer/signer host and network isolation.

See [ROADMAP_AND_DESKTOP_E2E.html](ROADMAP_AND_DESKTOP_E2E.html) for the gates
and the Packer/Catalyst sequence.
