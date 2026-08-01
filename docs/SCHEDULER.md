# Scheduler fairness and autoscaling

Schema v28 makes project fairness, catalog-derived capacity-pool demand,
fenced capacity actions, pull-aware worker scoring, the target-history read
model, and lease-recovery observability part of the PostgreSQL authority.
Redis remains a wake-up and presence accelerator and may be flushed without
changing dispatch order.

## Dispatch order

The admission queue and active phase queue use the same sequence:

1. Filter by hard constraints: state, project suspension and quota, lease,
   executor protocol, and the complete required-capability set.
2. Lock one `project_scheduler_fairness` row with `FOR UPDATE SKIP LOCKED`.
3. Prefer a project whose oldest runnable work exceeded its
   `starvation_threshold_seconds`; starved projects are ordered oldest first.
4. Otherwise choose the lowest project virtual runtime.
5. Within that project, select the normal job/phase row and apply its existing
   fence, budget, and capacity transaction.
6. Only after a successful claim, add
   `ceil(1000 / priority_weight)` to the corresponding admission or phase
   virtual runtime.

The fairness row and job/work row are committed together. A failed,
capacity-deferred, or rolled-back claim consumes no fairness credit.

`priority_weight` is a relative share, not an absolute priority. A project with
weight 300 receives roughly three opportunities for every opportunity given to
a continuously backlogged project with weight 100, subject to hard quotas and
executor compatibility. Queue-age promotion guarantees that a lower-weight
eligible project cannot wait indefinitely.

New projects begin at the largest current virtual runtime. Creating a new
project therefore does not reset consumed scheduler share.

## Pull-aware worker scoring

Hard constraints remain absolute: a polling executor must be active, fresh,
protocol-compatible, capability-compatible, and—except for the admission
coordinator—not already executing work. Among eligible workers, schema v25
records:

- candidate count;
- the worker's bounded `pressure_score` (default `500` until a runtime
  telemetry producer supplies it);
- failed attempts or phase claims during the previous hour; and
- the exact selection reason.

Pressure and recent failures are deliberately soft observations. They never
turn a compatible pull worker into a hard mismatch. A design that selected a
different worker and returned an empty poll would be a push scheduler hidden
inside a pull protocol and could stall until that other worker happened to
poll. The current contract therefore lets the eligible polling worker claim,
persists the score for explanation, and bounds candidate freshness to five
seconds. A future adaptive worker backoff may consume these observations
without changing lease/fence authority.

Admission workers declare `worker-kind:admission`, phase workers declare
`worker-kind:phase`, and compatibility workers declare `worker-kind:legacy`.
The admission coordinator is not an execution slot and may continue admitting
jobs while previous attempts own leases. Phase and legacy workers must be free.

## Target history, SLO, and cost

Schema v26 exposes `monitor_job_outcomes`, a read-only projection derived from
terminal job, attempt, and usage ledgers. It groups targets by project,
provider, execution zone, architecture, build mode, profile, immutable image
generation, and resource class. The scheduler status and Monitor return
24-hour, 7-day, and 30-day windows with:

- success, failure, and cancellation samples;
- success rate and a 95% SLO result once at least five success/failure samples
  exist;
- queue and run-duration P50/P95;
- catalog-reserved versus settled cloud-cost microunits; and
- the dominant recorded failure class.

Target IDs are short SHA-256-derived display identities. Package atoms, job
IDs, request payloads, subjects, credentials, and logs are not part of this
operator projection. The view is bounded to the 50 most active targets in the
30-day retention window and cached for 30 seconds per control-plane replica so
dashboard and Prometheus polling do not rescan it per request. Costs are
internal estimates/settlements from the admission ledger, not provider-invoice
reconciliation.

The Monitor projection publishes two database-derived event-time watermarks.
The source watermark is the latest `completed_at`/`updated_at` of a durable
terminal job. The projected watermark is the latest of those same persisted
source timestamps whose job is represented by `monitor_job_outcomes` in the
cached snapshot. When the source is ahead,
`lag_seconds = source_watermark - projected_watermark`: an event-time distance
between two persisted timestamps. Before the first projected watermark exists,
the fallback is `database_clock - source_watermark`. A caught-up projection and
an empty source both report zero. `source_watermark_present` distinguishes the
valid empty case. The cache refresh interval is 30 seconds; the checked-in
warning threshold is more than 120 watermark seconds and must remain true for
another two minutes. This is projection delay, not the age of the last build.

Schema v28 also persists the fixed `attempt`/`admission` kind on each newly
claimed scheduler lease and creates seven durable lease-expiry counters.
Recovery updates the appropriate counter in the same transaction as the
fenced state change:

- `attempt` and `admission` leases use `requeued`, `failed`, or `canceled`;
- phase-work leases use `reclaimed`.

These totals begin when schema v28 is applied. The enum set is closed; job,
package, project, pool, provider, worker, profile, and image identities are not
metric labels.

## Project policy

Owners can inspect the current policy:

```bash
portage-client project-policy \
  -server http://127.0.0.1:18080 \
  -api-key "$PORTAGE_ENGINE_API_KEY" \
  -project default
```

`project-policy-set` accepts:

- `-priority-weight 1..1000`
- `-starvation-seconds 30..86400`

A value of `0` preserves the existing fairness value when an older automation
client replaces the rest of the policy.

## Capacity pools and autoscaling boundary

Every
resolved catalog environment gets a stable pool identity derived from its exact
provider, execution zone, architecture, build mode, profile, image ID, and
image generation. Jobs, phase work, and executors all carry the resulting
`capacity-pool:<id>` label in addition to their existing exact capabilities.

Every active phase-executor deployment periodically writes:

- one backward-compatible global `scheduler_autoscale_state` row; and
- one `scheduler_capacity_pool_state` row per catalog pool, including its
  readable dimensions and complete selector.

Each row contains:

- current active and busy slots;
- total and currently unschedulable backlog;
- desired slots;
- `hold`, `scale-up`, or `scale-down` recommendation;
- cooldown, scale-down dwell, and last evaluation timestamps.

`SCHEDULER_AUTOSCALE_MIN_SLOTS` is a deployment-wide floor and is not
multiplied across catalog pools. A pool with no work has a safe default minimum
of zero. A future warm-pool feature must use an explicit per-pool policy.
Pool `desired_slots` is an observe-only demand signal capped by the configured
maximum; the sum of pool signals is not an approved infrastructure budget.
In `actuate` mode a global allocation step converts those signals into
single-slot actions only when both the deployment-wide ceiling and the
explicit provider ceiling have room. Existing claimed scale-up actions count
against both budgets. Unclaimed actions are refreshed or canceled when the
recommendation changes; claimed actions are never rewritten underneath an
actuator.

Only backlog matching an active executor capability set contributes to desired
slots. Capability-mismatched work is reported separately for the exact pool,
because capacity in another provider/profile/image pool cannot make it
runnable. A shared executor may advertise multiple pools; this is observed
capacity, not proof that one pool exclusively owns that process.

The scheduler still has no PVE, cloud, Terraform, or Kubernetes client in this
path.
No VM creation/deletion occurs inside a claim transaction. In particular, the
PVE VM created during a build's provision phase is a disposable, attempt-owned
builder and is not an autoscalable phase-executor pool member.

The separate `portage-capacity-actuator` consumes the resulting PostgreSQL
actions outside scheduler transactions. Each action has an owner, expiring
lease and increasing fence. Scale-up reserves a unique
`portage-capacity-<uuid>` identity before Terraform may run, reuses that
identity after a crash, and completes only after a fresh worker heartbeat
contains both `capacity-instance:<uuid>` and the complete immutable pool
selector. Scale-down binds one actuator-owned instance to the action, marks
only that instance's worker slots draining, proves that neither admission nor
phase leases remain live, and then deletes only the exact provider identity and
generation. PVE deletion includes provider-native exact-name absence readback;
cluster counts and tags are never deletion authority.

The PVE adapter accepts only an operator allowlist of persistent-executor
templates using bootstrap contract `pve-dmi-v1`. Terraform sets the
database-assigned UUID as the guest SMBIOS product UUID. The image's systemd
pre-start helper derives `CONTROL_PLANE_ID` and
`EXECUTOR_CAPACITY_INSTANCE_ID` from that value. A persistent executor starts
no HTTP listener and must explicitly declare the capabilities for exactly one
immutable pool. It does not require the Worker Gateway listener private key,
and startup now rejects that credential if it is accidentally distributed to
the executor role. It uses the Worker Gateway advertise URL and public trust
bundles when it launches a disposable outbound-pull worker; scheduler worker
registration and phase claims retain the existing fenced PostgreSQL contract.
The executor still needs intentionally scoped PostgreSQL, artifact, PVE and
workload-certificate issuer permissions at runtime. Those values are injected
after clone and are not image inputs.

The dedicated build entry is
`image-factory/persistent-executor/run.sh`. It derives the capacity-pool ID
from provider, zone, architecture, build mode, profile, image ID and image
generation, and bakes exactly those labels plus the four phase labels. The
image gate rejects a baked `capacity-instance` label, PVE/PBS management
secret, signing key, Terraform state, API listener key, or Packer SSH
authorization. This entry is intentionally separate from the immutable
single-use job-builder image.

