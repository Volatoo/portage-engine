# Using Portage Engine

Portage Engine has two distinct jobs, and it is important to keep them separate:

| Job | Who does it | How |
| --- | --- | --- |
| **Consume** prebuilt packages | Portage itself (`emerge`) | Configure a binhost once, then `emerge --getbinpkg` |
| **Request** a build | `portage-client build` | Only needed because Portage has no native "ask the binhost to build X" |

Installing packages is **not** done by a custom client — it is done natively by
Portage against the server's binhost. The client is a management/request tool.

---

## 1. Consuming packages (the normal path)

The dashboard's public `/packages` page searches already published packages
across catalog profiles and links to their immutable binhost paths. `/docs`
contains the anonymous quick start, and `/status` exposes only coarse service
availability. These public pages do not expose build jobs, logs, dependency
metadata, storage credentials, topology, or detailed health reasons.

The server exposes Gentoo-style release namespaces below `/binpkgs/`. Each
catalog profile owns one real `PKGDIR` and one independent `Packages` index:

```text
/binpkgs/releases/amd64/binpackages/23.0/x86-64_pe-systemd-base-v1/Packages
/binpkgs/releases/amd64/binpackages/23.0/x86-64_pe-desktop-verifier-v1/Packages
```

This follows the official `releases/<arch>/binpackages/<generation>/<target>`
shape while keeping Portage Engine targets distinct from Gentoo's official
targets. `/binpkgs/` is only the directory tree root; it is not an aggregate
binrepo and intentionally has no cross-profile `Packages` index.

### One-time binhost setup

Use HTTPS outside an isolated development network. The current client can write
the binrepo file, but it does **not** install or trust the signing key:

```bash
sudo ./bin/portage-client configure -server=https://portage.example.org
```

With no `-profile-id`, the command asks the server for the catalog's default
profile. Select another explicitly when the machine uses a different profile:

```bash
sudo ./bin/portage-client configure \
  -server=https://portage.example.org \
  -profile-id=pe/amd64/glibc/systemd/desktop-verifier-v1
```

This writes `/etc/portage/binrepos.conf/portage-engine.conf` with the selected
profile's exact path:

```ini
[portage-engine]
priority = 1
sync-uri = https://portage.example.org/binpkgs/releases/amd64/binpackages/23.0/x86-64_pe-systemd-base-v1
verify-signature = true
```

Do not shorten this to `/binpkgs` and do not point one binrepo at multiple
profiles. If a machine intentionally consumes more than one compatible
namespace, create separate `binrepos.conf` sections and use explicit
priorities. On Portage
3.0.74 and newer, `verify-signature = true` requires valid signatures for this
binrepo. Older clients should use the global feature instead:

```bash
FEATURES="${FEATURES} binpkg-request-signature"
```

### Establish signing trust

Obtain the release key's full primary fingerprint through an authenticated,
operator-controlled channel. Do not use the same HTTPS origin as both package
source and only trust root. The recommended one-time flow verifies that
independent fingerprint before it imports the public key into Portage's
keyring and writes `binrepos.conf`:

```bash
sudo portage-client setup \
  -server=https://portage.example.org \
  -expected-fingerprint=<FULL_PRIMARY_FINGERPRINT> \
  -profile-id=pe/amd64/glibc/systemd/base-v1
```

The public-key endpoint is intentionally unauthenticated because stock
community consumers need the key before they have a platform account. It is
not a trust anchor by itself; the full fingerprint comparison above is
mandatory. The manual equivalent is:

```bash
curl --fail --show-error \
  https://portage.example.org/api/v1/gpg/public-key \
  -o portage-engine.asc

gpg --show-keys --with-fingerprint portage-engine.asc
```

Compare the displayed fingerprint with the independently published value. Then
initialize Portage's verification keyring and import the approved key there:

```bash
sudo getuto
sudo gpg --homedir=/etc/portage/gnupg --import portage-engine.asc
sudo gpg --homedir=/etc/portage/gnupg --check-trustdb
```

