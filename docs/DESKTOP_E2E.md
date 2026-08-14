# Desktop E2E

Desktop verification is a separate process from `portage-builder`. The first
implementation is deterministic and does not use an AI model for release
decisions.

`portage-desktop-runner` loads a strict JSON scenario and sends reviewed,
bounded actions to a console adapter. The adapter can be backed by openQA,
os-autoinst, or a dedicated PVE guest worker. HTTPS is the default. For initial
bring-up on an isolated trusted LAN, a project-owned adapter may use HTTP only
when `-allow-http-control-plane` is explicitly set. That mode sends the bearer
token in plaintext on the network, rejects redirects, and must not be exposed
outside the protected segment. The bearer token is read from an environment
variable and is never allowed in the scenario or URL.

For a small PVE installation, the recommended first deployment is direct PVE
mode. It talks to the existing PVE HTTPS API and a capability-limited helper
inside the guest through QEMU Guest Agent. This mode does **not** require a new
Desktop Runner web service or another TLS certificate:

```bash
export PORTAGE_DESKTOP_PVE_TOKEN_ID='desktop-e2e@pve!runner'
export PORTAGE_DESKTOP_PVE_TOKEN_SECRET='read-from-a-secret-store'

./bin/portage-desktop-runner \
  -scenario tests/desktop/scenarios/image-baseline.json \
  -pve-config configs/desktop-pve.json \
  -output /var/lib/portage-engine/desktop-evidence/application-smoke.json
```

The direct driver can only restore one reviewed snapshot, operate one VMID and
invoke `/usr/libexec/portage-desktop-agent`. The helper has named actions for
staging installation, `.desktop` launch, AT-SPI lookup, bounded xdotool input,
screenshots and logs; it has no arbitrary command action. Give its PVE token a
dedicated pool and only the VM audit/power/snapshot/guest-agent privileges it
needs.

The schema-version 1 image-only baseline does not require a staging binhost. In that mode both
`staging_binhost` and `staging_digest` are omitted from the PVE policy and the
runner verifies applications already sealed into the image. A scenario that
contains `install` must configure both fields and remains digest-bound. This
keeps the Golden Desktop gate independent of the package publication service
without weakening application-install tests.

Schema version 2 is the application-matrix contract. It additionally binds the
exact image generation and display server, carries those values on every
adapter request, and requires every install to name the full primary signing
fingerprint. Direct PVE policy schema version 2 repeats the profile, image,
generation, display server, staging manifest, key path and fingerprint. The
runner rejects any mismatch before VM mutation; after QGA is ready, the guest
also compares those values with `/etc/portage-engine/build-plan.json`.

`start` does not rely on a fixed sleep. After QGA becomes available, the
driver repeatedly invokes the named `desktop-ready` capability until the X11
socket, user Xauthority, user D-Bus socket, and a connected `xrandr` output are
all observable. It also requires an owner-writable desktop-user `.config`
directory plus live `xfce4-session` and `xfwm4` processes. Only then can a
scenario launch an application. The launch capability injects the reviewed
DISPLAY/Xauthority/D-Bus environment into a transient user unit and uses
`KillMode=process`: launchers such as `gtk-launch` exit after forking the real
GUI process, which must remain alive for the accessibility and screenshot
gates.

The direct guest helper is deliberately X11-specific: readiness requires the
X socket and `xrandr`, screenshots use `scrot`, and normal close uses a bounded
`xdotool` `Alt+F4` followed by disappearance of the selected AT-SPI node. A
native Wayland scenario may use schema version 2 through an HTTP/openQA adapter,
but that adapter must implement compositor-native input and capture and must
still enforce the same image identity, fixture digest, accessibility and
cleanup semantics. Do not silently route a `display_server: wayland` scenario
through XWayland and call it native Wayland coverage.

Desktop BuildPlans pin `display_model` to `qxl`; non-desktop images pin it to
`std`. The Packer manifest carries the value and output stamping reads the
actual PVE `vga` configuration back before accepting a template. This prevents
the PVE default display hardware from drifting away from the profile's exact
`VIDEO_CARDS="-* qxl"` contract.

PVE does not expose a portable Guest Agent file-read endpoint on every
supported installation. Evidence therefore uses the supported QGA exec API and
two helper capabilities: `evidence-info` returns a bounded size and SHA-256,
and `read-evidence` returns at most 192 KiB from that reviewed file. The host
reassembles at most 32 MiB, rejects truncated QGA output, verifies the complete
digest, and writes the artifact atomically. The guest accepts only regular,
non-symlink `.b64`, `.log`, and `.json` evidence below its runtime directory.

`collect_accessibility` exports a strict, bounded AT-SPI tree (at most 4096
nodes) with parent/index, role, name, and reviewed states. Application logs are
limited to the deterministic transient user unit created for that application;
desktop-session and system logs are separate scopes. On a functional failure,
the runner stops further input but still collects declared screenshots,
accessibility trees, and logs, attempts the declared normal close, and then
runs its independently timed final stop. Schema-version 2 signed installs also
save the bounded emerge output as a digest-verified evidence artifact.

