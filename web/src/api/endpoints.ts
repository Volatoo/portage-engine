/**
 * Every endpoint the console calls, named once.
 *
 * A path is spelled here and nowhere else. In the console this replaces the same
 * URL appeared in three page scripts with three different query strings, and one
 * of them had been left pointing at a route that no longer existed — which is
 * invisible until someone opens that page.
 *
 * Each function returns an `ApiOutcome`, so a step-up refusal and a 401 stay
 * distinguishable from a failure all the way to the component that has to say
 * something about them.
 */

import { request } from './client';
import type { ApiOutcome, RequestOptions } from './client';
import type {
  BinhostInventory,
  BuildLogs,
  BuildStatus,
  BuildSubmitted,
  BuildersStatus,
  CacheStatus,
  CleanupFailed,
  CloudSettings,
  CloudSettingsResponse,
  ClusterStatus,
  DeviceDecision,
  GPGStatus,
  IAMMe,
  IAMSessions,
  ImageFactoryStatus,
  Instance,
  LanguagePreference,
  LedgerStatus,
  ProjectPolicy,
  PublicKey,
  PublicPackageList,
  PublicServiceStatus,
  RuntimeMetadataEnvelope,
  SchedulerStatus,
  SessionsRevoked,
  ShellPreflight,
  StepUpEstablished,
  WorkerGatewayStatus,
  WorkloadIdentityInventory,
} from './types';

/** The request context a page carries: which project, and how to cancel. */
export interface Scope {
  projectID?: string;
  signal?: AbortSignal;
}

/**
 * A context with no project in it, and no way to acquire one.
 *
 * For the routes that are answered outside every project. Spelling it as a type
 * rather than as a convention is what keeps a caller of one of those routes from
 * quietly starting to send a selector — and, on `/api/iam/me`, what keeps the
 * two callers of one path apart.
 */
export type Unscoped = Omit<Scope, 'projectID'>;

/**
 * A context that names a project, required rather than optional.
 *
 * For a surface that has already been handed one: `api.thing({})` on a route
 * declared this way does not compile, so the scope cannot be dropped by
 * shortening the call.
 */
export type ProjectScoped = Scope & { projectID: string };

function scoped(scope: Scope, extra: RequestOptions = {}): RequestOptions {
  return {
    ...extra,
    ...(scope.projectID === undefined ? {} : { projectID: scope.projectID }),
    ...(scope.signal === undefined ? {} : { signal: scope.signal }),
  };
}

/* ---- the console's own surfaces ---------------------------------------- */

