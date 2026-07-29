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
