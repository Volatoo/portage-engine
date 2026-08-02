/**
 * The wire shapes, transcribed from the Go handlers that produce them.
 *
 * Every field below was read off a struct tag or a `map[string]any` literal in
 * internal/dashboard or internal/server, and the Go declaration is named above
 * each block. Nothing here is inferred from what a page happened to use: the
 * console this replaces read `b.resolved_context.egress_policy_id` in one place
 * and `b.egress` in another, and only one of them existed.
 *
 * Optional fields are the ones carrying `omitempty` on the Go side, or living
 * inside a `map[string]any` a handler only sometimes fills. A field Go always
 * emits is required here even when its zero value means "nothing happened",
 * because that distinction belongs to the reader and not to the type.
 */

/** A Go `time.Time` on the wire. Its zero value is a real string; see `instant`. */
export type Timestamp = string;

/* -------------------------------------------------------------------------
   internal/builder/manager.go — ClusterStatus
   ------------------------------------------------------------------------- */

export interface ClusterStatus {
  active_builds: number;
  queued_builds: number;
  active_instances: number;
  total_builds: number;
  success_rate: number;
  last_updated: Timestamp;
}

/* -------------------------------------------------------------------------
   internal/catalog — ResolvedBuildContext, as carried on a build
   ------------------------------------------------------------------------- */

export interface ResolvedBuildContext {
  profile_id?: string;
  image_id?: string;
  image_generation?: string;
  /**
   * internal/catalog/catalog.go carries the policy itself here, not its id —
   * `EgressPolicy` with `id` and `mode` — and the digest beside it under its own
   * key. A flat `egress_policy_id` is a field the control plane never emits, so
   * a tile reading it renders nothing and says nothing about why.
   */
  egress_policy?: { id: string; mode: string; channel?: string };
  egress_policy_digest?: string;
  mirror_bundle_id?: string;
  binhost_path?: string;
}

/* -------------------------------------------------------------------------
   internal/builder/manager.go — BuildStatus
   ------------------------------------------------------------------------- */

export interface BuildStatus {
  job_id: string;
  status: string;
  package_name: string;
  version: string;
  arch: string;
  created_at: Timestamp;
  updated_at: Timestamp;
  project_id?: string;
  requested_by?: string;
  instance_id?: string;
  error?: string;
  artifact_path?: string;
  artifact_url?: string;
  artifacts?: string[];
  signed?: boolean;
  signing_key_id?: string;
  resolved_context?: ResolvedBuildContext;
  /** Names the pipeline stage a failed job died in. */
  failed_stage?: string;
  log?: string;
}

/* -------------------------------------------------------------------------
   internal/server/handlers_build.go — the build log document
   ------------------------------------------------------------------------- */

/**
 * One stage's slice of the log, as `buildLogStageSummary` in
 * internal/server/handlers_build.go writes it. `line_count` is what a filter
 * chip counts with, and it is the number the pane below the chip is held to.
 */
export interface BuildLogStage {
  id: string;
  line_count: number;
  started_at?: Timestamp;
  updated_at?: Timestamp;
  last_message?: string;
}

/**
 * The log document, which is the map literal `handleBuildLogs` encodes and not
 * the stage summary that used to be typed here.
 *
 * The text is `logs`, plural, and it is the only place the log itself lives:
 * reading a differently-spelled field back gives `undefined`, which renders as
 * an empty pane under a chip claiming thousands of lines — the disagreement the
 * log pane now refuses to paint.
 */
export interface BuildLogs {
  job_id: string;
  logs?: string;
  generated_at?: Timestamp;
  bytes?: number;
  truncated?: boolean;
  stages?: BuildLogStage[];
}

/* -------------------------------------------------------------------------
   internal/iac — Instance
   ------------------------------------------------------------------------- */

export interface Instance {
  id: string;
  provider: string;
  status: string;
  ip_address: string;
  public_ip: string;
  private_ip: string;
  arch: string;
  metadata: Record<string, string> | null;
  ssh_user: string;
  builder_endpoint: string;
  last_heartbeat: Timestamp;
  created_at: Timestamp;
  ttl: number;
  last_activity: Timestamp;
  active_tasks: number;
}

