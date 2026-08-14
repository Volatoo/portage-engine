# Distributed Build Alpha

Distributed Build Alpha is an optional compile-only acceleration milestone. It
does not block Public Beta and is disabled by default. Migration 00029 owns the
repository-side inventory and leases; 00027 and 00028 are reserved by the CLI
device-flow and monitoring milestones and 00030 by CLI session derivation.
Migration 00031 adds the worker-pruning lookup required by the shared scheduler,
so final integration lands on pinned PostgreSQL schema v31.

## Scheduling boundary

Project admission, quotas, weighted virtual runtime and anti-starvation run
first. Only a job/phase already claimed for a project may request a compile
slot. The distcc pool is therefore a hard compatibility/routing layer, not a
second fairness queue and not a way to move capacity between project budgets.

One exact pool contains architecture, CHOST, compiler/version digest, toolchain
image generation, the exact normalized CPU-feature set, isolated network zone,
and project trust domain. Alpha defaults the trust domain to the durable project
ID.

`ReserveCompileSlots` locks one compatible `compile_workers` row with
`FOR UPDATE SKIP LOCKED`, counts all unexpired active slot leases while holding
that row lock, advances a monotonic worker fence and inserts the reservation in
the same transaction. Concurrent replicas cannot oversell `max_slots`. A stale
builder identity/fence cannot renew, authorize observations, validate output or
release a newer lease. The reservation is joined to the active project job,
attempt fence and normal scheduler worker lease; a caller cannot manufacture a
project/job/attempt tuple just because a compatible compile worker exists.

Worker inventory and builder leases have separate heartbeats. An expired lease
is not revived. A stale/draining compile worker cannot renew builder access.
The manager rechecks the durable lease immediately before collecting remote
build output and again after the bytes have been collected, immediately before
the quarantine/staging commit. It checks once more after object-quarantine
persistence. Any rejection deletes the job-private partial generation and
stops verification and publication.

## Builder execution policy

Both server and disposable builder must opt in. The server requests a slot only
for `DISTCC_PACKAGE_ALLOWLIST`; the builder independently checks the same
reviewed list, its exact local toolchain dimensions and the operator's isolated
network CIDRs. The generated Portage `package.env` disables Portage's broad
distcc feature and selects job-private compiler wrappers only for reviewed
atoms:

```text
FEATURES="-distcc -distcc-pump"
CC="<job-root>/distcc-wrappers/cc"
CXX="<job-root>/distcc-wrappers/cxx"
DISTCC_HOSTS="<isolated-ip>:3632/<leased-slots>"
DISTCC_FALLBACK="0"
```

Request-supplied global `distcc` and `distcc-pump` features are stripped.
Packages absent from `package.env` stay local. The reviewed list must exclude
small packages and Rust, Go and Java packages. A wrapper invokes distcc only
when `EBUILD_PHASE=compile` and the compiler arguments contain the object-only
`-c` flag. Configure probes, preprocessing/assembly, link, install and test
invocations execute the exact CHOST-qualified compiler locally.

`DISTCC_FALLBACK_POLICY=local` omits the remote environment when reservation or
preflight fails. For an admitted invocation, distcc's implicit fallback is
disabled; the wrapper makes the reviewed, observable local retry after a remote
failure. `blocked` makes the build fail. Neither policy advances
partial output: failed `emerge` never reaches artifact collection, and a stale
output fence rejects a nominally successful remote result both before
collection and at the post-collection commit boundary.

Pump mode is always rejected. There is no configuration switch that enables it.

## Compile-worker security boundary

A compile worker contains the compiler/toolchain and distccd only. It must not
receive or mount a Portage repository or repository sync credential; object
publication/delete authority; release/signing private keys; PVE/PBS/Terraform
credentials; or a general control-plane API/admin credential.

`distccd` listens only on the isolated build network. The lease contract binds
worker ID, builder ID, builder network identity, project trust domain, durable
attempt ID/fence, pool ID, slots, expiry and a monotonic compile fence. The
manager independently derives the pool trust domain from the claimed project;
the disposable builder independently compares that project and attempt to the
lease before installing wrappers. The deployment's mTLS/L4 authorization
sidecar must query the same tuple before allowing a compiler connection and
remove access on lease or worker-heartbeat expiry. A raw distccd listener
reachable from another network, or an ACL based only on subnet membership,
does not pass this Alpha.

The repository deliberately does not claim that this network enforcement has
run without the real worker image, network policy and identity provider.

## Metrics

The manager records a real exact-pool reservation hit and queue time. Each
compiler wrapper writes a job-private event, uses its per-invocation distcc log
to sum sent/received payload bytes, and returns an aggregated report with the
builder result. The manager calls `RecordCompileObservation` for each bounded
group through the still-active lease/attempt fence. PostgreSQL accepts only the
outcomes `local`, `remote`, `hit`, `fallback` and reviewed failure reasons.
Prometheus exports only global low-cardinality totals plus the fixed
failure-reason label set; worker, pool, project, job, package, source path and
endpoint never become labels. Protocol framing overhead is not included in the
payload-byte counter.

## Local-only versus distcc Gate

The repository entry point compares evidence from two real runs:

```bash
make distcc-gate \
  LOCAL_EVIDENCE=/evidence/local-only.json \
  DISTCC_EVIDENCE=/evidence/distcc.json \
  GATE_RECEIPT=/evidence/distcc-comparison-receipt.json
```

Each schema-v1 manifest supplies the same immutable build-input digest,
artifact digest, ABI digest, passed install-evidence digest and at least one
passed GUI scenario/evidence digest. Distcc evidence additionally names its
pool, proves pump was disabled and records that output was fenced. The command
fails on any mismatch and writes a receipt atomically only after every check
passes. It compares evidence; it never fabricates builds, installs, GUI runs or
PVE/distccd results.

## Configuration

See `configs/server.conf` and `configs/builder.conf`. Alpha remains off unless
all exact toolchain fields, the reviewed package allowlist and at least one
isolated network CIDR are configured. Server enablement also requires
PostgreSQL required mode.

## Inputs still required for an on-site Alpha

Repository tests cover pool identity, policy, fencing, builder configuration,
comparison logic and (when `PORTAGE_TEST_DATABASE_URL` is supplied) concurrent
PostgreSQL reservation. A real Alpha still requires:

1. A reviewed compile-worker image and immutable toolchain/compiler digests.
2. At least one isolated build-network CIDR and an mTLS/L4 lease-enforcement
   sidecar that can bind the actual builder network identity.
3. Compile-worker leaf issuance/rotation and trusted inventory registration;
   no general API or infrastructure credential may be used.
4. The exact CHOST, CPU feature set, network zone and project trust-domain
   policy for the selected builders/workers.
5. A reviewed non-small C/C++ package allowlist and expected parallel slots.
6. Real local-only and distcc artifact/ABI/install/GUI evidence for the same
   immutable input.
7. Two concurrent jobs plus worker-disconnect runs under both `local` and
   `blocked` policy, with database, network-policy, log and metric evidence.

Until those inputs and results exist, the repository tests are complete but the
real distccd/PVE, two-concurrent-job and disconnect Gate status is **not-run**.