Importing a key is not the same as trusting it. Certify and assign trust to the
key according to the operator's key-rotation policy before using the binhost.
The exact interactive procedure is documented in the
[Gentoo Binary Package Guide](https://wiki.gentoo.org/wiki/Binary_package_guide#Verifying_OpenPGP_signatures_on_binary_packages).

### Enable binary fetching

Either per invocation:

```bash
emerge --getbinpkg dev-lang/python
```

or make it the default in `/etc/portage/make.conf`:

```bash
FEATURES="${FEATURES} getbinpkg"
```

With `--getbinpkg`, emerge fetches the prebuilt package when the binhost has it
and **falls back to a normal source build** when it does not. There is nothing
else to install or wrap.

---

## 2. Requesting a build (optional)

Portage cannot ask a binhost to build something it lacks. When you want the
server to build a package — typically with specific USE flags — use the client:

```bash
# Request a build and wait for it to finish
./bin/portage-client build \
  -server=https://portage.example.org \
  -package=dev-lang/python -version=3.11 \
  -profile-id=pe/amd64/glibc/systemd/base-v1 \
  -resource-class=medium \
  -use=ssl,threads,sqlite -wait
```

Flags:

| Flag | Meaning |
| --- | --- |
| `-server` | Server URL (default `http://localhost:8080`) |
| `-token` | Portage Engine federated session token, or set `PORTAGE_ENGINE_TOKEN` |
| `-project` | Authorized project name/UUID, or set `PORTAGE_ENGINE_PROJECT` |
| `-api-key` | Legacy administrator key, or set `PORTAGE_ENGINE_API_KEY` |
| `-package` | Package atom, e.g. `dev-lang/python` |
| `-version` | Package version (optional) |
| `-use` | USE flags, comma-separated |
| `-keywords` | Keywords, comma-separated |
| `-config` | A Portage config JSON file (see below) |
| `-portage-dir` | Read config from a Portage dir (see [SYSTEM_CONFIG_USAGE.md](SYSTEM_CONFIG_USAGE.md)) |
| `-arch` | Target architecture; it must match the selected catalog profile |
| `-profile-id` | Server catalog profile ID |
| `-profile` | Legacy Portage profile path used only for catalog compatibility mapping |
| `-repositories` | Approved catalog repository IDs, comma-separated |
| `-resource-class` | Server catalog resource class |
| `-wait` | Block until the build reaches a terminal state |

Check a job later:

```bash
./bin/portage-client status -server=https://portage.example.org -job=<job-id>
```

The dashboard build-detail and logs pages split the live stream into
`queued`, `provision`, `deploy`, `build`, `collect`, `verify`, `sign`,
`publish`, and `cleanup`.
Each stage shows line count, first/last ingestion time and its latest event;
the complete bounded log remains filterable. The server strips ANSI control
sequences, adds UTC timestamps, redacts common authorization/password/token
forms, and reports when the in-memory log had to drop its middle section.

Once the build completes, the package lands on the binhost and any client with
`--getbinpkg` will pick it up — so the two halves connect: **request once,
consume everywhere.**

The builder emits unsigned GPKG files into a private quarantine. After the
first install check, an independent outbound-pull signer signs an exact
digest-bound generation; a second Portage install check requires that
signature before the server immutably promotes the complete package closure.
Neither the builder nor the API server has the signing private key.

Both install checks use a new local `PKGDIR`. The verifier prefetches only the
manifest-bound artifact paths and checks their sizes and SHA-256 digests, then
runs emerge with `--usepkgonly=y --getbinpkg=n`; the host cache and any
unrelated `binrepos.conf` entry cannot satisfy the request. Signed mode also
requires exactly one configured signer fingerprint and independently verifies
every GPKG before emerge. As a mandatory negative control, the same unsigned
generation must be rejected under the signer policy before signing is allowed
to start. A missing public key or key ID is a hard failure, never an unsigned
downgrade.

The catalog IDs are resolved by the server; clients cannot select repository
URLs, PVE templates, storage or network settings. See [CATALOG.md](CATALOG.md).

### Native worker cleanliness

A native worker root is not a reusable clean environment. `--oneshot` only
avoids adding the requested atom to the world file; emerge still updates the
installed-package database and root filesystem, and ebuild install hooks may
perform arbitrary additional mutations. `emerge --buildpkgonly` avoids merging,
but Portage requires every build-time dependency to be installed already, so it
is only suitable for a deliberately pre-seeded, bounded package set.

The builder supports only Native Gentoo and defaults to
`NATIVE_JOB_POLICY=single-use`. It atomically accepts one BuildJob, writes
`DATA_DIR/native-builder-tainted.json` with mode `0600`, reports
`accepting_builds=false` / `draining`, and returns HTTP 503 for later build
requests. Restarting the service does not clear the marker. Do not delete only
the marker: restore the complete VM, LVM/Btrfs snapshot, or disposable rootfs.

“Single-use” is per BuildJob, not necessarily per atom. A job may build a
reviewed package set and its dependency closure inside the same disposable
root, then publish the complete result and discard the root. This keeps batch
throughput practical without allowing state from one request, tenant, profile
or repository revision to influence the next. Split very large sets into
resource-bounded batches and create one clean root for each batch.

For a long-lived static pool, use one of these execution boundaries:

1. A static agent that creates one systemd-nspawn root from an immutable
   stage4 plus a job-private OverlayFS upper layer.
2. A PVE/local-QEMU VM clone or LVM/Btrfs snapshot per job when full native
   Gentoo behavior is required.

Only repository snapshots and verified distfiles may be shared read-only.
Namespace ccache by architecture, profile, compiler and build-policy digest.
Keep `/etc/portage`, `/var/db/pkg`, `PORTAGE_TMPDIR`, `PKGDIR`, home, logs and
the writable root job-private. Signing belongs after quarantine verification,
outside the untrusted build root.

---

## 3. Identity and projects

The server supports three rollout modes:

| `AUTH_MODE` | Accepted identity | Intended use |
| --- | --- | --- |
| `legacy` | Administrator `API_KEY` | Compatibility/trusted bring-up |
| `hybrid` | OIDC users plus administrator `API_KEY` | Migration and break-glass |
| `oidc` | OIDC only | Normal identity path |

Upstream OIDC or GitHub credentials are exchanged once for an opaque Portage
Engine session token. OIDC credentials are verified against the configured exact issuer, signature,
expiry and audience. Authorization comes only from PostgreSQL memberships;
username, email, groups and token role claims are not authorization inputs.
Project roles are `viewer`, `developer`, `maintainer` and `owner`. A developer
can submit builds; a maintainer can cancel/retry/delete them; an owner manages
members. System-administrator endpoints such as cloud settings, workers and
the image factory remain separate from project roles.

Ask the API which projects the current subject can use:

```bash
export PORTAGE_ENGINE_TOKEN=$(idp-specific-command)
./bin/portage-client whoami \
  -server=https://portage.example.org \
  -token="${PORTAGE_ENGINE_TOKEN}"
```

Select a project explicitly when the identity has more than one:

```bash
export PORTAGE_ENGINE_PROJECT=gentoo-desktop
./bin/portage-client build \
  -server=https://portage.example.org \
  -package=app-misc/hello
```

The read-only binhost tree and `GET /api/v1/binhosts` inventory remain public
because `emerge` and initial client configuration cannot present an identity.
The legacy key can still be supplied with `-api-key` or
`PORTAGE_ENGINE_API_KEY`; it is a system-administrator credential, not a
per-user token.

The Dashboard uses OIDC Authorization Code + PKCE and stores an encrypted,
HttpOnly session containing the verified ID token. Its project selector sends
`X-Project-ID` to the API. An HTTP issuer/redirect can be enabled explicitly
for an isolated trusted LAN, but both the browser token and API bearer token
are observable on that network; use HTTPS outside that boundary.

Inspect the selected project's admission limits and live PostgreSQL usage:

```bash
./bin/portage-client project-policy \
  -server=https://portage.example.org \
  -token="${PORTAGE_ENGINE_TOKEN}" \
  -project="${PORTAGE_ENGINE_PROJECT}"
```

Owners use `project-policy-set` with the current policy version to change
queued, simultaneously active, UTC-daily, vCPU, memory and disk caps or
suspend a project:

```bash
./bin/portage-client project-policy-set \
  -server=https://portage.example.org \
  -token="${PORTAGE_ENGINE_TOKEN}" \
  -project="${PORTAGE_ENGINE_PROJECT}" \
  -version=1 -max-queued=100 -max-active=4 -max-daily=500 \
  -priority-weight=100 -starvation-seconds=300 \
  -max-vcpus=32 -max-memory-mib=131072 -max-disk-gib=1000 \
  -max-artifact-bytes=34359738368 \
  -max-claimed=4 -max-provision=2 -max-build=4 \
  -max-verify=2 -max-publish=1
```

Suspension stops new submissions, retries and scheduler claims; it does not
terminate an already active build. Active attempts reserve the selected
resource class in PostgreSQL and expose enforced
`claimed/provision/build/verify/publish` phase usage and limits through the
same policy API. At a full phase checkpoint, the fenced attempt waits and
renews its lease; it is not marked failed and cannot oversubscribe the final
slot across control-plane replicas.

`priority-weight` is the project's relative scheduler share.
`starvation-seconds` promotes an eligible project to oldest-first scheduling
after the configured wait. They do not bypass hard resource, phase, capability,
or suspension policy.

---

## 4. Config bundle files

To capture a Portage configuration without submitting a build, generate an
inspection bundle:

```bash
./bin/portage-client bundle \
  -portage-dir=/etc/portage \
  -package=dev-lang/python -version=3.11 \
  -out=python-bundle.tar.gz

tar -tzf python-bundle.tar.gz   # inspect
```

The exported tarball is currently an inspection/transfer artifact. The CLI does
not yet accept that tarball as `build -config`; `-config` currently expects a
JSON `PortageConfig`. Use `build -portage-dir` to submit the live system subset,
or provide the equivalent validated JSON configuration.

Config bundles are untrusted input. Portage Engine applies a strict variable,
repository, path and size policy before a bundle can be queued or written to a
builder. See [SYSTEM_CONFIG_USAGE.md](SYSTEM_CONFIG_USAGE.md#bundle-security-policy)
for the accepted subset and current limits.