export const api = {
  /** internal/dashboard/dashboard.go handleStatus -> /api/v1/cluster/status */
  clusterStatus: (scope: Scope = {}): Promise<ApiOutcome<ClusterStatus>> =>
    request('/api/status', scoped(scope)),

  /** A bare array, newest first. `limit` is clamped to 200 server-side. */
  builds: (limit: number, scope: Scope = {}): Promise<ApiOutcome<BuildStatus[]>> =>
    request(`/api/builds?limit=${String(limit)}`, scoped(scope)),

  buildDetail: (jobID: string, scope: Scope = {}): Promise<ApiOutcome<BuildStatus>> =>
    request(`/api/builds/detail?job_id=${encodeURIComponent(jobID)}`, scoped(scope)),

  buildLogs: (jobID: string, scope: Scope = {}): Promise<ApiOutcome<BuildLogs>> =>
    request(`/api/builds/logs?job_id=${encodeURIComponent(jobID)}`, scoped(scope)),

  cleanupFailedBuilds: (scope: Scope = {}): Promise<ApiOutcome<CleanupFailed>> =>
    request('/api/builds/cleanup-failed', scoped(scope, { method: 'POST' })),

  /**
   * The three writes a single job takes. Each answers with a one-key map the
   * console does not read — the list and the detail are re-fetched afterwards,
   * because the control plane, not the answer to the write, is what says what
   * state the job ended up in.
   *
   * DELETE for the delete, POST for the two that queue work: the dashboard
   * proxies each straight through to /api/v1/builds/... with the same method.
   */
  cancelBuild: (jobID: string, scope: Scope = {}): Promise<ApiOutcome<unknown>> =>
    request(
      `/api/builds/cancel?job_id=${encodeURIComponent(jobID)}`,
      scoped(scope, { method: 'POST' }),
    ),

  retryBuild: (jobID: string, scope: Scope = {}): Promise<ApiOutcome<unknown>> =>
    request(
      `/api/builds/retry?job_id=${encodeURIComponent(jobID)}`,
      scoped(scope, { method: 'POST' }),
    ),

  deleteBuild: (jobID: string, scope: Scope = {}): Promise<ApiOutcome<unknown>> =>
    request(
      `/api/builds/delete?job_id=${encodeURIComponent(jobID)}`,
      scoped(scope, { method: 'DELETE' }),
    ),

  submitBuild: (body: unknown, scope: Scope = {}): Promise<ApiOutcome<BuildSubmitted>> =>
    request('/api/builds/submit', scoped(scope, { method: 'POST', body })),

  instances: (scope: Scope = {}): Promise<ApiOutcome<Instance[]>> =>
    request('/api/instances', scoped(scope)),

  /* ---- monitor rollups ------------------------------------------------- */

  buildersStatus: (scope: Scope = {}): Promise<ApiOutcome<BuildersStatus>> =>
    request('/api/builders/status', scoped(scope)),

  schedulerStatus: (scope: Scope = {}): Promise<ApiOutcome<SchedulerStatus>> =>
    request('/api/scheduler/status', scoped(scope)),

  ledgerStatus: (scope: Scope = {}): Promise<ApiOutcome<LedgerStatus>> =>
    request('/api/ledger/status', scoped(scope)),

  /**
   * The envelope, not the document inside it.
   *
   * handleRuntimeMetadataStatus answers `{enabled, ok, status?, error?}` on
   * every branch it has, and two of the three carry no document at all — a
   * ledger that is switched off and a ledger that could not be reached. A
   * caller typed on the document reads `.live_infra` off the envelope, gets
   * `undefined`, and renders it as a zero.
   */
  runtimeMetadataStatus: (scope: Scope = {}): Promise<ApiOutcome<RuntimeMetadataEnvelope>> =>
    request('/api/runtime-metadata/status', scoped(scope)),

  cacheStatus: (scope: Scope = {}): Promise<ApiOutcome<CacheStatus>> =>
    request('/api/cache/status', scoped(scope)),

  workerGatewayStatus: (scope: Scope = {}): Promise<ApiOutcome<WorkerGatewayStatus>> =>
    request('/api/worker-gateway/status', scoped(scope)),

  workerGatewayIdentities: (scope: Scope = {}): Promise<ApiOutcome<WorkloadIdentityInventory>> =>
    request('/api/worker-gateway/identities', scoped(scope)),

  gpgStatus: (scope: Scope = {}): Promise<ApiOutcome<GPGStatus>> =>
    request('/api/gpg/status', scoped(scope)),

  imageFactoryStatus: (scope: Scope = {}): Promise<ApiOutcome<ImageFactoryStatus>> =>
    request('/api/image-factory/status', scoped(scope)),

  /* ---- identity -------------------------------------------------------- */

  /**
   * Who the reader is, asked outside every project.
   *
   * The only source of a capability. The boot payload carries identity and never
   * authority, so a gated destination stays hidden until this answers — an IAM
   * outage leaves it hidden rather than revealing it and taking it back.
   *
   * Unscoped, and typed so it cannot become otherwise: this is the question
   * whose answer NAMES the projects, so at the moment it is asked there is no
   * project to ask it in, and a selector on it would name one the reader may not
   * hold. Every caller that asks it for that reason — the chrome, the build
   * pages gating a destructive control, the device-approval page — comes here.
   */
  iamMe: (scope: Unscoped = {}): Promise<ApiOutcome<IAMMe>> =>
    request('/api/iam/me', scoped(scope)),

  /**
   * The same route, asked by a surface that has already been given a project.
   *
   * One path, two callers, two different right answers — and until they were
   * separated the difference lived in an argument list, where dropping it left
   * the request looking exactly like the identity ask above. It is a separate
   * entry point so the two are told apart in the source rather than on the wire,
   * where they are identical, and it requires the project rather than defaulting
   * it so that shortening the call is a compile error instead of a silent
   * unscoped request.
   */
  iamMeInProject: (scope: ProjectScoped): Promise<ApiOutcome<IAMMe>> =>
    request('/api/iam/me', scoped(scope)),

  iamSessions: (scope: Scope = {}): Promise<ApiOutcome<IAMSessions>> =>
    request('/api/iam/sessions', scoped(scope)),

  /**
   * Ends every session the caller holds.
   *
   * The empty object is the payload, not the absence of one: handleIAMRevokeAllSessions
   * in internal/server/iam.go decodes the body unconditionally, and `Decode` on
   * a request with no body answers io.EOF — which that handler reports as
   * "invalid session revocation request" and a 400. So the control never worked
   * at all, and the refusal named a request nobody had made.
   *
   * `{}` and nothing more: the decoder disallows unknown fields, an empty
   * `subject_id` already means the caller, and an empty `reason` already means
   * `revoke_all_requested`.
   */
  revokeAllSessions: (scope: Scope = {}): Promise<ApiOutcome<SessionsRevoked>> =>
    request('/api/iam/sessions/revoke-all', scoped(scope, { method: 'POST', body: {} })),

  /** The device page's approve/deny. `decision` is the server's own vocabulary. */
  deviceDecision: (
    userCode: string,
    decision: 'approve' | 'deny',
    scope: Scope = {},
  ): Promise<ApiOutcome<DeviceDecision>> =>
    request(
      '/api/iam/device/decision',
      scoped(scope, { method: 'POST', body: { user_code: userCode, decision } }),
    ),

  /**
   * Establishes a fresh local step-up credential. Called in answer to a
   * `step-up` outcome whose method is `local`, never speculatively.
   */
  stepUp: (username: string, password: string): Promise<ApiOutcome<StepUpEstablished>> =>
    request('/auth/step-up', { method: 'POST', body: { username, password } }),

  /**
   * Asks whether the shell may be opened, over plain HTTP.
   *
   * The WebSocket handshake is the reason this exists: a control plane that
   * refuses it reaches page script as an untyped error event with no status and
   * no body. Asking here first turns the refusal into a `step-up` outcome that
   * names the credential that would satisfy it.
   */
  shellPreflight: (scope: Scope = {}): Promise<ApiOutcome<ShellPreflight>> =>
    request('/api/shell/preflight', scoped(scope)),

  /* ---- project policy and settings ------------------------------------- */

  projectPolicy: (scope: Scope = {}): Promise<ApiOutcome<ProjectPolicy>> =>
    request('/api/projects/policy', scoped(scope)),

  cloudSettings: (scope: Scope = {}): Promise<ApiOutcome<CloudSettingsResponse>> =>
    request('/api/settings/cloud', scoped(scope)),

  /**
   * The one writer of the settings resource. Route every activation through
   * `singleWriter` — an empty secret field means "keep the stored one", so a
   * replayed submission wipes credentials rather than repeating a write.
   */
  saveCloudSettings: (
    body: Partial<CloudSettings>,
    scope: Scope = {},
  ): Promise<ApiOutcome<CloudSettingsResponse>> =>
    request('/api/settings/cloud', scoped(scope, { method: 'PUT', body })),

  /*
   * There is deliberately no settings-test here. handleCloudSettingsTest reads
   * the form out of the request body and falls back to the stored values only
   * for the fields the body omits, so a bodyless POST is `invalid JSON body:
   * EOF` and a 400 rather than "test what is saved" — and this module has no
   * way to carry the form. It lives in src/pages/settings/api.ts, beside the
   * form it tests.
   */

  /* ---- public surfaces ------------------------------------------------- */

  publicStatus: (scope: Scope = {}): Promise<ApiOutcome<PublicServiceStatus>> =>
    request('/api/public/status', scoped(scope)),

  publicBinhosts: (scope: Scope = {}): Promise<ApiOutcome<BinhostInventory>> =>
    request('/api/public/binhosts', scoped(scope)),

  /**
   * The public catalogue, paged.
   *
   * `limit` and `offset` are always sent because the handler clamps them
   * server-side (1..200, 0..1000000) and answers 400 rather than silently
   * correcting — so the page states the window it is asking for instead of
   * inheriting a default it does not know. An empty `q` or `profileID` is
   * omitted from the query string, since handlePublicPackages treats a present
   * `profile_id` it does not recognise as a 400 and an empty one as "all".
   */
  publicPackages: (
    filter: { q?: string; profileID?: string; limit: number; offset: number },
    scope: Scope = {},
  ): Promise<ApiOutcome<PublicPackageList>> => {
    const params = new URLSearchParams();
    if (filter.q !== undefined && filter.q !== '') {
      params.set('q', filter.q);
    }
    if (filter.profileID !== undefined && filter.profileID !== '') {
      params.set('profile_id', filter.profileID);
    }
    params.set('limit', String(filter.limit));
    params.set('offset', String(filter.offset));
    return request(`/api/public/packages?${params.toString()}`, scoped(scope));
  },

  publicKey: (): Promise<ApiOutcome<PublicKey>> => request('/api/keys/public'),

  /**
   * Persists the language choice as a cookie, so the NEXT navigation arrives
   * already translated. Without it the server renders the served language again
   * and the flash the whole boot payload exists to prevent comes back.
   */
  setLanguage: (lang: string): Promise<ApiOutcome<LanguagePreference>> =>
    request('/api/preferences/language', { method: 'POST', body: { lang } }),
} as const;

/** Every path this module can reach, for the test that checks the surface. */
export const ENDPOINT_PATHS = [
  '/api/builders/status',
  '/api/builds',
  '/api/builds/cancel',
  '/api/builds/cleanup-failed',
  '/api/builds/delete',
  '/api/builds/detail',
  '/api/builds/logs',
  '/api/builds/retry',
  '/api/builds/submit',
  '/api/cache/status',
  '/api/events/jobs',
  '/api/gpg/status',
  '/api/iam/device/decision',
  '/api/iam/me',
  '/api/iam/sessions',
  '/api/iam/sessions/revoke-all',
  '/api/image-factory/status',
  '/api/instances',
  '/api/keys/public',
  '/api/ledger/status',
  '/api/preferences/language',
  '/api/projects/policy',
  '/api/public/binhosts',
  '/api/public/packages',
  '/api/public/status',
  '/api/runtime-metadata/status',
  '/api/scheduler/status',
  '/api/settings/cloud',
  '/api/settings/cloud/test',
  '/api/shell',
  '/api/shell/preflight',
  '/api/status',
  '/api/worker-gateway/identities',
  '/api/worker-gateway/status',
  '/auth/step-up',
] as const;
