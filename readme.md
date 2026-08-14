# Portage Engine

[![CI](https://github.com/Volatoo/portage-engine/actions/workflows/ci.yml/badge.svg)](https://github.com/Volatoo/portage-engine/actions/workflows/ci.yml)
[![Security Scan](https://github.com/Volatoo/portage-engine/actions/workflows/security-scan.yml/badge.svg)](https://github.com/Volatoo/portage-engine/actions/workflows/security-scan.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Portage Engine is a self-hosted Gentoo binary-package build and publishing
system. It runs native `emerge` builds on isolated workers, verifies and signs
GPKG artifacts, and publishes a standard binhost that regular Portage clients
can consume.

> **Status: trusted alpha.** Proxmox VE (PVE), Terraform, and native Gentoo are
> the reference path. Repository CI and security gates are active, but the live
> Public Beta and GA infrastructure gates are not complete. Do not expose the
> trusted development stack directly to the internet. See the
> [production boundary](docs/PRODUCTION_BOUNDARY.md) and
> [remaining work](docs/NEXT_STEPS.md).

The core pipeline is:

```text
client → API/scheduler → disposable native worker → quarantine/verify
       → isolated signer → immutable binhost → emerge
```

Security boundaries:

- PostgreSQL is the durable authority for jobs, leases, identity, signing, and capacity.
- Builders are single-use; the isolated signer alone handles release keys.
- Catalog policy pins repositories, profiles, images, resources, and egress.
- OIDC and project RBAC scope writes; anonymous access is read-only and limited.

## Quick start

Requirements: Go 1.26.6, Node.js 22.22.2 or newer, and Docker Compose for the
local topology. `make build` installs the locked frontend dependencies and
embeds the console in the dashboard binary.

```bash
git clone https://github.com/Volatoo/portage-engine.git
cd portage-engine

make build
make test
```

Start the loopback-only development stack:

```bash
cp .env.compose.example .env.compose
scripts/check-compose-ports.sh .env.compose
docker compose --env-file .env.compose run --rm portage-migrate
docker compose --env-file .env.compose up -d
scripts/verify-compose.sh .env.compose
```

The Compose defaults bind only to loopback. Replace credentials and add the
documented HTTPS edge before allowing access from another network.

## Use the binhost

Configure one catalog profile, then let native Portage install the package:

```bash
sudo ./bin/portage-client configure \
  -server=https://portage.example.org \
  -profile-id=pe/amd64/glibc/systemd/base-v1

emerge --getbinpkg app-editors/vim
```

Use `--getbinpkgonly` when falling back to a local source build is not allowed.
Signing, multi-profile setup, build requests, and short-lived sessions are in
the [usage guide](docs/USAGE.md). The legacy API key is break-glass only; never
send an API key or bearer token over untrusted HTTP.

## Development checks

```bash
make test          # Go, release-contract, and recovery tests
make web           # console build, lint, and tests
make lint
make lint-security
make test-release
```

GitHub Actions additionally runs race-enabled integration tests, PostgreSQL
operational gates, CodeQL, Trivy, govulncheck, npm audit, cross-platform builds,
and runtime SBOM/provenance generation.

## Documentation

- [Documentation map](docs/README.md)
- [Usage guide](docs/USAGE.md)
- [PVE reference deployment](docs/PVE_TESTING.md)
- [Production boundary](docs/PRODUCTION_BOUNDARY.md)
- [Public Beta and GA plan](docs/NEXT_STEPS.md)
- [Release process](release/README.md)

Report vulnerabilities through the process in [SECURITY.md](SECURITY.md).
Never commit API tokens, infrastructure credentials, private signing keys, or
production logs containing secrets.

## License

[MIT](LICENSE)