The application matrix consumes a **signed candidate binrepo**, not the legacy
unsigned verification staging tree. The data plane may use plain HTTP on a
trusted LAN because every object is content-bound, while the signing key's full
primary fingerprint must arrive through a separate authenticated operator
channel. Put `Packages`, signed GPKG files and the reviewed public key (for
example `signing-key.asc`) in one immutable directory, then run
`image-factory/desktop/write-staging-manifest.py <binpkg-directory>` after the
directory is complete. Put the printed SHA-256 and independently approved
fingerprint in both the materialized PVE policy and scenario. On every clean
restore the guest downloads `MANIFEST.json`, rejects redirects/proxies,
verifies every object by size and SHA-256, checks that the manifest-bound key
contains exactly the approved primary fingerprint, creates a dedicated Portage
keyring, sets `verify-signature = true` and `binpkg-request-signature`, and only
then permits `emerge --usepkgonly`. A missing, unsigned, wrongly signed or
wrong-key GPKG fails closed. PVE remains HTTPS because it is the
credential-bearing control plane.

## GTK, Qt and WebView matrix

The first repeatable matrix deliberately uses small applications with clear
licenses and stable amd64 Gentoo keywords:

| Class | Scenario | Signed package | Fixture/assertion | License |
| --- | --- | --- | --- | --- |
| GTK | `gtk-mousepad.json` | `app-editors/mousepad` | local text file; exact AT-SPI frame name | GPL-2+ |
| Qt | `qt-featherpad.json` | `app-editors/featherpad` | same local text file; exact AT-SPI frame name | GPL-3+ |
| WebView | `webview-surf.json` | `www-client/surf` | local HTML only; WebKit-exposed AT-SPI `heading` | MIT |

Surf represents the Electron/WebView column without pulling an Electron SDK,
Chromium profile or online application. Its HTML has no remote stylesheet,
script, image or navigation dependency. An Electron-specific scenario may be
added later with the same schema; renderer DOM/ARIA may be asserted by an
adapter such as Playwright, but native window lifecycle and failure evidence
remain runner responsibilities.

Every matrix scenario executes restore, readiness, signature-required install,
digest-bound fixture launch, accessibility assertion, accessibility/screenshot
and journal evidence, normal window close with disappearance assertion, and
final VM stop. The atoms are resolved only inside the repository revision
pinned by the signed image-factory inputs; the staging manifest then locks the
exact `Packages`, GPKG and public-key bytes actually installed.

The tracked matrix files and
`configs/desktop-pve-matrix.example.json` are **static contract templates**.
Their all-zero staging digest and fingerprint are deliberate sentinels;
`portage-desktop-runner` rejects them as unmaterialized. Their planned
`desktop-verifier-matrix-g1` identity must also be replaced by the exact new
image containing the schema-version 2 guest agent and locked fixtures. They are
not evidence that PVE, the image build, or the signed package Gate has run.

To prepare a live run, first build a new desktop image through the normal
signed offline image-factory bundle. Its input lock must contain both
`fixture/desktop/*` objects, and its BuildPlan must pin the profile, image ID,
generation, repositories and package-set catalog. Then create the signed
candidate directory and materialize reviewed copies outside the source tree:

```bash
candidate_dir=/srv/portage-engine/signed-candidates/gui-matrix-g1
expected_fingerprint=REPLACE_WITH_INDEPENDENTLY_APPROVED_PRIMARY_FINGERPRINT
image_id=REPLACE_WITH_BUILT_DESKTOP_IMAGE_ID
image_generation=REPLACE_WITH_BUILT_DESKTOP_GENERATION

manifest_digest=$(python3 image-factory/desktop/write-staging-manifest.py "$candidate_dir")
actual_fingerprint=$(gpg --batch --with-colons --import-options show-only \
  --import "$candidate_dir/signing-key.asc" | awk -F: '$1 == "fpr" { print $10; exit }')
test "$actual_fingerprint" = "$expected_fingerprint"

install -d -m 0750 /var/lib/portage-engine/desktop-run-inputs
python3 - \
  tests/desktop/scenarios/gtk-mousepad.json \
  /var/lib/portage-engine/desktop-run-inputs/gtk-mousepad.json \
  "$manifest_digest" "$expected_fingerprint" "$image_id" "$image_generation" <<'PY'
import json, pathlib, sys

source, output, manifest, fingerprint, image, generation = sys.argv[1:]
document = json.loads(pathlib.Path(source).read_text())
document["image_id"] = image
document["image_generation"] = generation
for step in document["steps"]:
    if step["action"] == "install":
        step["input"]["staging_digest"] = manifest
        step["input"]["signer_fingerprint"] = fingerprint
pathlib.Path(output).write_text(json.dumps(document, indent=2) + "\n")
PY
```

