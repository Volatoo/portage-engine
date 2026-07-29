# Governance

Portage Engine is maintained in public with a maintainer-led, review-gated
model. Security boundaries and artifact formats favor explicit compatibility
over implicit consensus or silent fallback.

## Decisions

- Small fixes use an issue or pull request with tests and an operator-facing
  changelog entry when behavior changes.
- Changes to trust boundaries, persistent schemas, public APIs, catalog
  contracts, artifact formats, identity semantics, or release keys require a
  design note in the pull request. The note must describe migration, rollback,
  negative tests, and operational evidence.
- A maintainer who authored a security-sensitive change should seek another
  maintainer's review when one is available. Until the maintainer group grows,
  production operators must perform an independent deployment review.
- Database and artifact formats are forward-first. Old binaries must fail
  closed when they cannot enforce the current protocol.

## Roles

- Contributors propose and test changes.
- Reviewers provide repeat, technically substantive reviews but do not have
  merge or release authority by default.
- Maintainers triage, merge, cut releases, manage compatibility policy, and
  coordinate security work.

Maintainer membership is earned through sustained reviewed contributions and
reliable stewardship. Adding or removing a maintainer is recorded in a public
pull request updating [MAINTAINERS.md](MAINTAINERS.md), except when private
security handling temporarily requires limited disclosure.

## Releases

A release must identify an immutable commit, pass the repository quality gates,
produce checksums and provenance/SBOM evidence, and document schema or operator
actions. Signing, image promotion, and binpkg release keys are separate trust
domains; repository merge authority does not automatically grant access to
production signing keys.

The project currently has no legal entity, paid support promise, or guaranteed
release cadence.
