/**
 * The parts of `/api/scheduler/status` and `/api/runtime-metadata/status` that
 * src/api/types.ts does not carry yet, transcribed from the Go declarations
 * rather than from what the old page happened to read.
 *
 * `SchedulerStatus` in the shared types stops at the in-memory authority's four
 * keys plus a sketch of the autoscaler. The durable authority answers with
 * `SchedulerRuntimeStatus` (internal/builder/manager.go) marshalled over the
 * top of that map, which is another thirty fields — the ones /monitor is
 * actually for. Everything below is optional for one reason: under the memory
 * authority none of it is sent at all, and a page that reads a missing durable
 * field as zero reports "0 expired leases" for a scheduler that has no leases.
 * `undefined` and `0` are different answers and the render tells them apart.
 *
 * `RuntimeMetadataEnvelope` is a correction, not an addition: the handler
 * answers `{enabled, ok, status}` and the shared type names the inner document,
 * so a caller of `api.runtimeMetadataStatus()` reads `.live_infra` off the
 * envelope and gets undefined. Named here so this page is right today; the
 * shared type is a handoff.
 */

import type { RuntimeMetadataStatus, Timestamp } from '../../api/types';

/** internal/builder/manager.go — LeaseExpiryStatus */
export interface LeaseExpiryStatus {
  attempt_requeued: number;
  attempt_failed: number;
  attempt_canceled: number;
  admission_requeued: number;
  admission_failed: number;
  admission_canceled: number;
  phase_reclaimed: number;
}

/**
 * internal/builder/manager.go — MonitorProjectionStatus.
 *
 * `alert_threshold_seconds` is the read-through cache TTL that produced the lag
 * reading and not a threshold to badge on: the server reloads the moment a
 * snapshot reaches that age, so every reading reachable here is strictly below
 * it and a comparison against it can only ever be false. It is rendered beside
 * the reading so the two can be scaled against each other — 3s of 30s says
 * something, 3s alone does not — and the verdict rests on `valid`.
 */
export interface MonitorProjectionStatus {
  valid: boolean;
  state: string;
  observed_at?: Timestamp;
  source_watermark_present?: boolean;
  source_watermark_at?: Timestamp;
  projected_watermark_at?: Timestamp;
  lag_seconds: number;
  alert_threshold_seconds: number;
}

/** internal/builder/manager.go — TargetReliabilityWindow */
export interface TargetReliabilityWindow {
  name: string;
  hours: number;
  samples: number;
  successes: number;
  failures: number;
  canceled: number;
  success_rate_percent: number;
  slo_met: boolean;
  insufficient_data: boolean;
  queue_p50_seconds: number;
  queue_p95_seconds: number;
  run_p50_seconds: number;
  run_p95_seconds: number;
  reserved_cost_microunits: number;
  charged_cost_microunits: number;
  dominant_failure_class?: string;
}

/** internal/builder/manager.go — TargetReliabilityStatus */
export interface TargetReliabilityStatus {
  target_id: string;
  project_id: string;
  project_name: string;
  provider: string;
  execution_zone: string;
  architecture: string;
  build_mode: string;
  profile_id: string;
  image_id: string;
  image_generation: string;
  resource_class: string;
  windows: TargetReliabilityWindow[] | null;
}

/** internal/builder/manager.go — TargetHistoryStatus */
export interface TargetHistoryStatus {
  generated_at?: Timestamp;
  retention_days?: number;
  slo_target_percent?: number;
  minimum_samples?: number;
  projection?: MonitorProjectionStatus;
  targets?: TargetReliabilityStatus[];
}

/** internal/builder/manager.go — WorkerDecisionStatus */
export interface WorkerDecisionStatus {
  work_kind: string;
  phase?: string;
  worker: string;
  candidate_count: number;
  pressure_score: number;
  recent_failures: number;
  reason?: string;
  selected_at?: Timestamp;
}

