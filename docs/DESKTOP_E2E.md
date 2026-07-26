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

The image-only baseline does not require a staging binhost. In that mode both
`staging_binhost` and `staging_digest` are omitted from the PVE policy and the
runner verifies applications already sealed into the image. A scenario that
contains `install` must configure both fields and remains digest-bound. This
keeps the Golden Desktop gate independent of the package publication service
without weakening application-install tests.

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
accessibility trees, and logs before its independently timed final stop.

The staging binhost is a data plane and may use plain HTTP on a trusted LAN.
It is still immutable: run
`image-factory/desktop/write-staging-manifest.py <binpkg-directory>` after the
`Packages` index and all binpkgs are complete, then put the printed SHA-256 in
both the reviewed PVE policy and scenario. On every clean restore the guest
downloads `MANIFEST.json`, rejects redirects/proxies, verifies every object by
size and SHA-256, atomically switches to a local `file://` binrepo, and only
then permits `emerge --usepkgonly`. PVE remains HTTPS because it is the
credential-bearing control plane.

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
It returns `{"state":"passed|failed","message":"...","artifacts":[...]}`.
The scenario cannot contain arbitrary shell commands or operator credentials.
Supported actions cover lifecycle (`restore`, `start`, `stop`), staged install,
application launch, accessibility waits, bounded keyboard/mouse input, reviewed
needles, screenshots, accessibility export and scoped log collection. Direct
PVE mode intentionally rejects needle assertions; use a reviewed openQA/needle
adapter for that oracle.

The runner requires exactly one restore/start/stop sequence. After a failure it
skips further functional input, still runs declared screenshot/log collection,
and always attempts the final stop. Result JSON records times, states and
artifact references but does not echo typed input.

AI vision belongs after this deterministic gate. It may classify a failed
screenshot or propose a candidate needle/scenario change. It must not receive
credentials, update golden images automatically, or change a release result.