Configuration:

```ini
SCHEDULER_AUTOSCALE_MODE=observe
SCHEDULER_AUTOSCALE_MIN_SLOTS=1
SCHEDULER_AUTOSCALE_MAX_SLOTS=64
SCHEDULER_AUTOSCALE_TARGET_READY_PER_SLOT=2
SCHEDULER_AUTOSCALE_COOLDOWN_SECONDS=60
SCHEDULER_AUTOSCALE_SCALE_DOWN_SECONDS=600
SCHEDULER_AUTOSCALE_INTERVAL_SECONDS=15
SCHEDULER_AUTOSCALE_PROVIDER_MAX_SLOTS=pve:32
```

Use `SCHEDULER_AUTOSCALE_MODE=off` when the deployment should expose no desired
action. `actuate` fails closed unless every catalog provider represented by a
pool has a positive explicit provider limit.

Run the actuator independently:

```bash
cp configs/capacity-actuator.example.json configs/capacity-actuator.json
portage-capacity-actuator \
  -config configs/capacity-actuator.json \
  -server-config configs/server.conf
```

PVE credentials are accepted only from the existing `CLOUD_PVE_*` or `PM_*`
environment pairs and are not representable in the JSON file. Compose keeps
the daemon behind the opt-in `capacity-actuator` profile and connects it with
the dedicated `portage_actuator` database role.

## Operations

`GET /api/v1/scheduler/status` and the Dashboard Monitor show fairness and
autoscale state. Prometheus exposes:

- `portage_scheduler_queued_tasks`
- `portage_scheduler_unschedulable_tasks`
- `portage_scheduler_fair_eligible_projects`
- `portage_scheduler_fair_starved_projects`
- `portage_scheduler_fair_max_wait_seconds`
- `portage_scheduler_worker_decisions_last_hour`
- `portage_scheduler_worker_multi_candidate_last_hour`
- `portage_monitor_target_samples_30d`
- `portage_monitor_target_successes_30d`
- `portage_monitor_target_failures_30d`
- `portage_monitor_target_slo_breaches_30d`
- `portage_monitor_target_reserved_cost_microunits_30d`
- `portage_monitor_target_charged_cost_microunits_30d`
- `portage_scheduler_lease_expiries_total{lease,result}`
- `portage_monitor_projection_snapshot_valid`
- `portage_monitor_projection_source_watermark_present`
- `portage_monitor_projection_lag_seconds`
- `portage_scheduler_autoscale_active_slots`
- `portage_scheduler_autoscale_desired_slots`
- `portage_scheduler_autoscale_backlog`
- `portage_scheduler_autoscale_pools`
- `portage_scheduler_autoscale_blocked_pools`
- `portage_capacity_actuator_open_actions`
- `portage_capacity_instances_provisioning`
- `portage_capacity_instances_active`
- `portage_capacity_instances_draining`
- `portage_capacity_instances_deleting`

Package names, job IDs, project IDs, and capability strings are intentionally
not metric labels. Use structured job/audit logs for high-cardinality
explanations.

Checked-in alert ownership, severity, threshold semantics, diagnostics, and
drills are in [the observability runbooks](OBSERVABILITY_RUNBOOKS.md).

## Current limits

- Fairness is intentionally project-wide for v1 because project is the policy
  and quota boundary. Capacity pools are hard routing and observability
  boundaries, not independent fairness subqueues. Add hierarchical
  target/provider fairness only if production queue evidence shows starvation;
  doing it pre-emptively would fragment small pools and idle compatible slots.
- Executor heartbeat records bounded CPU, memory and disk pressure plus warm
  capacity-pool/profile/image cache keys. Telemetry changes update the
  snapshot without incrementing worker identity generation.
- Provider limits are deployment-wide per provider; per-project/provider
  billing limits still use the existing admission budget rather than a
  provider invoice feed.
- The code, real PostgreSQL, Compose role, Prometheus, WebUI, dedicated image
  entry and repository-side fence/readback Gate are complete. A live
  scale-up/scale-down PVE release Gate still requires site PVE credentials,
  runtime secret injection and a candidate built from the operator's accepted
  Gentoo base. The repository does not manufacture that evidence; use the
  exact `scripts/persistent-executor-gate.sh live-once` procedure in
  [PVE_TESTING.md](PVE_TESTING.md).
- Estimated and settled internal cloud cost is visible per target; provider
  invoice ingestion and reconciliation require an operator-selected billing
  export and are not implemented.