/** internal/builder/manager.go — WorkerScoringStatus */
export interface WorkerScoringStatus {
  decisions_last_hour: number;
  multi_candidate_last_hour: number;
  recent?: WorkerDecisionStatus[];
}

/** internal/builder/manager.go — SchedulerFairnessStatus */
export interface SchedulerFairnessStatus {
  enabled: boolean;
  eligible_projects: number;
  starved_projects: number;
  admission_dispatches: number;
  phase_dispatches: number;
  max_queue_wait_seconds: number;
}

/** internal/builder/manager.go — CapacityActionStatus */
export interface CapacityActionStatus {
  id: string;
  pool_id: string;
  kind: string;
  state: string;
  requested_slots: number;
  observed_slots: number;
  attempts: number;
  failure_detail?: string;
}

/** internal/builder/manager.go — CapacityInstanceStatus */
export interface CapacityInstanceStatus {
  id: string;
  pool_id: string;
  provider: string;
  provider_instance_id: string;
  state: string;
}

/** internal/builder/manager.go — CapacityActuatorStatus */
export interface CapacityActuatorStatus {
  open_actions: number;
  failed_actions: number;
  provisioning_instances: number;
  active_instances: number;
  draining_instances: number;
  deleting_instances: number;
  actions?: CapacityActionStatus[];
  instances?: CapacityInstanceStatus[];
}

/**
 * internal/builder/manager.go — SchedulerCapacityPoolStatus, which embeds
 * SchedulerCapacityPoolDefinition (internal/builder/capabilities.go), so the
 * definition's fields arrive flattened alongside the status ones.
 */
export interface SchedulerCapacityPoolStatus {
  id: string;
  provider: string;
  execution_zone: string;
  arch?: string;
  build_mode?: string;
  profile_id?: string;
  image_id?: string;
  image_generation?: string;
  provider_max_slots: number;
  mode: string;
  active_slots: number;
  busy_slots: number;
  backlog: number;
  unschedulable_backlog: number;
  desired_slots: number;
  recommendation: string;
  reason?: string;
}

/** internal/builder/manager.go — SchedulerAutoscaleStatus */
export interface SchedulerAutoscaleStatus {
  scope?: string;
  mode: string;
  active_slots: number;
  busy_slots: number;
  backlog: number;
  unschedulable_backlog: number;
  desired_slots: number;
  recommendation: string;
  reason?: string;
  pools?: SchedulerCapacityPoolStatus[];
  actuator?: CapacityActuatorStatus;
}

/**
 * What `/api/scheduler/status` actually answers.
 *
 * `authority` decides how much of the rest exists: "memory" sends the first
 * four keys and stops, "postgresql" sends the durable runtime status on top,
 * and a durable authority that cannot be reached sends `healthy: false` with an
 * `error` and nothing else — which is a degraded scheduler and not a failed
 * request, so it renders as a card.
 */
export interface MonitorSchedulerStatus {
  authority: string;
  queued_tasks: number;
  running_tasks: number;
  healthy?: boolean;
  error?: string;
  unschedulable_tasks?: number;
  active_leases?: number;
  expired_leases?: number;
  registered_workers?: number;
  active_workers?: number;
  capability_workers?: number;
  stale_workers?: number;
  attempts_last_hour?: number;
  lease_expiries?: LeaseExpiryStatus;
  fairness?: SchedulerFairnessStatus;
  worker_scoring?: WorkerScoringStatus;
  target_history?: TargetHistoryStatus;
  autoscaler?: SchedulerAutoscaleStatus;
}

/**
 * internal/server/handlers_runtime_metadata.go — the envelope, all three
 * branches of it. `status` is absent on both failure branches, which is what
 * makes "the ledger could not be asked" different from "the ledger answered and
 * six artifacts are missing".
 */
export interface RuntimeMetadataEnvelope {
  enabled: boolean;
  ok: boolean;
  error?: string;
  status?: RuntimeMetadataStatus;
}