/* -------------------------------------------------------------------------
   internal/server/handlers_builder.go — BuilderStatusInfo and the aggregate
   ------------------------------------------------------------------------- */

export interface BuilderStatusInfo {
  id: string;
  endpoint: string;
  architecture: string;
  status: string;
  capacity: number;
  current_load: number;
  enabled: boolean;
  cpu_usage: number;
  memory_usage: number;
  disk_usage: number;
  total_builds: number;
  success_builds: number;
  failed_builds: number;
  accepting_builds: boolean;
  native_job_policy?: string;
}

export interface BuildersStatus {
  /** calculateBuilderStats — a map literal, so every key is always present. */
  stats: {
    total_builders: number;
    online_builders: number;
    offline_builders: number;
    draining_builders: number;
    total_capacity: number;
    total_load: number;
    total_builds: number;
    success_builds: number;
    failed_builds: number;
    success_rate: number;
  };
  /** null when no remote builders are configured: Go returns a nil slice. */
  builders: BuilderStatusInfo[] | null;
}

/* -------------------------------------------------------------------------
   internal/builder/manager.go — GetSchedulerStatus, which is the in-memory
   map with the durable scheduler's SchedulerRuntimeStatus unmarshalled over
   the top of it when PostgreSQL is the authority
   ------------------------------------------------------------------------- */

/*
 * One shape, one declaration.
 *
 * This block used to stop at the four keys the in-memory authority writes, and
 * the thirty the durable one adds were transcribed a second time on the page
 * that reads them. They had already drifted: five names here — `running_leases`,
 * `stale_leases`, `capability_mismatches`, `attempts`, `workers` — are on no
 * struct in internal/builder/manager.go at all, so a tile bound to any of them
 * read `undefined` against a scheduler that was reporting the number under
 * another name. Everything below was read off `SchedulerRuntimeStatus` and the
 * structs it carries.
 *
 * A field is declared optional when the console has to be able to render the
 * answer without it. That is wider than Go's `omitempty` in a few places and
 * costs nothing, because a field that is always sent satisfies an optional
 * declaration; what it buys is that the whole durable half — absent wholesale
 * under the in-memory authority — is a set of readings the page tells apart
 * from zero. `undefined` is "this scheduler has no leases to expire"; `0` is
 * "none expired".
 */

/**
 * One builder the in-memory queue currently has tasks on.
 *
 * `capacity` is the literal 4 GetSchedulerStatus writes for every builder, not
 * a reading off the builder itself. Present under both authorities: the durable
 * status carries no `builders` key, so the merge leaves this one standing.
 */
export interface SchedulerBuilder {
  id: string;
  capacity: number;
  current_load: number;
  enabled: boolean;
  healthy: boolean;
  tasks: string[];
}

/**
 * internal/builder/manager.go — LeaseExpiryStatus.
 *
 * Recovery outcomes, counted. Deliberately identity-free on the Go side, which
 * is why there is nothing here to link a count back to the job it came from.
 */
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
  observed_at: Timestamp;
  source_watermark_present: boolean;
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

/**
 * internal/builder/manager.go — TargetReliabilityStatus.
 *
 * `windows` carries no `omitempty`, so a target with no windows arrives as
 * `null` rather than as an absent key — a different value from `[]` and from
 * `undefined`, and the one a `.length` reads through as a crash.
 */
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
  requested_at?: Timestamp;
  updated_at?: Timestamp;
  completed_at?: Timestamp;
}

/** internal/builder/manager.go — CapacityInstanceStatus */
export interface CapacityInstanceStatus {
  id: string;
  pool_id: string;
  provider: string;
  provider_instance_id: string;
  state: string;
  generation?: number;
  attributes?: Record<string, string>;
  heartbeat_observed_at?: Timestamp;
  drain_requested_at?: Timestamp;
  created_at?: Timestamp;
  updated_at?: Timestamp;
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
  selector?: string[] | null;
  provider_max_slots: number;
  mode: string;
  active_slots: number;
  busy_slots: number;
  backlog: number;
  unschedulable_backlog: number;
  desired_slots: number;
  recommendation: string;
  reason?: string;
  under_target_since?: Timestamp;
  last_changed_at?: Timestamp;
  last_evaluated_at?: Timestamp;
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
  under_target_since?: Timestamp;
  last_changed_at?: Timestamp;
  last_evaluated_at?: Timestamp;
  pools?: SchedulerCapacityPoolStatus[];
  actuator?: CapacityActuatorStatus;
}

