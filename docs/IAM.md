# Identity and project authorization

Portage Engine schema v23 separates authentication from authorization, makes
project admission policy durable, and enforces federated session revocation across
control-plane replicas:

- configured OIDC and GitHub providers prove an exact `(issuer, subject)` identity;
- upstream credentials are exchanged once for a random Portage Engine session;
- PostgreSQL `project_memberships` grants that subject a project role;
- every durable build records both `project_id` and
  `requested_by_subject_id`;
- project selection is explicit through `X-Project-ID` when an identity has
  more than one membership.

Mutable token claims such as username, email, groups and roles never grant
access. A verified email may be retained only as display metadata.

## Roles

| Role | Project capability |
| --- | --- |
| `viewer` | Read project builds, logs, overview and live events |
| `developer` | Viewer plus submit builds |
| `maintainer` | Developer plus cancel, retry and delete project jobs |
| `owner` | Maintainer plus member management and project admission policy |

`system-admin` is not a project role. It is reserved for exact subjects listed
in `OIDC_ADMIN_SUBJECTS` and, during migration, the legacy API key. It protects
cloud settings, builders/workers, scheduler internals, image factory, runtime
metadata, WebShell and global cleanup operations.

## Rollout modes

```ini
# Compatibility only
AUTH_MODE=legacy
API_KEY=<administrator-secret>
STEP_UP_API_KEY=<independent-administrator-secret>

# Recommended migration mode
AUTH_MODE=hybrid
API_KEY=<break-glass-administrator-secret>
OIDC_ISSUER_URL=https://idp.example.org/realms/portage
OIDC_AUDIENCE=portage-engine
OIDC_ADMIN_SUBJECTS=<exact-bootstrap-subject>
OIDC_SESSION_IDLE_MINUTES=60
OIDC_SESSION_MAX_MINUTES=720
OIDC_STEP_UP_MAX_AGE_MINUTES=10

# OIDC-only after automation and the WebUI no longer need the legacy key
AUTH_MODE=oidc
OIDC_ISSUER_URL=https://idp.example.org/realms/portage
OIDC_AUDIENCE=portage-engine
OIDC_ADMIN_SUBJECTS=<exact-bootstrap-subject>
OIDC_SESSION_IDLE_MINUTES=60
OIDC_SESSION_MAX_MINUTES=720
OIDC_STEP_UP_MAX_AGE_MINUTES=10
```

The issuer must expose standard OIDC discovery and JWKS documents. Portage
Engine verifies issuer, signature, expiry and audience. Subject IDs in
`OIDC_ADMIN_SUBJECTS` are exact `sub` values, not usernames.

The server requires PostgreSQL in required mode for `hybrid` and `oidc`.
Start with `hybrid`, prove OIDC login and project isolation, then remove
legacy-key dependencies before switching to `oidc`.

## Dashboard federated login

Community deployments should use the multi-provider registry described in
[IDENTITY_PROVIDERS.md](IDENTITY_PROVIDERS.md). Authentik, Google and GitHub
are peer login choices; the single OIDC variables below remain a compatibility
path for existing deployments.

Register this callback with the identity provider:

```text
https://portage.example.org/auth/oidc/callback
```

Configure:

```ini
AUTH_ENABLED=true
JWT_SECRET=<at-least-32-random-characters>
OIDC_ENABLED=true
OIDC_ISSUER_URL=https://idp.example.org/realms/portage
OIDC_CLIENT_ID=portage-engine
OIDC_CLIENT_SECRET=<empty-for-a-public-client-or-provider-secret>
OIDC_REDIRECT_URL=https://portage.example.org/auth/oidc/callback
COOKIE_SECURE=true
```

The Dashboard uses Authorization Code + PKCE S256 and OIDC nonce validation.
It exchanges the upstream credential once at the control plane, encrypts only
the returned `pe1_` session in an HttpOnly, SameSite=Lax cookie, and forwards
that opaque platform session on backend requests. The selected project is
forwarded separately in `X-Project-ID`.

`COOKIE_SECURE=true` is required for an HTTPS OIDC callback, including the
common case where a reverse proxy terminates TLS and forwards HTTP to the
Dashboard process.

