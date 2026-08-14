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

## What it does

| Workflow | Tool | Result |
| --- | --- | --- |
| Consume an existing package | Portage `emerge` | Installs from a signed binhost |
| Request a missing package | `portage-client` | Submits and tracks a remote build |
| Operate the platform | Dashboard and service binaries | Manages workers, policy, signing, storage, and releases |

The core pipeline is:

```text
client → API/scheduler → disposable native worker → quarantine/verify
       → isolated signer → immutable binhost → emerge
```

Important boundaries:

- PostgreSQL is the durable authority for jobs, leases, admission, signing,
  identity, and capacity state.
- Builders are single-use and never receive the release private key.
- The signer is isolated and pulls digest-bound work from its own queue.
- Catalog policy fixes the repository revision, profile, image generation,
  resource class, and allowed egress for every build.
- OIDC sessions and project RBAC scope authenticated operations; anonymous
  access is limited to package, documentation, and coarse status surfaces.

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

Default local endpoints:

- API and binhost: `http://127.0.0.1:18080`
- Dashboard: `http://127.0.0.1:18081`
- Grafana: `http://127.0.0.1:23000`
- Prometheus: `http://127.0.0.1:29090`

These defaults are for local development only. Replace credentials and add the
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
Signing trust and multi-profile setup are covered in the
[usage guide](docs/USAGE.md).

Request a build with a short-lived platform session and an authorized project:

```bash
export PORTAGE_ENGINE_TOKEN='<session-token>'
export PORTAGE_ENGINE_PROJECT='<project-name-or-uuid>'

./bin/portage-client build \
  -server=https://portage.example.org \
  -package=dev-lang/python -version=3.11 \
  -profile-id=pe/amd64/glibc/systemd/base-v1 \
  -resource-class=medium -wait
```

The legacy API key is a migration and break-glass administrator path, not a CI
identity. Never send API keys or bearer tokens over untrusted HTTP.

## Components

- `portage-server` — API, scheduler, policy, IaC, and publication control.
- `portage-builder` — native Gentoo build and install verification.
- `portage-signer` — isolated digest-bound signing worker.
- `portage-dashboard` — operator console and public read-only pages.
- `portage-client` — consumer setup and developer build requests.
- `portage-capacity-actuator` — fenced capacity operations outside scheduler transactions.
- `image-factory/` — offline Packer/Catalyst image production and desktop gates.

Distributed Build Alpha is optional and disabled by default. Its security
boundary and remaining live PVE/distccd validation are documented in
[Distributed Build Alpha](docs/DISTRIBUTED_BUILD_ALPHA.md).

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
- [Identity and project authorization](docs/IAM.md)
- [Production boundary](docs/PRODUCTION_BOUNDARY.md)
- [Object storage contract](docs/OBJECT_STORAGE.md)
- [Recovery Gate](docs/PUBLIC_BETA_RECOVERY.md)
- [Release process](release/README.md)
- [Public Beta and GA plan](docs/NEXT_STEPS.md)
- [Enterprise capability gaps](docs/ENTERPRISE_GAPS.md)

Report vulnerabilities through the process in [SECURITY.md](SECURITY.md).
Never commit API tokens, infrastructure credentials, private signing keys, or
production logs containing secrets.

## License

[MIT](LICENSE)