/**
 * What `/api/scheduler/status` answers, on both of its authorities.
 *
 * `authority` decides how much of the rest exists: "memory" sends the first
 * four keys and stops, "postgresql" sends SchedulerRuntimeStatus unmarshalled
 * over that map — which keeps `builders`, since the durable status has no such
 * key — and a durable authority that cannot be reached sends `healthy: false`
 * with an `error` and the memory keys only, which is a degraded scheduler and
 * not a failed request, so it renders as a card.
 */
export interface SchedulerStatus {
  /** "memory" or "postgresql". Which one decides how much of the rest exists. */
  authority: string;
  builders: SchedulerBuilder[];
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
  oldest_queued_at?: Timestamp;
  oldest_lease_expires_at?: Timestamp;
  fairness?: SchedulerFairnessStatus;
  worker_scoring?: WorkerScoringStatus;
  target_history?: TargetHistoryStatus;
  autoscaler?: SchedulerAutoscaleStatus;
}

/* -------------------------------------------------------------------------
   internal/persistence/jobs.go — JobLedgerStatus, wrapped by
   internal/server/server.go checkLedgerHealth
   ------------------------------------------------------------------------- */

export interface LedgerReconcileReport {
  checked_at: Timestamp;
  legacy_count: number;
  ledger_count: number;
  missing: number;
  mismatched: number;
  request_mismatch: number;
  extra: number;
  repaired: number;
  consistent: boolean;
  error?: string;
}

export interface LedgerStatus {
  enabled: boolean;
  ok: boolean;
  /** Absent when the ledger is disabled: that branch returns two keys only. */
  projection_stale?: boolean;
  authority?: string;
  writes?: number;
  write_errors?: number;
  projection_errors?: number;
  last_write_at?: Timestamp;
  last_projection_at?: Timestamp;
  last_error?: string;
  /**
   * The most recent write failure, retained. `last_error` above is cleared by
   * the next successful write, so on a busy ledger it is empty whatever
   * `write_errors` says — which left this card reporting a count nobody could
   * act on. These two carry the diagnosis for as long as the number does.
   */
  last_write_error?: string;
  last_write_error_at?: Timestamp;
  last_reconcile?: LedgerReconcileReport;
}

/* -------------------------------------------------------------------------
   internal/persistence/runtime_metadata.go — RuntimeMetadataStatus
   ------------------------------------------------------------------------- */

export interface RuntimeMetadataStatus {
  live_infra: number;
  cleanup_failed_infra: number;
  published_artifacts: number;
  staged_artifacts: number;
  factory_runs: number;
  missing_artifacts: number;
  corrupt_artifacts: number;
  orphaned_artifacts: number;
  last_metadata_update_at?: Timestamp;
}

/**
 * internal/server/handlers_runtime_metadata.go — what the route actually
 * answers, on all three of its branches.
 *
 * The handler never writes the document above on its own: every branch encodes
 * `{enabled, ok}` and hangs the document off `status`. Typing the call as the
 * document meant a caller read `.live_infra` off the envelope and got
 * `undefined` — a zero-looking reading for a ledger that had answered.
 *
 * `status` is absent on both failure branches, which is what makes "the ledger
 * could not be asked" different from "the ledger answered and six artifacts are
 * missing". Both of those come back 503, so the difference is not in the status
 * line either.
 */
export interface RuntimeMetadataEnvelope {
  enabled: boolean;
  ok: boolean;
  error?: string;
  status?: RuntimeMetadataStatus;
}

/* -------------------------------------------------------------------------
   internal/runtimecache/redis.go — Health, plus the disabled-cache branch in
   internal/server/handlers_cache.go
   ------------------------------------------------------------------------- */