Local `ADMIN_USER`/`ADMIN_PASSWORD` may coexist in `hybrid` mode as a
break-glass system-administrator path. Remove them for an OIDC-only Dashboard.

## CLI browser-assisted login

The recommended CLI login does not accept an upstream provider token:

```bash
export PORTAGE_ENGINE_TOKEN="$(
  portage-client login -server=https://api.portage.example.org
)"
```

Use `-no-browser` on a headless host or `-out=<path>` to write the final
platform session with mode `0600`. `device-login` is an alias. The existing
`token-exchange` command remains compatible for clients that already possess
an upstream provider credential.

The authorization boundary is deliberately asymmetric:

- device start and polling are unauthenticated public-client operations,
  bounded by the normal pre-authentication rate limit;
- `/device` redirects through the existing provider login while preserving
  only the short `user_code` return target;
- approval/denial requires an active `federated-session`; local/legacy
  administrator sessions fail closed;
- the 256-bit device capability and final `pe1_` bearer are carried only in
  POST bodies/responses, are never placed in a URL, and are persisted only as
  SHA-256 digests;
- PostgreSQL serializes polling and creates the platform session in the same
  transaction that consumes an approval, so concurrent polls yield exactly
  one bearer and replay yields `expired_token`;
- expiry, denial, an invalid approver session, and a success response lost in
  transit never permit a second issue from the same code.

The approved identity is the stable provider `(issuer, subject)`. Device login
does not derive permission from email, username, group, repository membership
or provider role claims. Every use of the resulting platform session reads
current project roles from PostgreSQL `project_memberships`, so approval does
not freeze or amplify access.

Set the control plane's external Dashboard route:

```ini
DEVICE_AUTHORIZATION_VERIFICATION_URI=https://portage.example.org/device
```

See [IDENTITY_PROVIDERS.md](IDENTITY_PROVIDERS.md) for protocol responses,
headless use and operational checks.

## Bootstrap projects and members

Use a system-administrator identity to create a project:

```bash
curl --fail-with-body \
  -H "Authorization: Bearer ${PORTAGE_ENGINE_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"name":"gentoo-desktop","description":"Reviewed desktop packages"}' \
  https://portage.example.org/api/v1/projects
```

Pre-provision an exact issuer/subject as the first owner:

```bash
curl --fail-with-body -X PUT \
  -H "Authorization: Bearer ${PORTAGE_ENGINE_TOKEN}" \
  -H "X-Project-ID: gentoo-desktop" \
  -H "Content-Type: application/json" \
  -d '{
    "issuer":"https://idp.example.org/realms/portage",
    "subject":"exact-provider-subject",
    "role":"owner"
  }' \
  https://portage.example.org/api/v1/projects/members
```

The database serializes membership changes per project and refuses to demote
or remove the last owner.

List the current identity and its projects:

```bash
portage-client whoami \
  -server=https://portage.example.org \
  -token="${PORTAGE_ENGINE_TOKEN}"
```

Submit within one project:

```bash
export PORTAGE_ENGINE_PROJECT=gentoo-desktop
portage-client build \
  -server=https://portage.example.org \
  -token="${PORTAGE_ENGINE_TOKEN}" \
  -package=app-misc/jq -wait
```

## Project admission policy

Each project has a versioned PostgreSQL policy. View its limits and current
usage:

```bash
portage-client project-policy \
  -server=https://portage.example.org \
  -token="${PORTAGE_ENGINE_TOKEN}" \
  -project=gentoo-desktop
```

An owner (or system administrator) can replace it using the returned version:

```bash
portage-client project-policy-set \
  -server=https://portage.example.org \
  -token="${PORTAGE_ENGINE_TOKEN}" \
  -project=gentoo-desktop \
  -version=1 -max-queued=100 -max-active=4 -max-daily=500 \
  -priority-weight=100 -starvation-seconds=300 \
  -max-vcpus=32 -max-memory-mib=131072 -max-disk-gib=1000 \
  -max-artifact-bytes=34359738368 \
  -max-claimed=4 -max-provision=2 -max-build=4 \
  -max-verify=2 -max-publish=1 \
  -max-daily-build-minutes=1440 \
  -max-daily-cloud-cost-microunits=1000000000 \
  -max-failures-hour=20 -abuse-cooldown-seconds=3600
```

