# Compatibility policy

Portage Engine is pre-1.0. Compatibility is explicit and narrower than a
general semantic-versioning promise.

## Stable consumer surface

- Published binhosts use Portage `Packages` indexes and GPKG files.
- Catalog profiles own independent Gentoo-style
  `releases/<arch>/binpackages/<generation>/<target>` namespaces.
- Existing immutable generation manifests remain readable across an in-place
  control-plane upgrade when their schema is documented as supported.

## Versioned control-plane surface

- PostgreSQL migrations are forward-first. Back up and test restore before
  migrating; do not run an older binary after a schema/protocol cutover.
- Executor protocol and exact capability requirements fence old workers.
- Artifact and image manifests carry schema versions. Unknown versions fail
  closed.
- Public JSON fields may be added compatibly. Removing or changing a field,
  path, authentication requirement, or meaning requires a changelog entry and
  a migration window.

## Not stable before 1.0

Internal Go packages, database tables, trusted-LAN compatibility endpoints,
Dashboard HTML/JavaScript, configuration defaults, and image-factory evidence
layout may change. Security defaults may become stricter without a deprecation
period.

Legacy API keys, static push builders, local/NFS publication authority, HTTP
identity providers, and automatic release-key creation are compatibility
features only and are rejected by public mode.

## Desktop E2E scenario and adapter compatibility

- Scenario schema version 1 remains readable for the historical image baseline
  and digest-only application smoke contract. It does not carry image
  generation/display identity and is not the signed GUI matrix Gate.
- Scenario schema version 2 adds `image_generation`, `display_server` and
  `application_kind`; signature-required installs add `signer_fingerprint`;
  and the action vocabulary adds `launch_fixture` and `close`. Unknown fields
  and actions continue to fail closed.
- Direct PVE policy schema version 2 must repeat the exact profile, image,
  generation, X11 backend, staging manifest, signing-key path and full primary
  fingerprint. It verifies the guest's sealed BuildPlan before functional
  actions. Policy schema version 1 is retained only for existing version-1
  scenarios.
- `/v1/actions` is additive at the JSON field level: version-2 requests include
  the runtime identity on every request. An older adapter can continue to serve
  version-1 scenarios, but must be upgraded before it is selected for a
  version-2 scenario. Silently ignoring identity fields or claiming unsupported
  actions passed is incompatible.
- Desktop result JSON uses the same schema version as its scenario. Version 1
  remains readable and must omit version-2 runtime identity. Version 2 requires
  `image_generation`, `display_server` and `application_kind`; promotion rejects
  identity drift and requires signed-install and log artifacts,
  fixture/readiness steps, normal close and final stop.
- The bundled direct-PVE helper is X11-only. Native Wayland requires an adapter
  with compositor-native readiness, capture, input and close semantics;
  XWayland execution must be labeled X11 rather than native Wayland. WebView DOM
  assertions may flow through WebKit's AT-SPI tree; Electron renderer assertions
  may be implemented by an adapter, but neither changes native window cleanup
  or evidence requirements.
- Tracked matrix files contain all-zero digest/fingerprint sentinels so they can
  serve as static schema fixtures without pretending a live signed candidate
  exists. The runner refuses to execute them until reviewed copies are
  materialized with real locks.