export interface CacheStatus {
  enabled: boolean;
  ok: boolean;
  last_error?: string;
  last_success_at?: Timestamp;
  control_plane_presence?: number;
  wake_supervised?: boolean;
  wake_subscribed?: boolean;
  wake_error?: string;
  /** Only the no-client branch sends these two. */
  error?: string;
  correctness_fallback?: string;
}

/* -------------------------------------------------------------------------
   internal/server/server.go — handleWorkerGatewayStatus
   ------------------------------------------------------------------------- */

export interface WorkerGatewayStatus {
  enabled: boolean;
  authority: string;
  listener_port: number;
  transport: string;
  registered_sessions: number;
  connected_sessions: number;
  pending_tasks: number;
  pending_uploads: number;
  active_issuers: number;
  draining_issuers: number;
  revoked_issuers: number;
  active_certificates: number;
  revoked_certificates: number;
  expiring_certificates: number;
  issuer_id: string;
  issuer_provider: string;
  issuer_healthy: boolean;
  issuer_runtime: {
    healthy?: boolean;
    consecutive_failures?: number;
    last_success_at?: Timestamp;
    last_error?: string;
  };
  inbound_builder_api: boolean;
  executor_protocol: number;
  certificate_ttl_min: number;
  phase_executor_mode: string;
  /** Present only when the job ledger answered; the error replaces it. */
  phase_work?: {
    shadow: number;
    active: number;
    ready: number;
    unschedulable: number;
    claimed: number;
    blocked: number;
    failed: number;
  };
  phase_work_error?: string;
}

/* -------------------------------------------------------------------------
   internal/workergateway/cert.go — IssuerGenerationStatus and
   CertificateStatus, served unchanged by handleWorkloadIdentityInventory in
   internal/server/handlers_workload_identity.go
   ------------------------------------------------------------------------- */

/**
 * One issuer generation, keyed by its certificate fingerprint.
 *
 * There is no `id` and no `status` on this record. Reading either back gives
 * `undefined`, and `undefined.slice(0, 12)` — which is what a fingerprint
 * abbreviation is — throws inside the render, taking the whole of /monitor with
 * it on any deployment that has ever issued a workload certificate.
 */
export interface WorkloadIssuer {
  fingerprint: string;
  issuer_id: string;
  provider: string;
  subject: string;
  serial: string;
  not_before: Timestamp;
  not_after: Timestamp;
  state: string;
  last_issued_at: Timestamp;
  active_certificates: number;
  revoked_at?: Timestamp;
  revoke_reason?: string;
}

/** The leaf, bound to the attempt it was minted for. `state`, never `status`. */
export interface WorkloadCertificate {
  fingerprint: string;
  serial: string;
  issuer_fingerprint: string;
  worker_id: string;
  job_id: string;
  attempt_id: string;
  attempt_fence: number;
  not_before: Timestamp;
  not_after: Timestamp;
  state: string;
  issued_at: Timestamp;
  last_seen_at?: Timestamp;
  revoked_at?: Timestamp;
  revoke_reason?: string;
}

export interface WorkloadIdentityInventory {
  issuers: WorkloadIssuer[] | null;
  certificates: WorkloadCertificate[] | null;
  certificate_limit: number;
}

/* -------------------------------------------------------------------------
   internal/server/handlers_gpg.go
   ------------------------------------------------------------------------- */

export interface GPGStatus {
  enabled: boolean;
  ready: boolean;
  key_id: string;
  mode: string;
  private_key_here: boolean;
  /**
   * internal/signing/types.go — RuntimeStatus, verbatim. The queue counts five
   * states and names none of them `pending` or `signed`; those two were never on
   * the wire, so a counter bound to them reads zero on a queue that is backed up.
   */
  queue?: {
    queued: number;
    claimed: number;
    completed: number;
    failed: number;
    canceled: number;
    oldest_queued_at?: Timestamp;
    oldest_claimed_at?: Timestamp;
  };
  queue_error?: string;
}

/* -------------------------------------------------------------------------
   internal/dashboard/dashboard.go — handlePublicKeyAPI
   ------------------------------------------------------------------------- */

export interface PublicKey {
  public_key: string;
}