`PUT /api/v1/projects/policy` uses optimistic versioning, so two owners cannot
silently overwrite each other. The Dashboard project context displays
`queued/limit`, `active/limit`, vCPU/memory/disk reservations, active
quarantine bytes, build-time/cloud-cost usage, active runtime reservations,
trailing-hour failures, UTC-today submissions and both manual/automatic
suspension. It also displays the scheduler weight and anti-starvation
threshold. See [Scheduler fairness and autoscaling](SCHEDULER.md).

Submission and scheduler claim transactions lock the same project policy row
before checking usage. This is the correctness boundary for multiple API
replicas and workers; Redis is only a short-burst accelerator. A repeated
request with the same project-scoped `Idempotency-Key` is resolved before
admission, so replay returns the existing job even after a limit is reached or
the project is suspended.

| HTTP | `code` | Meaning |
| --- | --- | --- |
| `423` | `project_suspended` | New submission/retry disabled; queued jobs are not claimed |
| `423` | `project_abuse_suspended` | Failure-storm cooldown is active; an owner may clear it explicitly |
| `429` | `queued_limit_reached` | Project queued-job cap is full |
| `429` | `daily_submission_limit_reached` | UTC calendar-day cap is full |
| `429` | `resource_request_invalid` | Catalog resource class is missing a required accounting dimension |
| `429` | `vcpu_request_exceeds_limit` | One job requests more vCPUs than the project permits |
| `429` | `memory_request_exceeds_limit` | One job requests more memory than the project permits |
| `429` | `disk_request_exceeds_limit` | One job requests more disk than the project permits |
| `429` | `build_minute_request_exceeds_daily_limit` | One catalog class cannot fit the project's daily runtime budget |
| `429` | `cloud_cost_request_exceeds_daily_limit` | One catalog class cannot fit the project's daily estimated-cost budget |
| `429` | `runtime_budget_request_invalid` | Catalog runtime or cost accounting is missing/invalid |
| `429` | `authentication_rate_limit` | Pre-authentication source-IP/path short-burst limit |
| `429` | `request_rate_limit` | Authenticated identity/path write short-burst limit |

Policy changes and authenticated admission/rate-limit denials are written to
`audit_events`. The pre-authentication denial cannot have a durable actor and
is captured by structured access logs. Lowering `max_active_jobs` does not kill
existing work; it prevents further claims until usage falls below the new
limit. The same non-preemptive rule applies when a resource limit is lowered.

## Attempt resource reservations

Schema v11 creates one PostgreSQL reservation per claimed attempt. Claiming a
job locks its project policy, sums active reservations, and atomically checks
job count, vCPU, memory MiB and disk GiB before inserting the attempt,
reservation and worker lease. A temporarily full resource budget leaves the
job queued with a short database-clock backoff so another runnable job can be
considered.

The reservation phase follows the durable attempt through `claimed`,
`provision`, `build`, `verify` and `publish`. Terminal completion, failure,
cancellation and lease-expiry recovery release it in the same transaction as
the corresponding job/attempt transition. The policy API derives live usage
from this ledger; Redis and process-local worker counts are not quota
authorities.

Catalog resource classes must contain positive `cores`, `memory` and
`disk_size`. Pre-catalog compatibility jobs are charged the built-in medium
shape (4 vCPU, 8192 MiB, 50 GiB); new partially specified catalog jobs fail
closed. Drain active attempts before applying schema v11 so no pre-migration
executor can run without a reservation.

## Artifact quarantine byte budgets

Schema v12 snapshots `max_artifact_bytes_per_job` when an attempt first opens
its private quarantine. Each retained generation is accounted independently:
the collected unsigned generation remains charged while the isolated signer
creates its signed generation, so the ledger captures the real dual-generation
peak. After the signed generation is verified and adopted, unsigned bytes are
released. Promotion, failure, cancellation, lease expiry and quarantine
cleanup release the remaining reservation.

