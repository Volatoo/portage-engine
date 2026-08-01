# Community release trust and operations

This directory defines the OCI community release contract. A release is
authoritative only when a reviewer has approved an immutable
`ghcr.io/<owner>/<repository>/releases@sha256:<digest>` and its canonical
`release-manifest.json` passes the Sigstore and local contract checks. Tags such
as `stable` are discovery hints; they are never deployment authority.

No checked-in workflow uses a long-lived signing key. Release jobs use the
short-lived `GITHUB_TOKEN` for GHCR and GitHub OIDC for keyless Cosign
certificates and transparency-log entries.

## Audited build roles

`release-config.json` is the machine-readable inventory. Every public runtime
target is built for `linux/amd64` and `linux/arm64` into its own repository:

| Role | Docker target | GHCR suffix | Principal executable | Boundary |
| --- | --- | --- | --- | --- |
| actuator | `actuator-runtime` | `/actuator` | `portage-capacity-actuator` | Terraform capacity mutation |
| api | `api-runtime` | `/api` | `portage-server` | Internet-facing API, no Terraform/SSH/GPG tools |
| artifact-lifecycle | `artifact-lifecycle-runtime` | `/artifact-lifecycle` | `portage-artifact-lifecycle` | One-shot artifact retention command |
| dashboard | `dashboard-runtime` | `/dashboard` | `portage-dashboard` | Read-only/operator UI |
| executor | `executor-runtime` | `/executor` | `portage-server` | Phase execution with Terraform/SSH/GPG tools |
| migrate | `migrate-runtime` | `/migrate` | `portage-migrate` | One-shot database migration |
| signer | `signer-runtime` | `/signer` | `portage-signer` | Isolated package signing worker |

The `trusted-runtime` target is deliberately excluded: it is the root,
multi-command trusted-LAN/development image and crosses the role boundaries
above. `runtime-base`, `go-build`, and `terraform` are build stages, not
publishable roles. Contract tests compare the Dockerfile's production targets
with `release-config.json`, so adding or removing a target without an explicit
release decision fails CI.

The workflow and Makefile audit produced these changes:

- ordinary CI and PR workflows declare `contents: read`; no PR-triggered file
  contains `packages: write`, `id-token: write`, or a `secrets:` mapping;
- the floating Trivy `master` reference and every other external Action tag
  were replaced with a full 40-character commit SHA and a reviewed version
  comment;
- the duplicate, permissive CodeQL job in `security-scan.yml` was removed;
  `codeql.yml` remains the single scanner allowed to write code-scanning
  results;
- `make test-release` validates the manifest schema, Docker target coverage,
  Action pins, PR permissions, digest binding, promotion CAS, rollback inputs,
  and negative paths; `make test` runs it after the Go suite;
- the pre-existing CI SBOM/provenance job remains read-only and non-publishing.

The image-factory control plane remains independent. Its Ed25519 sync/release
keys protect offline PVE image catalogs. OCI release workflows do not read or
replace those keys. They reuse the same operational invariants: immutable
generations, a canonical signed receipt, complete digest binding, explicit
previous-state compare-and-swap, and rollback only to a previously signed
generation. An image-factory stable alias does not authorize an OCI release,
and an OCI `stable` tag does not authorize a PVE image.

## Registry and naming contract

GHCR is the default and only schema-v1 registry:

```text
ghcr.io/<lowercase-owner>/<lowercase-repository>/<role>@sha256:<image-index>
ghcr.io/<lowercase-owner>/<lowercase-repository>/releases@sha256:<OCI-manifest>
```

Role repositories contain a multi-platform OCI index. The canonical manifest
records the index digest, the exact `repository@sha256:...` reference, both
platforms, and downloadable SBOM/provenance file digests. It rejects tag-only
image references.

Candidate and release-ID tags are unique or write-once by workflow policy.
`stable` is necessarily mutable because OCI Distribution does not define a
portable atomic tag compare-and-swap operation. Promotion and rollback are
serialized with the `release-stable` concurrency group, resolve the current
alias before work, compare its signed canonical digest with the operator input,
resolve it again immediately before cutover, and read the new value back. This
detects stale input and normal workflow races, but an out-of-band registry
administrator can still race a tag write. Therefore the signed manifest digest,
not the alias, is the authority recorded in deployment configuration and change
review. If any check fails, the job stops; convenience tags may require repair,
but the previously approved digest remains authoritative.

Do not enable GHCR retention rules that delete unreferenced release manifests,
Cosign signature objects, or BuildKit attestation referrers. Community packages
must be made public only after a dry-run review confirms the intended files and
permissions.

## Candidate creation and trust gate

`release-candidate.yml` is the only workflow that builds and pushes images.
It accepts:

1. an annotated SemVer tag (`v1.2.3` or a SemVer-style prerelease) whose peeled
   commit is reachable from the default branch; or
2. `workflow_dispatch` on the default branch with a full 40-character source
   commit, a release ID, and the explicit `publish-candidate` confirmation.

Configure both controls before enabling the workflow:

- a repository ruleset restricting `v*` tag creation/deletion to release
  maintainers; and
- `release-candidate` and `release-stable` GitHub Environments with required
  reviewers, no untrusted deployment branches, and prevention of self-review
  where the plan supports it.

The tag/ruleset is the source gate; the Environment is the human authorization
gate. A repository without those settings is not approved for publication even
though the YAML also fails closed on malformed or unreachable inputs.

The candidate workflow then:

- cross-builds every configured command for Linux/macOS and amd64/arm64 and
  writes sorted `SHA256SUMS`;