/* -------------------------------------------------------------------------
   internal/server/handlers_public.go — publicServiceStatus
   ------------------------------------------------------------------------- */

export interface PublicComponentStatus {
  name: string;
  status: string;
}

export interface PublicServiceStatus {
  status: string;
  version: string;
  updated_at: Timestamp;
  components: PublicComponentStatus[];
}

/* -------------------------------------------------------------------------
   internal/server/handlers_binhost.go — binhostProfile
   ------------------------------------------------------------------------- */

export interface BinhostProfile {
  profile_id: string;
  arch: string;
  profile_path: string;
  binhost_path: string;
  channel?: string;
  default: boolean;
  sync_path: string;
}

export interface BinhostInventory {
  binhosts: BinhostProfile[];
}

/* -------------------------------------------------------------------------
   internal/server/handlers_binhost.go — publicPackage
   ------------------------------------------------------------------------- */

export interface PublicPackage {
  name: string;
  version: string;
  arch: string;
  /** `omitempty`, so a package built with no USE flags carries no key at all. */
  use_flags?: string[];
  profile_id: string;
  profile_path: string;
  channel?: string;
  binhost_path: string;
  download_path: string;
  /**
   * Whether the artifact behind `download_path` is signed, and by which key.
   *
   * Both are optional because `publicPackage` in handlers_binhost.go does not
   * emit them today: the signing state lives on the build record
   * (`BuildStatus.signed`, `BuildStatus.signing_key_id`) and on the isolated
   * signer's queue, and neither is joined into the public catalogue. They are
   * declared here in the spelling the build record already uses so that adding
   * the two fields server-side lights the column up rather than needing a
   * second shape. Until then the packages page says the catalogue does not
   * report it — it never infers "signed" from the deployment having a key.
   */
  signed?: boolean;
  signing_key_id?: string;
}

export interface PublicPackageList {
  packages: PublicPackage[];
  total: number;
  limit: number;
  offset: number;
}

/* -------------------------------------------------------------------------
   internal/persistence/iam.go — ProjectPolicy
   ------------------------------------------------------------------------- */

export interface ProjectPolicy {
  project_id: string;
  suspended: boolean;
  priority_weight: number;
  starvation_threshold_seconds: number;
  max_queued_jobs: number;
  max_active_jobs: number;
  max_daily_submissions: number;
  max_active_vcpus: number;
  max_active_memory_mib: number;
  max_active_disk_gib: number;
  max_artifact_bytes_per_job: number;
  max_daily_build_seconds: number;
  max_daily_cloud_cost_microunits: number;
  max_failures_per_hour: number;
  abuse_cooldown_seconds: number;
  abuse_suspended: boolean;
  abuse_suspended_until?: Timestamp;
  abuse_reason?: string;
  abuse_generation: number;
  max_claimed_attempts: number;
  max_provision_attempts: number;
  max_build_attempts: number;
  max_verify_attempts: number;
  max_publish_attempts: number;
  queued_jobs: number;
  active_jobs: number;
  submissions_today: number;
  reserved_vcpus: number;
  reserved_memory_mib: number;
  reserved_disk_gib: number;
  quarantine_bytes: number;
  active_artifact_budgets: number;
  build_seconds_today: number;
  cloud_cost_microunits_today: number;
  active_runtime_budgets: number;
  failures_last_hour: number;
  claimed_reservations: number;
  provision_reservations: number;
  build_reservations: number;
  verify_reservations: number;
  publish_reservations: number;
  waiting_reservations: number;
  phase_work_shadow: number;
  phase_work_active: number;
  phase_work_blocked: number;
  phase_work_ready: number;
  phase_work_unschedulable: number;
  phase_work_claimed: number;
  phase_work_failed: number;
  submission_day_starts_at: Timestamp;
  submission_day_ends_at: Timestamp;
  version: number;
  updated_by: string;
  updated_at: Timestamp;
}

/* -------------------------------------------------------------------------
   pkg/config — CloudSettings, wrapped by internal/server/handlers_settings.go
   cloudSettingsResponse
   ------------------------------------------------------------------------- */