Outbound worker uploads receive a one-shot limit equal to the durable
remaining budget. Legacy HTTP downloads reject oversized `Content-Length` and
also stop unknown-length streams after one sentinel byte beyond the remaining
limit. The signer receives a durable maximum output size, and promotion
recomputes the selected generation from regular files before acquiring the
publication path.

The project Dashboard reports aggregate active quarantine bytes and the
per-job cap. Lowering the policy does not preempt attempts that already
snapshotted a limit; new attempts use the new value. Drain active attempts
before schema v12 so no older executor can write unaccounted quarantine bytes.

## Pipeline phase capacity

Schema v13 adds per-project hard caps for `claimed`, `provision`, `build`,
`verify` and `publish`. Claim admission and every phase change lock the same
project policy row before counting active `project_resource_reservations`, so
two control-plane replicas cannot both consume the final slot.

An attempt that reaches a full checkpoint keeps renewing its durable worker
lease and waits without changing the job state or consuming the destination
phase. Once another attempt leaves, the same fenced transition is retried and
admitted atomically. Lowering a phase cap is non-preemptive: current attempts
finish, while new transitions wait for usage to fall below the limit.

This is IAM-1B2b1's checkpoint scheduler. The executor still owns the whole
pipeline while paused; a later scheduler iteration must split phases into
independently claimable work items so waiting provision/publish capacity does
not occupy a build executor slot. Drain active attempts before schema v13 so
an older executor cannot bypass the checkpoint Gate.

## Durable phase work queue

Schema v14 creates four independently fenced work items for every attempt:
`provision`, `build`, `verify` and `publish`. Each item has its own owner,
monotonic claim fence and renewable lease. A PostgreSQL `SKIP LOCKED` claim
checks the destination project phase cap atomically; completion releases that
capacity, wakes deferred peers and makes only the next item ready. An expired
phase lease can be reclaimed, and the previous fence cannot complete it.
Attempt failure, cancellation or whole-attempt lease expiry cancels all
remaining items transactionally.

The rollout is deliberately two-mode. Existing attempts create a `shadow`
plan that cannot be claimed. A plan becomes `active` only through an explicit
fenced cutover while the attempt is still at its initial `claimed` boundary.
This prevents the legacy whole-pipeline executor and a new phase executor from
running the same build simultaneously.

IAM-1B2b2a stops at this database authority: production execution remains on
the legacy path while plans are shadow-only.

## Durable Worker Gateway spool

Schema v15 moves worker sessions, commands, completions and upload
capabilities out of the process-local Broker. Any control-plane replica can
enqueue a stable command ID, another replica can deliver it, and a third can
accept its completion. Delivery and upload claims have independent monotonic
fences. An expired claim can be replayed, but a completion from the older
fence is rejected. Terminal jobs, explicit cancellation and scheduler lease
expiry revoke the session and cancel unfinished commands/uploads in the same
database transaction.

The builder copies the durable command ID into `LocalBuildRequest.ExecutionID`.
Submitting the same build command again returns the original persisted
`BuildJob`, including an interrupted failure after builder restart, instead of
running `emerge` twice. Reusing that ID with a different decoded build request
is a hard conflict. Upload generations use fence-qualified temporary
files; PostgreSQL selects the winning digest/size generation before it can be
renamed into the quarantine destination. Recovery rechecks both size and
SHA-256. The gateway also normalizes every destination and confines it to the
configured quarantine/spool root both when the control plane creates the
capability and after a durable claim is read back from PostgreSQL. A malformed
or corrupted row therefore cannot redirect a worker upload elsewhere on the
server filesystem.

The schema-v15 Gate covered a drained v14 migration, two independent
repository/control-plane views, expired delivery and upload reclaim, stale
completion rejection, Broker restart/status recovery, cross-replica
dispatch/completion, builder restart idempotency, and the full local Compose
stack. Worker Gateway executor protocol 4 was introduced there. Schema v16 and
the IAM-1B2b2c Gate subsequently promoted phase execution to `active`; active
mode does not start the legacy whole-pipeline executor. Schema v20 capability
routing raises the current protocol to 5.

## Active phase execution

