# Design decisions

Why the system is shaped the way it is, and what was rejected on the way.

The reference documents describe the contracts as they are today. They do not
say what else was considered, and that is the part nobody can reconstruct from
the code: an invariant with no recorded reason gets removed by whoever finds it
inconvenient. Each entry below states the decision, the alternative it rejected,
and where the contract it produced now lives.

These were carried over from a long design document that had accumulated as a
single 174 KB HTML file. Its conclusions had already landed in the reference
docs as the work was implemented; the reasoning had not, so the reasoning is
here and the file is gone.

## PostgreSQL is the only job authority

**Rejected:** the in-process work queue, capacity 100.

It lost every queued job on restart, two control-plane replicas could not claim
from it atomically, and it could express neither fair scheduling nor a durable
cancel. Durable rows plus `FOR UPDATE SKIP LOCKED` replaced it.

`LISTEN/NOTIFY` and Redis only wake a scheduler up. Neither holds queue state,
so losing either delays a dispatch and cannot lose or duplicate one.

See [SCHEDULER.md](SCHEDULER.md).

## Redis is an accelerator, never an authority

Redis carries presence, a token bucket and scheduler wake-ups. Every one of
those degrades to a PostgreSQL query, polling, or local conservative rate
limiting when Redis is gone, which is why a Redis failure is fail-open inside
the application rather than fail-closed.

This is the reason `DATABASE_ENABLED=true` with `DATABASE_REQUIRED=false` is
refused at startup: it is the one combination that would put job authority in
two places at once.

See [PRODUCTION_BOUNDARY.md](PRODUCTION_BOUNDARY.md) and [IAM.md](IAM.md).

## One PostgreSQL primary before any cluster

**Rejected:** introducing a multi-primary or HA cluster early, to look
production-ready.

Several control-plane replicas do not imply several databases. The first
milestone is one primary with strict backup, WAL archiving, PITR and a restore
drill that runs against an isolated instance for every migration release —
because a backup nobody has restored is not a backup.

See [PUBLIC_BETA_RECOVERY.md](PUBLIC_BETA_RECOVERY.md).

## A stale actor may never overwrite newer state

Every state transition is `UPDATE ... WHERE id = ? AND version = ? AND state IN
(...)`, and zero affected rows means the caller is stale. Renewal and completion
check the attempt, the worker, the lease state, the expiry and the fence
together, so a worker that recovers its network cannot complete or publish an
attempt that has already been reassigned.

See [SCHEDULER.md](SCHEDULER.md).

## Hard constraints never degrade

Architecture, profile and image generation, build mode, worker capability,
network zone and the verification/signing boundary must all match. A job that
finds no match is displayed as blocked or unschedulable; it is never run on
something close enough.

## The success-rate denominator is only terminal pipelines

`succeeded / (succeeded + failed)`. Canceled, superseded, policy-blocked and
still-running jobs are outside it, and infrastructure and verification failure
rates are published separately. A denominator that quietly includes cancellations
turns an operator's own cleanup into a reliability regression.

## Single-use means one bounded job, not one package

The unit is a `BuildJob` with a boundary, not an atom: one disposable root may
build a reviewed package set and its dependency closure, then be destroyed. Sets
too large for the CPU, memory, disk, runtime or artifact-count budget are split
into several jobs.

**Rejected:** native `emerge` plus `depclean` on a long-lived host. It cannot
prove what it changed and cannot roll back postinst, config protection, VDB or
preserved-libs. Acceptable for manual debugging, refused in production.

`static` therefore describes a long-lived scheduling agent — never a reused
writable Gentoo root.

## Rollback is owned from outside the build root

A builder running inside a dirty root cannot prove its own cleanliness by
deleting a marker. The marker is fail-closed admission state, not a cleanup
mechanism; the reset is performed by an agent outside the root, by PVE, or by
the storage controller.

## Portage Engine does not replace Portage

Consuming a binpkg requires no Portage Engine CLI, no overlay and no API token:
configure the binhost and the trust root once, then keep using
`emerge --getbinpkg`. The CLI adds the one capability Portage does not have —
requesting a remote build — and the builder is infrastructure that never appears
on a user's path.

**Rejected:** a hook that submits a remote build when `emerge` misses. One
ordinary `emerge` can pull a whole dependency graph, so the hook turns into
implicit spend, queue waits, configuration leaking off the machine, and build
storms. An explicit `query` / `plan` / `build` flow gives the same result and
says what it will cost.

See [USAGE.md](USAGE.md).

## HTTP is allowed on a controlled LAN only with signature enforcement

The public data plane is HTTPS. An internal deployment may serve binpkgs over
HTTP, but only with signature verification enforced — transport security and
artifact authenticity are separate properties, and the second one is the one
that decides what gets installed.

## A template carries only what a job cannot safely change

Package-level `USE`, keywords, `CFLAGS` and ordinary dependency choices belong
to an immutable `BuildSpec` and must not produce a new VM template. A template
exists for boundaries that cannot be switched inside a job, or that change the
ABI or the base runtime.

Two build paths converge on one image manifest: a Packer fast path from a
verified seed, and a Catalyst traceable path from a pinned stage, repository
snapshot and profile.

See [CATALOG.md](CATALOG.md) and the
[image factory](../image-factory/README.md).

## Offline has two modes, and they share one manifest

Mirror-backed offline has no public egress but can reach an internal mirror.
Strict air-gap cannot reach even the sync gateway and imports approved, signed
content bundles. Both use the same manifest, and neither may silently substitute
a newest-available artifact for a pinned one. Exceeding the freshness SLO still
allows development and testing; it blocks stable promotion.

## Desktop verification is a separate plane

GUI testing is not part of the builder process. Portage Engine schedules and
owns artifact state; a GUI runner owns VM lifecycle, input, screenshots and
assertions. Early scenarios restore a prepared desktop VM on PVE and drive it
with the in-tree runner; once the scenario count justifies it, openQA becomes a
separate verification service that receives a staging digest.

Every input is recorded, host operations are forbidden, snapshot restore is
limited to snapshots the test project allows, and a failure keeps the expected
and actual state, the diff, screenshots, the input sequence and the system logs.
Assertion strength is layered, with OCR reserved for text no accessibility tree
exposes.

See [DESKTOP_E2E.md](DESKTOP_E2E.md).