export interface CloudSettings {
  provider: string;
  remote_builders?: string[];
  gcp_project: string;
  gcp_region: string;
  gcp_zone: string;
  gcp_key_file: string;
  aws_region: string;
  aws_zone: string;
  aws_access_key: string;
  /** Never returned by GET: the response redacts it and sets has_aws_secret_key. */
  aws_secret_key?: string;
  pve_endpoint: string;
  pve_node: string;
  pve_nodes?: string[];
  pve_token_id: string;
  pve_token_secret?: string;
  pve_username: string;
  pve_password?: string;
  pve_insecure: boolean;
  pve_storage: string;
  pve_network: string;
  pve_template: string;
  pve_cicustom: string;
  pve_nameserver: string;
  ssh_key_path: string;
  ssh_user: string;
  ssh_known_hosts?: string;
  ssh_insecure_host_key: boolean;
  gentoo_mirror: string;
  portage_sync_uri: string;
  portage_sync_method: string;
  make_conf_extra: string;
  build_features: string;
  build_mode: string;
  server_callback_url: string;
  builder_binary_path?: string;
  builder_binary_url?: string;
  builder_binary_sha256?: string;
  instance_ttl_minutes: number;
  skip_verify_install: boolean;
  upload_url: string;
  upload_user: string;
  upload_password?: string;
  upload_dir: string;
}

export interface CloudSettingsResponse extends CloudSettings {
  /**
   * Whether a secret is already stored. An empty secret on PUT means "keep the
   * stored one", which is the whole reason a write in flight must never be
   * replayed: the replay carries the cleared fields.
   */
  has_pve_token_secret: boolean;
  has_pve_password: boolean;
  has_aws_secret_key: boolean;
  has_upload_password: boolean;
  secret_values_managed_externally: boolean;
}

/*
 * handleCloudSettingsTest's answer is declared beside its only caller, in
 * src/pages/settings/api.ts: the request takes the form as its body, which the
 * shared module has no way to express, and the node rows are
 * internal/iac/pve_scheduler.go PVENodeInfo — `free_mem_gb`, `cpu_load`,
 * `has_template` — and not the `{cpu, maxcpu, mem, maxmem}` that used to be
 * transcribed here from nothing.
 */

/* -------------------------------------------------------------------------
   internal/imagefactory — FactoryStatus, wrapped by
   internal/server/handlers_image_factory.go imageFactoryStatusResponse
   ------------------------------------------------------------------------- */

export interface FactoryEvidence {
  label: string;
  digest?: string;
  path?: string;
  recorded_at?: Timestamp;
  size_bytes?: number;
}

export interface FactoryStep {
  id: string;
  title: string;
  state: string;
  summary?: string;
  started_at?: Timestamp;
  completed_at?: Timestamp;
  log?: FactoryEvidence;
}

export interface FactoryMilestone {
  id: string;
  title: string;
  state: string;
  summary?: string;
  completed_at?: Timestamp;
  evidence?: FactoryEvidence[];
  steps?: FactoryStep[];
}

export interface FactoryBlocker {
  code: string;
  summary: string;
  action?: string;
}

export interface FactoryDesktopE2E {
  state: string;
  strategy?: string;
  ai_policy?: string;
  runner?: string;
  display?: string;
  artifacts?: string;
}

export interface FactoryStatus {
  schema_version: number;
  updated_at: Timestamp;
  overall_state: string;
  milestones: FactoryMilestone[];
  blockers?: FactoryBlocker[];
  desktop_e2e: FactoryDesktopE2E;
}

export interface ImageFactoryProfileView {
  id: string;
  arch: string;
  profile_path: string;
  binhost_path: string;
  image_id: string;
  egress_policy_id: string;
  channel: string;
  default: boolean;
  package_sets?: string[];
}

export interface ImageFactoryStatus {
  configured: boolean;
  catalog: {
    version: number;
    profiles: ImageFactoryProfileView[];
    images: { id?: string; generation?: string; digest?: string; size_bytes?: number }[];
    mirror_bundles: { id?: string; sync_uri?: string }[];
    egress_policies: { id?: string; description?: string }[];
  };
  status?: FactoryStatus;
  message?: string;
}