Schema v16 adds a non-secret, attempt-fenced execution context for the
provisioned instance, worker identity, quarantine generation and published
artifact projection. `PHASE_EXECUTOR_MODE=active` starts separate admission
and phase-worker loops: admission is the only shadow-to-active cutover path,
and the legacy whole-pipeline executor is not started. Capacity-blocked
`ready` work stays in PostgreSQL, so waiting for a provision, build, verify or
publish cap does not consume a Go executor slot.

Every phase renews its own 45-second lease and the owning attempt lease.
Commands, collect uploads and verification commands derive stable UUIDs from
the durable phase work ID. A replacement replica therefore waits for the same
command result and upload generation. After any long-running worker result,
and immediately before signing or publication, the executor must renew the
exact phase owner/fence. A stale replica may observe the durable result but
cannot append its logs, collect artifacts, sign, publish or commit context.
Final publish, job/attempt completion, lease/resource/artifact-budget release
and Worker Gateway revocation are one PostgreSQL transaction.

The first empty Worker Gateway long poll is also a committed session
heartbeat. Treating “no command yet” as a transaction error would roll back
`connected_at` and deadlock provisioning while it waited to enqueue the first
command. Completion delivery is exact-replay idempotent so an accepted
database write followed by a lost HTTP response can be retried safely;
different payloads or delivery fences remain rejected.

## Exact executor capability routing

Schema v20 makes heterogeneous execution an explicit PostgreSQL contract.
Each queued job and each `provision`, `build`, `verify`, or `publish` work item
stores a canonical all-of requirement set. Labels bind the exact phase,
provider, execution zone, architecture, build mode, profile ID and immutable
image generation. When present, the resolved egress policy ID and digest are
also required. A worker is eligible only if its active, protocol-5 durable
record contains every label; a similar architecture, profile, image, provider,
or zone is never used as fallback.

`EXECUTOR_ZONES` declares the catalog zones reachable by one control-plane
replica. With an empty `EXECUTOR_CAPABILITIES`, that replica derives its labels
only from catalog profiles whose provider and zone match its configuration.
An explicit capability list is an exact override for specialized pools such
as a desktop verifier. The HA Compose overlay provides a separate secondary
override, so enabling a new VM or replica affects only the capabilities it
advertises rather than every VM in the cluster.

Admission requires at least one live matching capability worker before it
claims a queued job. The subsequent phase claim repeats the same match in the
transaction that registers/heartbeats the executor and increments the phase
fence. Scheduler, project-policy and Worker Gateway status distinguish
`unschedulable` from ordinary queued/ready work and expose the count of live
capability workers. Schema v20 is a drain-required migration: legacy rows
receive the fail-closed `legacy:unroutable` requirement instead of silently
running on a partially upgraded executor.

## Runtime and estimated cloud-cost budgets

Schema v17 adds one `project_attempt_usage` row per claimed attempt. Claiming
locks the project policy and reserves the selected resource class's full
`max_runtime_minutes` plus
`max_runtime_minutes * cloud_cost_microunits_per_minute`. Active reservations
and settled charges share the same UTC-day budget, so concurrent replicas
cannot each spend the final allowance. A full daily budget defers queued work
until the next UTC day; a single request larger than the project limit is
rejected before a job is created.

The runtime deadline is enforced in attempt and phase lease renewal, claim
validation and the final side-effect fence. Terminal settlement charges at
least one second of actual wall time, capped by the attempt reservation, and
releases unused reservation. The cap prevents lease-recovery latency after a
deadline from manufacturing usage that was never admitted. Cloud cost starts
when provisioning begins and is charged in ceiling minutes, also capped by its
reservation; a request that never starts provisioning incurs no cloud charge.
This is a stable scheduler/accounting estimate from the operator catalog, not
an invoice or a dependency on an external billing API.

Failed and expired attempts are counted over a trailing hour. Reaching
`max_failures_per_hour` sets a separate `abuse_suspended_until` for
`abuse_cooldown_seconds`, records the reason/generation and emits a
`project.abuse_suspended` audit event. It never overwrites the owner's manual
`suspended` flag. An owner can clear only the automatic state with
`-clear-abuse-suspension`; this is also versioned and audited.

Drain all nonterminal jobs and active worker leases before applying schema
v17. The migration raises the minimum executor protocol to 4 so an older
replica cannot claim work without the runtime ledger/deadline fences.

