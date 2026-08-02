/**
 * The two requests this page needs that `src/api/endpoints.ts` cannot express
 * yet. Both are listed as handoffs; neither is a second spelling of a path the
 * shared module already reaches correctly.
 *
 *  1. `POST /api/settings/cloud/test` takes the form as its body.
 *     internal/server/handlers_settings.go handleCloudSettingsTest decodes the
 *     request body and falls back to the stored values only for the fields the
 *     body omits, so a test with no body is not "test what is saved" — it is
 *     `invalid JSON body: EOF` and a 400. The shared helper sends no body.
 *     Its node rows are declared here too: `CloudSettingsTest` in
 *     src/api/types.ts describes `{cpu, maxcpu, mem, maxmem}`, and
 *     internal/iac/pve_scheduler.go PVENodeInfo has never carried those names.
 *
 *  2. `DELETE /api/iam/sessions?session_id=…` revokes one session. The shared
 *     module carries the list and the revoke-all, and nothing between them.
 *
 * Everything else on this page goes through `api`, including the settings GET
 * and PUT, the build submit and detail polls, the GPG status and the public
 * key.
 */

import { request } from '../../api/client';
import type { ApiOutcome } from '../../api/client';
import type { CloudSettings } from '../../api/types';

/**
 * The sentence a refusal carries.
 *
 * A 401 and a step-up refusal have no `message` on the transport's result type
 * and the reader still has to be told something other than silence, so the
 * outcome's own name is the floor. Nothing on this page swallows an outcome.
 */
export function failureMessage(outcome: ApiOutcome<unknown>): string {
  return 'message' in outcome ? outcome.message : outcome.kind;
}

/** internal/iac/pve_scheduler.go — PVENodeInfo. */
export interface PVENodeRow {
  node: string;
  status: string;
  free_mem_gb: number;
  cpu_load: number;
  has_template: boolean;
}

/** internal/server/handlers_settings.go — handleCloudSettingsTest's response. */
export interface CloudSettingsTestResult {
  ok: boolean;
  error?: string;
  nodes?: PVENodeRow[];
}

export function testCloudSettings(
  body: Partial<CloudSettings>,
  scope: { projectID?: string } = {},
): Promise<ApiOutcome<CloudSettingsTestResult>> {
  return request('/api/settings/cloud/test', {
    method: 'POST',
    body,
    ...(scope.projectID === undefined ? {} : { projectID: scope.projectID }),
  });
}

/** internal/server/iam.go — handleIAMSessions, the DELETE arm. */
export function revokeSession(
  sessionID: string,
  scope: { projectID?: string } = {},
): Promise<ApiOutcome<unknown>> {
  return request(`/api/iam/sessions?session_id=${encodeURIComponent(sessionID)}`, {
    method: 'DELETE',
    ...(scope.projectID === undefined ? {} : { projectID: scope.projectID }),
  });
}

/**
 * One federated session, as internal/persistence/iam_sessions.go writes it.
 *
 * `IAMSession` in src/api/types.ts names `session_id` and `created_at`, which
 * the Go struct does not have — it is `id` and `issued_at`, and `acr`/`amr`
 * are absent from the shared type altogether. Rather than read four wrong field
 * names, the rows are normalised here out of `unknown`: a payload missing a
 * field yields the empty string, which the renderer prints as "-", instead of
 * `undefined` reaching a formatter.
 */
export interface SessionRow {
  id: string;
  issued_at: string;
  last_seen_at: string;
  expires_at: string;
  acr: string;
  amr: readonly string[];
  revoked: boolean;
}

function stringAt(source: Record<string, unknown>, key: string): string {
  const value = source[key];
  return typeof value === 'string' ? value : '';
}

export function readSession(raw: unknown): SessionRow {
  if (typeof raw !== 'object' || raw === null) {
    return {
      id: '',
      issued_at: '',
      last_seen_at: '',
      expires_at: '',
      acr: '',
      amr: [],
      revoked: false,
    };
  }
  const source = raw as Record<string, unknown>;
  const amr = source['amr'];
  return {
    id: stringAt(source, 'id'),
    issued_at: stringAt(source, 'issued_at'),
    last_seen_at: stringAt(source, 'last_seen_at'),
    expires_at: stringAt(source, 'expires_at'),
    acr: stringAt(source, 'acr'),
    amr: Array.isArray(amr)
      ? amr.filter((entry): entry is string => typeof entry === 'string')
      : [],
    // A revocation is a timestamp on the Go side and the presence of it is the
    // whole state, so it is read as one rather than kept as a clock reading
    // nothing prints.
    revoked: stringAt(source, 'revoked_at') !== '',
  };
}

/**
 * A count off the signing queue.
 *
 * internal/signing/types.go RuntimeStatus is `{queued, claimed, completed,
 * failed, canceled}`; `GPGStatus.queue` in src/api/types.ts declares
 * `{pending, signed, failed}`, two of which have never been on the wire. Read
 * through `unknown` so a wrong declaration cannot make this page render
 * `undefined` where a number belongs.
 */
export function queueCount(queue: unknown, key: 'queued' | 'claimed' | 'failed'): number {
  if (typeof queue !== 'object' || queue === null) {
    return 0;
  }
  const value = (queue as Record<string, unknown>)[key];
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}
