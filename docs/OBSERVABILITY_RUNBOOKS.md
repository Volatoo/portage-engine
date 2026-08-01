# Observability alert runbooks

These runbooks cover every alert shipped in
`deploy/observability/rules/portage-engine.yml`. The alert label
`owner=platform-operations` is the routing authority; severity determines the
response target. Do not place job, package, project, pool, provider, worker, or
other identity values in metric labels. Use the authenticated Monitor and
structured events for that detail.

Before and after a drill, validate the checked-in rules and current scrape:

```bash
docker run --rm \
  --entrypoint /bin/promtool \
  -v "$PWD/deploy/observability/rules:/rules:ro" \
  prom/prometheus:v3.11.2 \
  check rules /rules/portage-engine.yml

curl -fsS http://127.0.0.1:18080/metrics/prometheus \
  | grep '^portage_'
curl -fsS 'http://127.0.0.1:29090/api/v1/rules' \
  | jq -e '.status == "success"'
```

If metrics Basic authentication is enabled, add
`-u "metrics:$PORTAGE_METRICS_PASSWORD"` to the server `curl` command. Run
authenticated API examples with
`-H "X-API-Key: $PORTAGE_ENGINE_API_KEY"` (or the deployment's bearer token).
Run state-changing drills only in an isolated staging project and record the
start and recovery timestamps.

## PortageEngineMetricsTargetDown

Owner: `platform-operations`. Severity: `critical`.

Meaning: Prometheus has failed to scrape the server metrics endpoint for two
minutes. Check the server process, network path, metrics authentication, and
Prometheus target error before investigating downstream alerts.

```bash
curl -fsS http://127.0.0.1:18080/livez
curl -fsS http://127.0.0.1:18080/readyz
curl -fsS 'http://127.0.0.1:29090/api/v1/targets?state=active' \
  | jq '.data.activeTargets[] | select(.labels.job=="portage-engine-server")'
```

Mitigate by restoring the server listener, scrape routing, or the matching
metrics credential. Drill by temporarily directing a staging Prometheus scrape
target to an unused port; verify the alert fires after two minutes, restore the
target, and verify `up{job="portage-engine-server"} == 1` and the alert clears.

## PortageEngineStorageErrors

Owner: `platform-operations`. Severity: `critical`.

Meaning: at least one artifact storage read, write, or publication error was
observed in the trailing ten minutes. Inspect server logs and object/local
storage health before allowing publication to continue.

```bash
curl -fsS 'http://127.0.0.1:29090/api/v1/query?query=increase(portage_storage_errors_total%5B10m%5D)' | jq .
curl -fsS http://127.0.0.1:18080/health | jq '.checks'
```

Mitigate the storage availability, credentials, capacity, or integrity error;
do not bypass digest or create-only publication checks. Drill with a staging
storage endpoint whose write permission is deliberately removed, submit a
disposable build, restore permission, and verify the counter stops increasing
and the alert clears after its ten-minute lookback expires.

## PortageEngineQueueBacklog

Owner: `platform-operations`. Severity: `warning`.

Meaning: at least one durable build has remained queued for fifteen minutes.
Use Monitor to separate capacity, policy, and capability causes.

```bash
curl -fsS http://127.0.0.1:18080/api/v1/scheduler/status \
  | jq '{queued_tasks,unschedulable_tasks,fairness,autoscaler}'
curl -fsS 'http://127.0.0.1:29090/api/v1/query?query=portage_scheduler_queued_tasks' | jq .
```

Mitigate the identified executor capacity or project-policy constraint. Drill
by pausing every matching staging executor, submit one disposable job, verify
the alert after fifteen minutes, resume the executor, and verify the queued
gauge returns to zero.

## PortageEngineUnschedulableBuilds

Owner: `platform-operations`. Severity: `warning`.

Meaning: queued work has no active executor matching the exact protocol and
capability set. High-cardinality capability details are intentionally available
only through the authenticated Monitor and durable job events.

```bash
curl -fsS http://127.0.0.1:18080/api/v1/scheduler/status \
  | jq '{unschedulable_tasks,active_workers,capability_workers,autoscaler}'
curl -fsS 'http://127.0.0.1:29090/api/v1/query?query=portage_scheduler_unschedulable_tasks' | jq .
```

Mitigate by restoring an executor with the catalog-resolved capability set;
never weaken the job requirement to silence the alert. Drill with a staging
job whose profile is absent from all staging workers, confirm the alert after
ten minutes, restore the exact worker, and confirm the gauge returns to zero.

## PortageEngineCapacityProvisioningStuck

Owner: `platform-operations`. Severity: `warning`.

Meaning: an actuator-owned persistent executor has remained in provisioning
for thirty minutes without its exact fenced heartbeat.

```bash
curl -fsS http://127.0.0.1:18080/api/v1/scheduler/status \
  | jq '.autoscaler.actuator'
curl -fsS 'http://127.0.0.1:29090/api/v1/query?query=portage_capacity_instances_provisioning' | jq .
```

Mitigate the provider action, bootstrap, identity, or heartbeat failure while
preserving action and instance fences. Drill only with the reviewed persistent
executor template: block its staging heartbeat, verify the alert, restore the
heartbeat, and confirm the instance becomes active or the fenced action reaches
a terminal failure. Do not use the disposable job-builder template.

## PortageEngineTargetSLOBreach

Owner: `platform-operations`. Severity: `warning`.

Meaning: at least one of the bounded Monitor targets has five or more
success/failure samples and a 30-day success rate below 95 percent.

```bash
curl -fsS http://127.0.0.1:18080/api/v1/scheduler/status \
  | jq '.target_history.targets[] | . as $t | .windows[] | select(.name=="30d" and (.insufficient_data|not) and (.slo_met|not)) | {target_id:$t.target_id,window:.}'
curl -fsS 'http://127.0.0.1:29090/api/v1/query?query=portage_monitor_target_slo_breaches_30d' | jq .
```

Mitigate the dominant failure class shown by Monitor and validate a successful
build on the same target. Drill with five disposable staging jobs on one target,
including enough controlled failures to fall below 95 percent; verify the alert
and record that the rolling 30-day signal does not clear immediately after one
success.

## PortageEngineLeaseExpiry

Owner: `platform-operations`. Severity: `warning`.

Meaning: a durable lease expired in the trailing ten minutes. The fixed labels
are `lease=attempt|admission` with `result=requeued|failed|canceled`, or
`lease=phase,result=reclaimed`. Counters start at schema v28 and are incremented
in the same PostgreSQL transaction as recovery.

```bash
curl -fsS http://127.0.0.1:18080/metrics/prometheus \
  | grep '^portage_scheduler_lease_expiries_total'
curl -fsS http://127.0.0.1:18080/api/v1/scheduler/status \
  | jq '.lease_expiries'
```

Investigate replica loss, executor health, database latency, and lease-renewal
logs. A `requeued` or `reclaimed` result confirms fencing recovery, not task
success; `failed` or `canceled` is terminal. Drill by pausing one disposable
staging executor beyond its lease, resuming another compatible executor, and
verifying exactly one expected series increments while the stale fence cannot
complete work. Recovery is complete when no expired live lease remains and the
replacement attempt or phase reaches the intended state.

## Monitor read-model age (no alert)

`portage_monitor_projection_lag_seconds` is charted but deliberately not
alerted. `monitor_job_outcomes` is a plain view over the same base tables the
source watermark reads, so a fresh read is always current; the only thing that
can be stale is the per-replica 30-second read-through cache in
`targetHistoryStatus`, and a failed refresh returns an error rather than serving
older data. The gauge is therefore the age of the cached snapshot being served —
zero when freshly loaded or empty, bounded above by the 30-second cache TTL. Any
threshold above that could never fire, which is why the former
`PortageEngineMonitorProjectionLag` rule was removed rather than retuned. A
genuinely broken read model surfaces as
`PortageEngineMonitorProjectionUnavailable` below.

```bash
curl -fsS http://127.0.0.1:18080/api/v1/scheduler/status \
  | jq '.target_history.projection'
curl -fsS http://127.0.0.1:18080/metrics/prometheus \
  | grep '^portage_monitor_projection_'
```

## PortageEngineMonitorProjectionUnavailable

Owner: `platform-operations`. Severity: `critical`.

Meaning: PostgreSQL authority is configured but the Monitor projection
snapshot has reported invalid for five minutes. Empty data is valid and does
not fire this alert.

```bash
curl -fsS http://127.0.0.1:18080/api/v1/scheduler/status | jq '{healthy,error,target_history}'
curl -fsS 'http://127.0.0.1:29090/api/v1/query?query=portage_monitor_projection_snapshot_valid' | jq .
```

Mitigate the database/view/schema error and confirm the current schema v30 is
compatible and includes migration 00028.
Drill by revoking the staging application role's `SELECT` permission on
`monitor_job_outcomes`, verify the alert after five minutes, restore the grant,
and verify `snapshot_valid` returns to 1. Empty the staging job history as a
negative drill and confirm the snapshot remains valid with
`source_watermark_present=0` and `lag_seconds=0`.