## Session lifecycle and administrator step-up

Schema v18 stores a SHA-256 token fingerprint and lifecycle metadata for each
session; schema v19 adds issuer-scoped OIDC logout replay records. Neither
stores or returns bearer-token bytes, upstream credentials, or raw logout JWTs.
PostgreSQL is authoritative for expiry, maximum lifetime, idle timeout,
per-session revocation, and the per-subject `tokens_valid_after` watermark used
by “revoke all”. Revocation therefore takes effect on every control-plane
replica without relying on Redis or a sticky session.

```ini
OIDC_SESSION_IDLE_MINUTES=60
OIDC_SESSION_MAX_MINUTES=720
OIDC_STEP_UP_MAX_AGE_MINUTES=10

# Optional: require one accepted authentication method and context.
OIDC_STEP_UP_AMR_VALUES=otp,hwk
OIDC_STEP_UP_ACR_VALUES=urn:example:mfa
```

An OIDC high-risk write requires a recent `auth_time`. If AMR/ACR allowlists
are configured, the token must also match both policies. The Dashboard starts
a new Authorization Code + PKCE flow with `prompt=login` and `max_age=0`.
API clients may exchange a fresh provider credential and send the resulting
platform session separately as
`X-Step-Up-Authorization: Bearer ...`; it must have the same exact issuer and
subject as the primary bearer credential.

Legacy/hybrid break-glass administration uses a different secret:

```ini
API_KEY=<primary-api-key>
STEP_UP_API_KEY=<independent-step-up-key>
```

The primary key cannot be reused as the step-up key. The local Dashboard asks
the administrator to re-enter local credentials, then grants a ten-minute
HttpOnly, SameSite=Strict elevation bound to that exact Dashboard session. It
forwards `SERVER_STEP_UP_API_KEY` only while that elevation is valid.

High-risk operations include creating projects, changing project members or
admission policy, deleting jobs, global session revocation, and non-read
system-administrator settings, worker, scheduler, image-factory, cache,
artifact and runtime-metadata operations. Self-revoking one session does not
require step-up; revoking another subject's session requires a stepped-up
system administrator.

Manage sessions with the CLI:

```bash
portage-client sessions \
  -server=https://portage.example.org \
  -token="${PORTAGE_ENGINE_TOKEN}"

portage-client session-revoke \
  -server=https://portage.example.org \
  -token="${PORTAGE_ENGINE_TOKEN}" \
  -session-id=<session-uuid>

portage-client sessions-revoke-all \
  -server=https://portage.example.org \
  -token="${PORTAGE_ENGINE_TOKEN}" \
  -step-up-token="${PORTAGE_ENGINE_FRESH_TOKEN}"
```

“Revoke all” advances a second-precision subject watermark and accepts only
tokens whose `iat` is strictly newer. This deliberately fails closed for a
token minted in the same second as revocation: re-authenticate after the
operation. OIDC providers that declare back-channel logout support can revoke
by issuer plus `sid` or `sub`; raw logout JWTs and `jti` values are not
retained. Providers without that protocol, including the GitHub OAuth App
flow, continue to rely on local logout, idle/max lifetime and revoke-all.
Server-side revocation remains authoritative for Portage Engine requests.

## Trusted-LAN HTTP

The normal control plane and Dashboard may use HTTP during an explicitly
trusted-LAN bring-up. An HTTP OIDC issuer or non-loopback HTTP callback also
requires `OIDC_ALLOW_INSECURE_HTTP=true`.

This is transport opt-in, not encryption: any network observer can copy bearer
tokens and session traffic. Keep the service bound/firewalled to the trusted
segment and move to HTTPS before untrusted users or networks are involved. The
dedicated worker identity channel remains TLS 1.3 mutual TLS regardless of this
control-plane choice.

## Workload issuer lifecycle

Schema v21 makes PostgreSQL authoritative for the public lifecycle metadata of
Worker Gateway credentials. Every issued leaf records:

- the SHA-256 fingerprint and serial of the leaf;
- the exact worker, job, attempt and attempt fence;
- its validity interval;
- the logical issuer ID, provider and CA-generation fingerprint.

