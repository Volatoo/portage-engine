# Changelog

This project uses an Unreleased section until versioned community releases
begin. Entries describe operator-visible changes, not every internal refactor.

## Unreleased

### Added

- Browser-assisted `portage-client login` / `device-login` with one-time,
  PostgreSQL-serialized device authorization and a local authenticated
  Dashboard approval page.
- S3-compatible object quarantine, revocable verification capabilities,
  independent signer hand-off, immutable binhost generations, ETag-fenced
  stable channel pointers, deep audit, replication, and reference-aware GC.
- Anonymous package search, documentation, coarse service status, binhost
  inventory, and public signing-key retrieval.
- `portage-client setup` with an independently supplied full primary
  fingerprint and exact profile binrepo configuration.
- Generation manifest schema v2 with redacted build-input, repository, image,
  mirror-bundle, package-set, and egress-policy provenance.
- Real executor CPU, memory, disk pressure and warm cache-key telemetry.
- A separated public deployment Compose reference and production-boundary
  documentation.

### Changed

- Public API roles reject object deletion capability; executor deletion remains
  application-gated and must be bucket-policy scoped to quarantine.
- S3 mode treats local binpkg directories only as process-local scratch.
- Native Gentoo builders are single-use; the Docker/static writable-root build
  backend is removed.

### Security

- Device and platform bearer capabilities are persisted only as SHA-256
  digests; approval requires an existing federated platform session, and
  denial, expiry, concurrent consumption and replay fail closed.
- Package publication now verifies the exact signed generation in a throwaway
  root before CAS promotion.
- Public binpkg reads validate channel, manifest, object size, and SHA-256.
- Periodic object-index validation failures now make readiness fail closed.

### Compatibility

- Generation manifest schema v1 remains readable; all new publications use
  schema v2.
- `DEPLOYMENT_MODE=public` is fail closed and rejects legacy credentials,
  insecure identity transport, combined runtime roles, and local/NFS artifact
  authority.
