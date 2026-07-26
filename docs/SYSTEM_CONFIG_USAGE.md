# Using System Portage Configuration

When you request a build, you can hand the server the build-relevant, policy-
approved subset of your Portage configuration instead of re-specifying USE
flags and settings by hand. Pass `-portage-dir` to the client and it reads your
`/etc/portage` directly, then validates the generated bundle before submission.

```bash
./bin/portage-client build \
  -server=https://portage.example.org \
  -portage-dir=/etc/portage \
  -package=dev-lang/python -version=3.11 -wait
```

The build then compiles the package with the accepted USE flags, keywords,
masks, and make.conf settings. Operator-controlled settings and shell syntax are
rejected rather than copied into a privileged remote build environment.

## What gets read

`-portage-dir` reads the following from the given directory (default
`/etc/portage`). Each file is optional; missing files are skipped.

| Source | Purpose |
| --- | --- |
| `make.conf` | Allowlisted build settings such as USE, CFLAGS, MAKEOPTS and language targets. Falls back to `/etc/make.conf`. |
| `package.use` (file or directory) | Per-package USE flags |
| `package.accept_keywords` (file or directory) | Per-package keywords (e.g. `~amd64`) |
| `package.mask` (file or directory) | Masked packages/versions |
| `package.unmask` (file or directory) | Unmasked packages/versions |
| `repos.conf` | Repository / overlay definitions |

Both the single-file and the split-directory (`package.use/`) layouts are
supported. `repos.conf` may also be a file or a directory containing `.conf`
files.

> Note: accepted settings from your `make.conf` are **appended** to the build
> environment's own `make.conf`. Infrastructure-owned variables such as
> `FEATURES`, `ROOT`, `PORTAGE_CONFIGROOT`, `PKGDIR`, signing commands and hooks
> cannot be supplied by a user bundle.

## Bundle security policy

Every bundle is validated when it is created, imported, accepted by the server,
accepted by a remote builder, written to disk, and executed. The current policy
includes:

- at most 128 package build specifications and a 2 MiB HTTP request body;
- strict atom, version, USE flag, keyword, profile and metadata syntax;
- allowlisted make.conf/environment variables and literal values only;
- `MAKEOPTS` limited to jobs/load values from 1 through 64;
- at most 32 repositories, with safe repository names and locations fixed to
  `/var/db/repos/<name>`;
- repository sync restricted to supported `git`, `webrsync` and `rsync`
  transports; credentials, fragments, query strings and local `file://` URLs
  are rejected;
- on a catalog-enabled server, captured repository names are treated as
  registry IDs; their client URLs are discarded and replaced by the approved
  server URL and immutable revision. Unregistered overlays are rejected;
- duplicate package specs/repositories and unknown JSON fields are rejected.

Raw shell expressions such as `CFLAGS="${COMMON_FLAGS}"` are not replayed by a
bundle. The current reader is deliberately line-oriented: it does not execute
shell, expand variables, follow sourced files, or calculate the effective
configuration through Portage's Python API. Use literal resolved values in an
explicit config JSON for now. A future `pe build plan` command will use
Portage's effective configuration rather than parsing shell expressions itself.

## Generating a bundle instead of building

To capture your system configuration into a portable file for review or
archival without starting a build, use the `bundle` subcommand:

```bash
./bin/portage-client bundle \
  -portage-dir=/etc/portage \
  -package=dev-lang/python -version=3.11 \
  -out=python-system-config.tar.gz

# Inspect what was captured
tar -tzf python-system-config.tar.gz
```

The bundle contains the collected `/etc/portage` fragments plus the package
specification. It is not currently accepted by `build -config`; that flag reads
a JSON `PortageConfig`, while `build -portage-dir` reads and submits the live
system subset.

## Why use it

- **USE-flag consistency** — the built binary carries the requested accepted flags, so
  `emerge --getbinpkg` will accept it instead of rebuilding from source.
- **No manual re-specification** — no need to copy USE flags into `-use`.
- **Keywords and masks respected** — testing keywords and masks are honored.

See [USAGE.md](USAGE.md) for the full consume-vs-request overview.
See [CATALOG.md](CATALOG.md) for registering an internal or community overlay.
