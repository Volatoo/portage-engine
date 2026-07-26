# Portage Engine profile repository template

This directory is a seed for a separately protected `pe-profiles` Git
repository. Do not publish it from the CLI overlay and do not let build
requests modify it. Production uses a reviewed, signed commit mirrored into
the image factory and referenced by full commit ID from the server catalog.

The base profiles deliberately contain only parent relationships. Desktop
profiles add reviewed build policy in `make.defaults` and narrowly scoped
`package.use` entries (for example `x11-base/xorg-server xvfb`). Common runtime,
build-test, and desktop package membership remains in the versioned image
package-set catalog; it is not added to `@system` through a profile `packages`
file. This keeps “which packages” separate from “how this profile builds them”.

Before publishing a commit, run the repository checks in a Gentoo CI worker:

```bash
pkgcheck scan --repo .
test "$(cat profiles/repo_name)" = pe-profiles
git diff --check
```

The repository contains distinct multilib and no-multilib generations because
that ABI choice is an image/seed boundary, not a runtime toggle. A BuildPlan
must select the generation matching its source image. The desktop verifier in
each generation inherits the corresponding Portage Engine base profile and
receives desktop packages from `pe/desktop-verifier-v1`; it does not silently
depend on a mutable Gentoo desktop profile.