- builds every role as one amd64/arm64 OCI index with maximum BuildKit
  provenance and a digest-pinned SPDX generator;
- extracts independently downloadable SPDX and SLSA JSON from the registry
  attestations and writes `EVIDENCE.SHA256SUMS`;
- keylessly signs every image digest, `SHA256SUMS`, and the canonical candidate
  manifest, immediately verifying the expected GitHub workflow certificate;
- uploads the complete bundle as both a 90-day GitHub Actions artifact and an
  OCI artifact, then signs the OCI artifact digest.

The Actions artifact is a convenience copy. Its numeric run ID or name is not
authority. Record the OCI digest printed in the job summary.

## Canonical manifest

`manifest.schema.json` is the portable JSON Schema. The stricter standard-
library validator in `scripts/release_manifest.py` additionally enforces
repository-specific invariants that JSON Schema cannot express conveniently:

- exact, sorted role/target and binary/platform inventories;
- `repository@sha256` binding and GHCR namespace derivation;
- regular confined evidence paths with no symlink or `..` escape;
- actual file hashes, checksum files sorted by their path field, and equality
  between the checksum files and manifest inventories;
- compact, UTF-8, sorted-key canonical serialization;
- candidate/promotion/rollback transition shape and explicit CAS digests.

The manifest deliberately does not contain its own digest. Its SHA-256 is
computed over the exact canonical bytes and is signed as a blob. The enclosing
OCI artifact is independently signed by digest, binding the downloadable file
set.

## Promotion

Run `Promote release candidate` only after review of the candidate OCI digest,
manifest diff, vulnerability results, SBOMs, provenance, and smoke evidence.
Supply:

- `candidate_manifest_ref`: the same-repository candidate digest reference;
- `expected_previous_manifest_digest`: the canonical SHA-256 recorded for the
  currently approved stable manifest, or `none` only for the first release;
- `promote-to-stable` confirmation.

The workflow verifies the candidate OCI and blob signatures, checksum
signature, every role-image signature, all local digest bindings, and the
current signed stable state. It creates a new canonical stable manifest whose
transition binds the candidate file digest, candidate OCI digest, and expected
previous canonical plus OCI digests. Only after signing and publishing that
immutable manifest does it update role convenience tags and finally the
release `stable` alias.

## Rollback

Rollback never accepts a version tag as its target. Run `Roll back stable
release` with:

- a digest-only previously signed stable or rollback manifest reference;
- the canonical digest of the currently approved stable manifest;
- a printable 1-512 character incident/change reason; and
- `rollback-stable` confirmation.

The workflow verifies both current and target OCI/manifest signatures, the
target checksum and image signatures, and the expected-current CAS value. It
creates a new signed rollback revision binding the target manifest digest,
target OCI digest, replaced stable canonical and OCI digests, reason, and time.
This new revision is the authority; it preserves the complete audit edge
instead of silently moving `stable` back to an old tag.

If the desired target bundle or any signature/referrer has been deleted, stop
and recover the retained immutable artifact under incident review. Do not
reconstruct authority from a mutable tag or re-sign an unverified local copy.

## Independent verification

Install fixed, trusted Cosign and ORAS versions and check out a trusted copy of
this repository, then run:

```bash
scripts/verify-release.sh \
  slchris/portage-engine \
  stable \
  ghcr.io/slchris/portage-engine/releases@sha256:<digest>
```

The verifier checks the expected GitHub OIDC issuer and workflow identities,
the OCI artifact signature, canonical-manifest signature, binary-checksum
signature, every image signature, schema, inventories, and every downloaded
file digest. Set `PORTAGE_RELEASE_DEFAULT_BRANCH` only if the repository has a
reviewed non-`main` default branch.

Consumers must deploy role images from the printed digest references. A
separate policy engine may verify the BuildKit in-toto attestations directly;
the standalone JSON is for review and offline retention.

## Base image and security update policy

Production Dockerfile inputs (`golang`, `debian`, and `terraform`) and the
Gentoo CI image are multi-platform digest-pinned. The release builder also pins
the BuildKit daemon, QEMU/binfmt image, SPDX generator, Cosign release, ORAS
release, and Action source commits. Dependabot opens weekly Docker and GitHub
Actions updates.

Each base/tool update is reviewed as a supply-chain change:

1. verify that the new digest is published by the expected upstream and read
   its security/release notes;
2. rebuild every role and run Go, release-contract, image, and vulnerability
   tests;
3. compare role-specific SBOMs and provenance materials, paying special
   attention to added packages and execution tools;
4. publish a candidate and promote it through the normal signed path; never
   retag an old digest as a substitute for rebuilding; and
5. retain the superseded signed manifest for rollback, subject to vulnerability
   policy. A rollback to a known-exploitable base needs explicit incident risk
   acceptance and compensating controls.

Debian `apt-get` installs are resolved inside the digest-pinned base at build
time. This intentionally receives current Bookworm security packages rather
than using a frozen Debian snapshot, so the base digest alone is not a complete
reproducibility claim. Maximum BuildKit provenance, the final image index
digest, and the downloaded SBOM record the actual result. Rebuild candidates
regularly even when the parent digest has not changed, and immediately when a
relevant advisory requires it.

GitHub-hosted runner VM images remain mutable behind GitHub's runner label; the
workflow cannot address them by OCI digest. This is the remaining platform
constraint. Risk is minimized by full-SHA Action pins, fixed downloaded tool
versions/images, BuildKit provenance, keyless workload identity, isolated
release Environments, and treating only signed artifact digests as authority.

No workflow in this change was executed against GHCR, and this repository
contains no private signing material.