Repeat the materialization for `qt-featherpad.json` and `webview-surf.json`.
Materialize a reviewed copy of the schema-version 2 PVE example with the same
four values, then replace its endpoint, CA, node, VMID, snapshot, staging URL
and evidence directory with site-approved values. The scenario and PVE policy
must match exactly; direct PVE mode fails before restore when they do not.

```bash
python3 - \
  configs/desktop-pve-matrix.example.json \
  /var/lib/portage-engine/desktop-run-inputs/desktop-pve-matrix.json \
  "$manifest_digest" "$expected_fingerprint" "$image_id" "$image_generation" <<'PY'
import json, pathlib, sys

source, output, manifest, fingerprint, image, generation = sys.argv[1:]
document = json.loads(pathlib.Path(source).read_text())
document["image_id"] = image
document["image_generation"] = generation
document["staging_digest"] = manifest
document["staging_key_fingerprint"] = fingerprint
pathlib.Path(output).write_text(json.dumps(document, indent=2) + "\n")
PY
```

With a real image, signed candidate and reviewed policy in place, run each
materialized scenario explicitly:

```bash
export PORTAGE_DESKTOP_PVE_TOKEN_ID='desktop-e2e@pve!runner'
export PORTAGE_DESKTOP_PVE_TOKEN_SECRET='read-from-a-secret-store'

./bin/portage-desktop-runner \
  -scenario /var/lib/portage-engine/desktop-run-inputs/gtk-mousepad.json \
  -pve-config /var/lib/portage-engine/desktop-run-inputs/desktop-pve-matrix.json \
  -output /var/lib/portage-engine/desktop-evidence/gtk-mousepad.result.json
```

Run the Qt and WebView files with distinct result paths. A passed result is
credible only when the install step includes its signed-install log artifact,
the accessibility/screenshot/log steps include digest-verified artifacts, the
close and stop steps pass, and the result identity matches the built image
manifest. The repository does not contain a live PVE policy or materialized
signed candidate, so the checked-in validation is limited to schema, runner,
guest-contract and fixture/static tests. An on-site 2026-08-14 linked-clone
audit of the existing VMID145 `desktop-g6` template additionally proved that it
is still schema v1, lacks the schema-v2 image/fixture capabilities and cannot
be reused as matrix evidence. The disposable audit clone was removed; see
`evidence/pve/desktop-matrix-readiness-audit-20260814.json`.

```bash
export PORTAGE_DESKTOP_DRIVER_TOKEN='read-from-a-secret-store'

./bin/portage-desktop-runner \
  -scenario tests/desktop/scenarios/application-smoke.json \
  -driver-url https://desktop-runner.internal \
  -output /var/lib/portage-engine/desktop-evidence/application-smoke.json
```

For a trusted-LAN HTTP bring-up:

```bash
./bin/portage-desktop-runner \
  -scenario tests/desktop/scenarios/application-smoke.json \
  -driver-url http://desktop-runner.internal \
  -allow-http-control-plane \
  -output /var/lib/portage-engine/desktop-evidence/application-smoke.json
```

The opt-in changes transport policy only. Endpoint userinfo, query strings,
fragments, non-origin paths, and redirects remain rejected. Move the adapter
behind HTTPS, a VPN, or another authenticated encrypted transport before it
crosses an untrusted network.

The adapter implements one authenticated endpoint:

```text
POST /v1/actions
Authorization: Bearer <token>
Content-Type: application/json
```

It receives a scenario ID, step ID, action and an action-specific input map.
Schema-version 2 requests also carry profile ID, image ID, image generation,
display server and application kind on every action; an adapter must reject
drift rather than treating those fields as advisory.
It returns `{"state":"passed|failed","message":"...","artifacts":[...]}`.
The scenario cannot contain arbitrary shell commands or operator credentials.
Supported actions cover lifecycle (`restore`, `start`, `stop`), staged install,
application launch, digest-bound local fixture launch, accessibility waits,
normal close with accessibility disappearance, bounded keyboard/mouse input,
reviewed needles, screenshots, accessibility export and scoped log collection.
Direct PVE mode intentionally rejects needle assertions; use a reviewed
openQA/needle adapter for that oracle.

The runner requires exactly one restore/start/stop sequence. After a failure it
skips further functional input, still runs declared screenshot/log collection
and cleanup actions, and always attempts the final stop. Result JSON uses the
scenario's schema version: version 1 omits the new runtime identity, while
version 2 requires image generation, display server and application kind.
Both record times, states and artifact references but do not echo typed input.

AI vision belongs after this deterministic gate. It may classify a failed
screenshot or propose a candidate needle/scenario change. It must not receive
credentials, update golden images automatically, or change a release result.