/* -------------------------------------------------------------------------
   internal/iam — Principal, ProjectAccess; internal/server/iam.go handleIAMMe
   ------------------------------------------------------------------------- */

export interface Principal {
  issuer: string;
  subject: string;
  authentication: string;
  system_admin: boolean;
  step_up: boolean;
  provider_id?: string;
  subject_id?: string;
  preferred_username?: string;
  display_name?: string;
  email?: string;
  session_id?: string;
  token_issued_at?: Timestamp;
  token_expires_at?: Timestamp;
  authenticated_at?: Timestamp;
  acr?: string;
  amr?: string[];
}

export interface ProjectAccess {
  project_id: string;
  project_name: string;
  role: string;
}

export interface IdentityProvider {
  id: string;
  type: string;
  display_name: string;
  backchannel_logout_enabled: boolean;
  capabilities?: Record<string, unknown>;
}

export interface IAMMe {
  principal: Principal;
  projects: ProjectAccess[] | null;
  auth_mode: string;
  identity_providers: IdentityProvider[];
}

/**
 * internal/persistence/iam_sessions.go — IAMSession.
 *
 * `id` and `issued_at`, not `session_id` and `created_at`, and the device-flow
 * lineage (`kind`, `derived_from_session_id`) is on the record too. There is no
 * `user_agent` and no `ip_address` on this row at all: a panel reading either
 * would print an empty cell for a fact the control plane has never carried.
 */
export interface IAMSession {
  id: string;
  kind: string;
  derived_from_session_id?: string;
  subject_id: string;
  provider_session_id?: string;
  provider_token_id?: string;
  issued_at: Timestamp;
  authenticated_at?: Timestamp;
  expires_at: Timestamp;
  last_seen_at: Timestamp;
  acr?: string;
  amr?: string[];
  revoked_at?: Timestamp;
  revoke_reason?: string;
}

export interface IAMSessions {
  sessions: IAMSession[] | null;
  current_session_id: string;
}

/* -------------------------------------------------------------------------
   Small write responses, each a one-key map literal in Go
   ------------------------------------------------------------------------- */

/** internal/server/handlers_build.go — handleBuildsCleanupFailed */
export interface CleanupFailed {
  removed: number;
}

/** internal/server/handlers_build.go — handleSubmitBuildWithConfig */
export interface BuildSubmitted {
  job_id: string;
  status: string;
  message?: string;
}

/** internal/server/iam.go — handleIAMDeviceDecision */
export interface DeviceDecision {
  status: string;
}

/**
 * internal/server/iam.go — handleIAMRevokeAllSessions.
 *
 * Two keys, and the count is `revoked_sessions`. `subject_id` is echoed back
 * because the request may name someone other than the caller, and the answer to
 * "whose sessions did I just end" is not something a panel should infer.
 */
export interface SessionsRevoked {
  subject_id: string;
  revoked_sessions: number;
}

/** internal/dashboard/dashboard.go — handleLanguagePreference */
export interface LanguagePreference {
  lang: string;
}

/** internal/dashboard/dashboard.go — handleLocalStepUp */
export interface StepUpEstablished {
  step_up: boolean;
  expires_at: Timestamp;
}

/**
 * internal/dashboard/dashboard.go — handleShellPreflight.
 *
 * The satisfied branch is 200 with `{step_up, method}`; the refusal is 428 with
 * `{error, code, method}`, and the code distinguishes "authenticate again" from
 * "this deployment cannot". The client turns that into a `step-up` outcome
 * rather than a generic error, because the two need different words and only
 * one of them is worth offering a retry for.
 */
export interface ShellPreflight {
  step_up: boolean;
  method: StepUpMethod;
}

/** Which credential would satisfy a step-up, from the server's own vocabulary. */
export type StepUpMethod = 'local' | 'federated' | 'unavailable';

/* -------------------------------------------------------------------------
   Streams
   ------------------------------------------------------------------------- */

/** internal/server/handlers_cache.go — the `ready` event's data. */
export interface JobEventsReady {
  transport: string;
  project_id: string;
}

/** The `job` event's data: the cache payload, which carries the job id. */
export interface JobEvent {
  job_id: string;
  status?: string;
}