Private keys and certificate PEM are never written to PostgreSQL. After the Go
TLS stack verifies the client chain, every pull, completion and upload request
recalculates the peer leaf fingerprint and checks the leaf, issuer generation,
session, current attempt and fence. Revoking a leaf, issuer, session, attempt or
job therefore takes effect on all Gateway replicas without a CRL cache or
Redis.

The local reference provider is configured as:

```ini
WORKER_GATEWAY_ISSUER_ID=community-build-workers
WORKER_GATEWAY_ISSUER_PROVIDER=file
WORKER_GATEWAY_ISSUER_CERT=/run/secrets/workload-ca.crt
WORKER_GATEWAY_ISSUER_KEY=/run/secrets/workload-ca.key
```

The key must be owner-only (`0600`). The file provider reloads the pair for
each issuance, so replacing both files switches new attempts to a new
generation. It does not retain the signing key in a package-level singleton.
For production, the implemented Vault PKI provider keeps the CA private key
outside every Portage Engine process. The control plane generates an Ed25519
worker key and CSR locally, submits only that CSR to the configured Vault role,
then validates the returned public chain, exact URI identity, client-auth EKU,
requested TTL and current Gateway listener trust roots before returning the
one-shot key to the worker:

```ini
WORKER_GATEWAY_ISSUER_ID=community-vault-workers
WORKER_GATEWAY_ISSUER_PROVIDER=vault
WORKER_GATEWAY_VAULT_ADDRESS=https://vault.internal.example
WORKER_GATEWAY_VAULT_MOUNT=pki_workers
WORKER_GATEWAY_VAULT_ROLE=portage-worker
WORKER_GATEWAY_VAULT_TOKEN_FILE=/run/secrets/vault-token
WORKER_GATEWAY_VAULT_SERVER_CA=/run/secrets/vault-server-ca.pem
WORKER_GATEWAY_VAULT_TIMEOUT_SECONDS=15
```

The Vault address must be an HTTPS origin; this does not require the normal
Portage Engine control-plane listener to use HTTPS on a trusted LAN. Redirects
and environment proxies are disabled for the signing request so the Vault
token cannot be forwarded to another host. The token file must be a regular,
non-symlink, owner-only file. A Vault Agent renewable file sink is preferred
over a static long-lived token.

Create a role that can issue only attempt-bound URI identities and client
certificates:

```bash
vault secrets enable -path=pki_workers pki
vault secrets tune -max-lease-ttl=8760h pki_workers

# Import/use an existing CA or generate the integration CA according to the
# site's PKI policy before creating this role.
vault write pki_workers/roles/portage-worker \
  allow_any_name=true \
  enforce_hostnames=false \
  allowed_uri_sans='spiffe://portage-engine/*' \
  use_csr_common_name=true \
  use_csr_sans=true \
  server_flag=false \
  client_flag=true \
  code_signing_flag=false \
  key_type=any \
  ttl=3h \
  max_ttl=24h
```

The application token needs only one capability:

```hcl
path "pki_workers/sign/portage-worker" {
  capabilities = ["update"]
}
```

Do not grant `issue`, role mutation, issuer mutation, root generation, token
creation or policy administration to the Portage Engine token. In production,
prefer a short-period batch token delivered and renewed by Vault Agent. The
server rereads the token file for each issuance, so agent rotation and recovery
do not require restarting the control plane.

Use a staged rotation for either provider:

1. Drain active Worker Gateway sessions before applying schema v21.
2. Add the new CA certificate to `WORKER_GATEWAY_CLIENT_CA` while keeping the
   old CA, then roll the Gateway listeners. TLS trust bundles are loaded when
   listeners start.
3. For `file`, atomically replace `WORKER_GATEWAY_ISSUER_CERT` and
   `WORKER_GATEWAY_ISSUER_KEY`. For `vault`, switch the role/default issuer
   only after every Gateway replica serves the dual-CA trust bundle. The first
   new issuance registers the new generation as `active` and changes the
   previous generation to `draining`.
4. Wait at least the configured leaf TTL and confirm the old generation has no
   active certificates. Then remove the old CA from the trust bundle and roll
   listeners again.
