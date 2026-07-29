# Security policy

Portage Engine executes untrusted package build logic and publishes software
artifacts. Treat a bypass of catalog policy, project isolation, worker fencing,
signature verification, immutable publication, or secret separation as a
security issue.

## Supported versions

Until the first stable release, only the latest commit on `main` receives
security fixes. Operators should deploy an immutable commit and follow the
upgrade notes in [CHANGELOG.md](CHANGELOG.md). There is no security support for
modified deployments that disable the documented verification gates.

## Reporting a vulnerability

Use GitHub's private vulnerability-reporting flow on the repository Security
page. Include:

- affected commit and deployment mode;
- the trust boundary crossed and required attacker access;
- a minimal reproduction without real credentials or private package data;
- whether signing keys, provider credentials, published packages, or project
  isolation may be affected; and
- any evidence that must be retained before containment.

Do not open a public issue for an unpatched vulnerability. Never include API
keys, OIDC tokens, PVE/PBS/Vault credentials, private keys, database dumps, or
unredacted logs in a report.

Maintainers will acknowledge a complete report, establish a private remediation
plan, request a CVE when appropriate, and coordinate disclosure after supported
deployments can upgrade. Exact response dates depend on severity and maintainer
availability; this project does not claim a staffed 24×7 incident response.

## Operator incident priorities

1. Stop new admission and publication without deleting evidence.
2. Revoke affected workload/OIDC/object-storage credentials.
3. Fence stale attempts and signer tasks in PostgreSQL.
4. Preserve audit logs, channel pointers, manifests, object versions, and
   database backup/WAL evidence.
5. Rotate or revoke release keys through the documented transition process;
   never silently replace the user trust root.

The public deployment boundary and required recovery exercises are documented
in [docs/PRODUCTION_BOUNDARY.md](docs/PRODUCTION_BOUNDARY.md).
