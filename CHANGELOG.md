# Changelog

This project uses an Unreleased section until versioned community releases
begin. Entries describe operator-visible changes, not every internal refactor.

## Unreleased

### Added

- Browser-assisted `portage-client login` / `device-login` with one-time,
  PostgreSQL-serialized device authorization and a local authenticated
  Dashboard approval page.
- Default-off Distributed Build Alpha on migration 00029: exact compile-worker
  pools, atomic fenced slot leases/heartbeats, reviewed package-scoped builder
  policy, builder-to-persistence compiler telemetry, project/attempt-bound
  leases, post-collection output fencing, safe fallback/blocking, and a
  local-only-versus-distcc artifact/ABI/install/GUI comparison Gate.
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

- The operator console is a Vite/React project under `web/`, embedded into
  `portage-dashboard` and served at `/`. The console it replaces answers under
  `/legacy`, and `/ui` redirects to the new one. Building the dashboard now
  requires Node: `make build` builds `web/` first, and a dashboard binary
  compiled without that bundle answers 503 on every console route.
- Public API roles reject object deletion capability; executor deletion remains
  application-gated and must be bucket-policy scoped to quarantine.
- S3 mode treats local binpkg directories only as process-local scratch.
- Native Gentoo builders are single-use; the Docker/static writable-root build
  backend is removed.
- The distcc allowlist is reduced the same way on both sides. A versioned atom
  such as `>=dev-qt/qtwebengine-6.7.2` used to be accepted by the scheduler and
  refused by the builder, which then ran with distcc off while the control plane
  kept reserving compile slots; an entry that reduces on neither side is now
  rejected at startup instead of dropped.
- `portage_distcc_*_total` are now `portage_distcc_*_last_hour` gauges. The
  readings behind them are a one-hour rolling window, so a quiet hour made a
  counter go down and every consumer of `rate()` read a spike out of the drop.
- The compile-slot lease that expired while a build waited to be admitted no
  longer fails the build under `DISTCC_FALLBACK_POLICY=local`; it takes the
  controlled local fallback the builder already implements. A blocked policy
  still refuses, and a malformed lease still refuses under either policy.
- The desktop guest agent honours the step's declared `timeout_seconds` for
  `wait_accessible` and `close`, which were capped at 60 seconds regardless of
  the budget a scenario declared.

### Fixed

- `portage_scheduler_lease_expiries_total` no longer drops to zero when the
  scheduler status read times out. Prometheus read that as a counter reset, and
  `increase()` over the reset re-counted the whole lifetime total, firing
  `PortageEngineLeaseExpiry` with no lease having expired.
- The Monitor read-through cache answers a hit from memory. Every hit used to
  re-read the source watermark — an unindexable aggregate over every visible
  terminal job — under the same mutex, so each Prometheus scrape and each
  Monitor reader queued behind one sequential scan the cache exists to avoid.
- Job retention runs before the projection is loaded at startup. Loading first
  left the in-memory map holding exactly the rows retention then hid, and the
  consistency check read the difference as a diverged ledger: readiness answered
  503 and logged a ledger warning until the next reconciler tick.
- Scheduler lease recovery writes its expiry counters once per batch in a fixed
  order. Row-at-a-time updates in `expires_at` order let two replicas take the
  same counter rows in opposite orders, and the resulting deadlock rolled back a
  whole round of requeuing expired work.
- The Chinese console catalogue uses Chinese punctuation, and the
  builder-binary SHA-256 field is translated rather than hard-coded English.

### Security

- Device and platform bearer capabilities are persisted only as SHA-256
  digests; approval requires an existing federated platform session, and
  denial, expiry, concurrent consumption and replay fail closed.
- Package publication now verifies the exact signed generation in a throwaway
  root before CAS promotion.
- Public binpkg reads validate channel, manifest, object size, and SHA-256.
- Periodic object-index validation failures now make readiness fail closed.
- Every Dashboard response refuses framing (`frame-ancestors 'none'` and
  `X-Frame-Options: DENY`). The device approval page grants a CLI session from a
  single click, so a frameable page was a clickjacking path to an operator's
  identity.
- A step-up refusal relayed by the Dashboard names the credential that can
  satisfy it, and the console boot payload states the same for the session. A
  federated operator was previously offered the local administrator password
  prompt, which no OIDC session can pass, leaving Save, Test Connection, Start
  Test Build, Delete Job, Clean Up Failed and Revoke All unusable.
- The public edge reference denies every WebShell path the Dashboard registers,
  including `/legacy/shell/` and `/api/shell/preflight`, which the previous
  hand-written deny list left reachable.
- `/readyz` publishes a fixed reason vocabulary instead of the underlying
  error text. The probe is anonymous, and it used to write `databaseHealth.Reason`
  and `err.Error()` into the body — pgx connection failures carry the host, user
  and database name, so an unreachable database disclosed the internal topology
  to any caller. The detail now goes to the process log and to the authenticated
  `/api/v1/health`.

### Compatibility

- Generation manifest schema v1 remains readable; all new publications use
  schema v2.
- `DEPLOYMENT_MODE=public` is fail closed and rejects legacy credentials,
  insecure identity transport, combined runtime roles, and local/NFS artifact
  authority.
- **Breaking:** `DATABASE_ENABLED=true` with `DATABASE_REQUIRED=false` is now
  rejected at startup. The pair split job authority between PostgreSQL and
  process memory — whichever answered — and a replica that drifted into the
  in-memory half provisioned VMs no other replica could see. An existing
  `server.conf` carrying it will not start after this upgrade: set
  `DATABASE_REQUIRED=true` to make PostgreSQL the sole authority, which is what
  every shipped deployment already does, or `DATABASE_ENABLED=false` to run
  entirely in process memory.
- `DISTCC_PACKAGE_ALLOWLIST` entries are validated at startup. An entry that is
  not a package atom, such as a bare name with no category, now fails the start
  rather than silently disabling remote compilation for that package.