5. Use issuer revocation only for compromise response. Revocation is
   irreversible for that fingerprint and terminates all of its active worker
   sessions, commands and uploads.

System administrators can inspect the redacted inventory:

```bash
curl -fsS \
  -H "Authorization: Bearer ${PORTAGE_ENGINE_TOKEN}" \
  https://portage.example.org/api/v1/worker-gateway/identities | jq
```

Leaf and issuer revocation are high-risk writes and require the same fresh
step-up policy as other system-administrator mutations:

```bash
curl -fsS -X POST \
  -H "Authorization: Bearer ${PORTAGE_ENGINE_TOKEN}" \
  -H "X-Step-Up-Authorization: Bearer ${PORTAGE_ENGINE_FRESH_TOKEN}" \
  -H "Content-Type: application/json" \
  --data '{"fingerprint":"<64-lowercase-hex>","reason":"incident-123"}' \
  https://portage.example.org/api/v1/worker-gateway/certificates/revoke

curl -fsS -X POST \
  -H "Authorization: Bearer ${PORTAGE_ENGINE_TOKEN}" \
  -H "X-Step-Up-Authorization: Bearer ${PORTAGE_ENGINE_FRESH_TOKEN}" \
  -H "Content-Type: application/json" \
  --data '{"fingerprint":"<64-lowercase-hex>","reason":"CA compromise"}' \
  https://portage.example.org/api/v1/worker-gateway/issuers/revoke
```

The Monitor shows active, draining and revoked issuer counts, active/revoked
leaf counts, leaves expiring within 30 minutes, and the redacted generation
inventory. Provider health additionally exposes consecutive failures and the
last success/failure timestamp with a bounded, credential-free error. Alert
when the provider is unhealthy, there is no active issuer, an unexpected
second active generation, a revoked generation still appears in a listener
trust bundle, or certificate authorization failures rise above the normal
stale-attempt rate.

At startup, an enabled Gateway performs a real sign-and-verify probe and refuses
to start if Vault is unreachable, the token is unauthorized, or the returned
generation is not in that instance's listener trust bundle. At runtime, a Vault
failure blocks only new certificate issuance and degrades `/health`; already
issued, unexpired and non-revoked worker sessions continue to be authorized
against PostgreSQL. Restore/renew the owner-only token file or Vault service,
then the next successful issuance clears the failure counter. Never bypass the
trust check to recover from an incorrectly ordered CA rotation—restore the old
Vault default issuer or deploy the dual trust bundle first.

Run the repeatable real-Vault Gate locally:

```bash
scripts/test-vault-issuer.sh
```

It pins `hashicorp/vault:2.0.3`, creates a temporary TLS dev server and
sign-only token, proves token failure/recovery, proves that switching Vault
before listener trust rollout fails closed, then proves old → dual → new CA
rollover. Dev mode and its generated CA are test-only and are destroyed when
the script exits.

Operational references:

- [Vault PKI sign API and role constraints](https://developer.hashicorp.com/vault/api-docs/secret/pki)
- [Vault PKI setup](https://developer.hashicorp.com/vault/docs/secrets/pki/setup)
- [AppRole authentication](https://developer.hashicorp.com/vault/docs/auth/approle)
- [Vault Agent file sink](https://developer.hashicorp.com/vault/docs/agent-and-proxy/autoauth/sinks/file)
- [TLS dev server flags](https://developer.hashicorp.com/vault/tutorials/get-started/setup)

## Remaining public-service IAM work

IAM-0 through IAM-1H now establish identity, isolation, manual and automatic
suspension, atomic queued/active limits, UTC-daily submission/runtime/cost
caps, dual-layer short-burst limiting, durable per-attempt accounting,
cross-replica token/session revocation, one-time CLI device authorization,
administrator step-up, and exact
heterogeneous executor routing, plus a durable workload leaf/issuer revocation
boundary plus a real external Vault PKI issuer, listener-bound trust rollover,
and failure recovery Gate. They do not make the service ready for unrestricted
public submissions. Remaining work is a production-site Vault HA/unseal/backup
runbook and long-duration drill, real Authentik/Google/GitHub community-domain
callback validation, explicit account-linking policy, fair scheduling, and
production SLO/abuse operations.
