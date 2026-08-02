import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { afterEach, describe, expect, it, vi } from 'vitest';

import { request } from './client';
import { api, ENDPOINT_PATHS } from './endpoints';
import { resolveState } from './state';
import { subscribeToJobEvents } from './stream';
import type { GPGStatus, IAMSessions, SessionsRevoked, WorkloadIdentityInventory } from './types';
import { singleWriter } from './write';

/**
 * The three behaviours the console this replaces got wrong, each pinned by the
 * failure it actually produced rather than by the code path it takes.
 */

function respond(status: number, body: string, contentType = 'application/json'): Response {
  return new Response(body, { status, headers: { 'Content-Type': contentType } });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('step-up is an outcome, not an error', () => {
  it('reads the 428 the shell preflight answers with, and the method on it', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          respond(
            428,
            JSON.stringify({
              error: 'fresh step-up authentication required',
              code: 'step_up_required',
              method: 'local',
            }),
          ),
        ),
      ),
    );
    const outcome = await request('/api/shell/preflight');
    expect(outcome).toEqual({
      kind: 'step-up',
      method: 'local',
      message: 'fresh step-up authentication required',
    });
  });

  it('distinguishes a deployment that holds no step-up credential at all', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          respond(
            428,
            JSON.stringify({
              error: 'this dashboard holds no step-up credential for the web shell',
              code: 'step_up_unavailable',
              method: 'unavailable',
            }),
          ),
        ),
      ),
    );
    const outcome = await request('/api/shell/preflight');
    expect(outcome.kind).toBe('step-up');
    expect(outcome.kind === 'step-up' && outcome.method).toBe('unavailable');
  });

  it('does not guess a method when the control plane omits it, because it cannot', async () => {
    // internal/server/server.go sends `{error, code}` and no method: the session
    // there was established federally, so re-authenticating is the only answer.
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          respond(
            428,
            JSON.stringify({
              error: 'fresh step-up authentication required',
              code: 'step_up_required',
            }),
          ),
        ),
      ),
    );
    const outcome = await request('/api/settings/cloud', { method: 'PUT', body: {} });
    // This is the shape stepUpRequired in internal/server/iam.go actually
    // answers with — a code and a sentence. Naming a method here was picking
    // one for every deployment alike, and the one picked sent a legacy or local
    // operator on a full navigation that discarded the form they were filling
    // in. Which credential can satisfy this is the session's property, and the
    // session is not something a transport rule can see.
    expect(outcome.kind === 'step-up' && outcome.method).toBe('unstated');
  });

  it('leaves a 428 that is not a step-up as an error', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(respond(428, JSON.stringify({ error: 'precondition' })))),
    );
    expect((await request('/api/status')).kind).toBe('error');
  });
});

describe('the server sentence survives', () => {
  it('keeps a text/plain rejection, which http.Error produces', async () => {
    // Parsing only JSON threw away the prose naming the rejected field and left
    // the reader with a bare "HTTP 400".
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(respond(400, 'pve_endpoint must be an absolute URL\n', 'text/plain')),
      ),
    );
    const outcome = await request('/api/settings/cloud', { method: 'PUT', body: {} });
    expect(outcome).toEqual({
      kind: 'error',
      status: 400,
      message: 'pve_endpoint must be an absolute URL',
    });
  });

  it('prefers details over error when a handler sends both', async () => {
    // `error` is the class of failure and `details` is what happened; a reader
    // told "conflict" has been told the category of their problem and nothing
    // about it. Asserted as the whole outcome, because "it is an error" is true
    // of every possible answer here and so pins nothing.
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          respond(409, JSON.stringify({ error: 'conflict', details: 'policy version 7 is stale' })),
        ),
      ),
    );
    expect(await request('/api/projects/policy')).toEqual({
      kind: 'error',
      status: 409,
      message: 'policy version 7 is stale',
    });
  });

  it('still carries the error label when that is the only sentence sent', async () => {
    // The other half of the preference: most handlers send `error` alone, and a
    // reader of those must not be left with the status number.
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(respond(409, JSON.stringify({ error: 'conflict' })))),
    );
    expect(await request('/api/projects/policy')).toEqual({
      kind: 'error',
      status: 409,
      message: 'conflict',
    });
  });

  it('makes 401 its own outcome, so the redirect is the shell decision', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(respond(401, 'unauthorized', 'text/plain'))),
    );
    expect(await request('/api/iam/me')).toEqual({ kind: 'unauthorized' });
  });

  it('turns a transport failure into the same error state an outage produces', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new Error('NetworkError'))),
    );
    const outcome = await request('/api/status');
    expect(outcome).toEqual({ kind: 'error', status: 0, message: 'NetworkError' });
  });
});

describe('a write in flight discards further activations', () => {
  it('answers the second activation with null and never runs it', async () => {
    // The replay is the harm. A successful settings write empties the secret
    // inputs, and an empty secret means "keep the stored one" — so a queued
    // second submission would carry blanks for credentials the operator typed
    // seconds earlier, and a wiped credential looks exactly like a kept one.
    let runs = 0;
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const writer = singleWriter(async () => {
      runs += 1;
      await gate;
      return 'written';
    });

    const first = writer.run();
    expect(writer.pending()).toBe(true);
    const second = await writer.run();
    expect(second).toBeNull();
    expect(runs).toBe(1);

    release?.();
    expect(await first).toBe('written');
    expect(writer.pending()).toBe(false);
    // And the resource is writable again once nothing is in flight.
    expect(await writer.run()).toBe('written');
    expect(runs).toBe(2);
  });

  it('clears the flag when the write rejects, so a failure is correctable', async () => {
    const writer = singleWriter(() => Promise.reject(new Error('rejected')));
    await expect(writer.run()).rejects.toThrow('rejected');
    expect(writer.pending()).toBe(false);
  });
});

describe('the five states are decided once', () => {
  it('never says empty before the first request resolves', () => {
    expect(resolveState({ outcome: null, count: 0 })).toEqual({ state: 'loading' });
  });

  it('separates an account with nothing in it from a filter matching nothing', () => {
    const outcome = { kind: 'ok', value: [] } as const;
    expect(resolveState({ outcome, count: 0 })).toEqual({ state: 'empty' });
    expect(resolveState({ outcome, count: 0, query: 'app-misc/' })).toEqual({
      state: 'filtered-empty',
      query: 'app-misc/',
    });
  });

  it('makes a failed load its own state and not a flavour of empty', () => {
    expect(
      resolveState({ outcome: { kind: 'error', status: 503, message: 'ledger unavailable' } }),
    ).toEqual({
      state: 'error',
      status: 503,
      message: 'ledger unavailable',
    });
  });

  it('renders a surface that lost one section as partial, with the data it did get', () => {
    expect(
      resolveState({
        outcome: { kind: 'ok', value: { rows: 3 } },
        count: 3,
        missing: ['scheduler'],
      }),
    ).toEqual({ state: 'partial', data: { rows: 3 }, missing: ['scheduler'] });
  });

  it('writes no node on the happy path, so a polling surface holds its geometry', () => {
    expect(resolveState({ outcome: { kind: 'ok', value: [1, 2] }, count: 2 })).toEqual({
      state: 'ok',
      data: [1, 2],
    });
  });
});

/**
 * Every module that opens a connection, and the text it opens it from.
 *
 * The two beyond this directory are named rather than discovered: the settings
 * test takes the form as its body and the single-session revoke takes a query,
 * neither of which `api` can express, so both are declared beside the form that
 * sends them — and a path spelled there is still a path the console reaches.
 */
function transportSources(): string {
  const here = dirname(fileURLToPath(import.meta.url));
  const endpoints = readFileSync(join(here, 'endpoints.ts'), 'utf8');
  return [
    // The declaration list itself is not evidence that anything calls the path,
    // so it is cut away before the search.
    endpoints.slice(endpoints.indexOf('export const api'), endpoints.indexOf('ENDPOINT_PATHS')),
    readFileSync(join(here, 'stream.ts'), 'utf8'),
    readFileSync(join(here, '..', 'pages', 'settings', 'api.ts'), 'utf8'),
  ].join('\n');
}

/**
 * The paths that text actually reaches.
 *
 * Anchored on what can precede one — the quote opening the literal, or the brace
 * closing an interpolation, which is how the shell socket builds its URL — and
 * terminated by what ends one, so `/api/shell` and `/api/shell/preflight` stay
 * two paths and a query string is part of neither. A path named in prose carries
 * no quote in front of it and so is not mistaken for a call.
 */
function pathsReached(source: string): Set<string> {
  const reached = new Set<string>();
  for (const match of source.matchAll(/['"`}](\/(?:api|auth)\/[A-Za-z0-9._/-]*)/g)) {
    reached.add(match[1] ?? '');
  }
  return reached;
}

describe('the endpoint surface is one list', () => {
  /**
   * A path is spelled in a transport module and nowhere else, and ENDPOINT_PATHS
   * is what makes that checkable — in both directions, because each catches a
   * different mistake. In the console this replaces the same URL appeared in
   * three page scripts with three different query strings, one of them pointing
   * at a route that no longer existed, which is invisible until someone opens
   * that page. A one-way check sees the route that went away; it never sees the
   * fourth spelling being added.
   */
  it('reaches every path it declares', () => {
    const reached = pathsReached(transportSources());
    expect(ENDPOINT_PATHS.filter((path) => !reached.has(path))).toEqual([]);
  });

  it('declares every path it reaches', () => {
    const declared = new Set<string>(ENDPOINT_PATHS);
    const undeclared = [...pathsReached(transportSources())]
      .filter((path) => !declared.has(path))
      .sort();
    expect(undeclared).toEqual([]);
  });

  it('names thirty-five paths, each exactly once, the shell socket among them', () => {
    // The count is asserted rather than described, so the sentence above cannot
    // drift away from the list the way "twenty-eight" did while it grew to this.
    expect(ENDPOINT_PATHS.length).toBe(35);
    expect(new Set(ENDPOINT_PATHS).size).toBe(ENDPOINT_PATHS.length);
    expect(ENDPOINT_PATHS).toContain('/api/events/jobs');
    expect(ENDPOINT_PATHS).toContain('/api/shell');
    expect(ENDPOINT_PATHS).toContain('/auth/step-up');
  });

  it('keeps the settings test beside the form, and off the shared module', () => {
    // A bodyless POST to /api/settings/cloud/test is `invalid JSON body: EOF`
    // and a 400: handleCloudSettingsTest reads the form out of the request and
    // falls back to the stored values only per omitted field. The one that works
    // is in src/pages/settings/api.ts; a second, bodyless spelling here is a
    // control that answers 400 whenever anyone finds it.
    expect(Object.keys(api)).not.toContain('testCloudSettings');
  });
});

describe('a request the control plane can decode', () => {
  it('sends a body with revoke-all, because a POST with none is answered 400', async () => {
    // handleIAMRevokeAllSessions in internal/server/iam.go decodes the body
    // before it looks at anything else, and `Decode` on a request that carries
    // none answers io.EOF — which that handler reports as "invalid session
    // revocation request". The control has never once reached the revocation.
    const inits: RequestInit[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((_path: string, init: RequestInit) => {
        inits.push(init);
        return Promise.resolve(
          respond(200, JSON.stringify({ subject_id: 'sub-1', revoked_sessions: 3 })),
        );
      }),
    );

    const outcome = await api.revokeAllSessions();
    expect(outcome).toEqual({
      kind: 'ok',
      value: { subject_id: 'sub-1', revoked_sessions: 3 },
    });
    expect(inits[0]?.method).toBe('POST');
    const body = inits[0]?.body;
    expect(typeof body).toBe('string');
    // Exactly `{}`, and not a field this module invented: the decoder disallows
    // unknown fields, so an extra key is the same 400 by a longer route. An
    // empty `subject_id` already means the caller and an empty `reason` already
    // means `revoke_all_requested`.
    expect(JSON.parse(typeof body === 'string' ? body : 'null')).toEqual({});
  });
});

describe('the job stream carries the project it is a stream of', () => {
  it('cannot be opened without naming one', () => {
    // `undefined` is assignable to an optional parameter and to nothing else,
    // so this annotation is the assertion: it stops compiling the moment the
    // scope goes back to optional, which is the state in which every caller
    // silently subscribed to the unscoped stream.
    const required: undefined extends Parameters<typeof subscribeToJobEvents>[1] ? never : true =
      true;
    expect(required).toBe(true);
  });

  it('puts the project in the query, which is what the fan-out resolves on', () => {
    const opened: string[] = [];
    vi.stubGlobal(
      'EventSource',
      class {
        constructor(url: string) {
          opened.push(url);
        }
        addEventListener(): void {
          return undefined;
        }
        removeEventListener(): void {
          return undefined;
        }
        close(): void {
          return undefined;
        }
      },
    );
    const stop = subscribeToJobEvents(() => undefined, { projectID: 'proj-7' });
    expect(opened).toEqual(['/api/events/jobs?project_id=proj-7']);
    stop();

    // A reader who holds no project has none to name, and that stream is the
    // one the control plane will still serve them.
    subscribeToJobEvents(() => undefined, { projectID: '' })();
    expect(opened[1]).toBe('/api/events/jobs');
  });
});

describe('the wire types spell what the Go encoders emit', () => {
  /**
   * These read a payload written the way the handler writes it. A field renamed
   * back to what it used to say here does not fail an assertion — it fails to
   * compile, which is the only place a wire-shape mistake can be caught before a
   * reader finds it as a blank cell or a thrown render.
   */
  it('reads an issuer generation by fingerprint and state', () => {
    // internal/workergateway/cert.go — IssuerGenerationStatus, as
    // handleWorkloadIdentityInventory encodes it.
    const inventory: WorkloadIdentityInventory = {
      issuers: [
        {
          fingerprint: 'a1b2c3d4e5f60718293a4b5c6d7e8f901234567890abcdef1234567890abcdef',
          issuer_id: 'file-1',
          provider: 'file',
          subject: 'CN=portage-engine worker issuer',
          serial: '0a1b2c',
          not_before: '2026-07-01T00:00:00Z',
          not_after: '2026-09-01T00:00:00Z',
          state: 'active',
          last_issued_at: '2026-08-01T09:00:00Z',
          active_certificates: 3,
        },
      ],
      certificates: [
        {
          fingerprint: 'ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff',
          serial: '02',
          issuer_fingerprint: 'a1b2c3d4e5f60718293a4b5c6d7e8f901234567890abcdef1234567890abcdef',
          worker_id: 'worker-3',
          job_id: 'a1e0e0f0-0000-4000-8000-000000000001',
          attempt_id: 'a1e0e0f0-0000-4000-8000-000000000002',
          attempt_fence: 4,
          not_before: '2026-08-01T09:00:00Z',
          not_after: '2026-08-01T09:20:00Z',
          state: 'active',
          issued_at: '2026-08-01T09:00:00Z',
        },
      ],
      certificate_limit: 100,
    };
    expect(inventory.issuers?.[0]?.state).toBe('active');
    expect(inventory.issuers?.[0]?.active_certificates).toBe(3);
    expect(inventory.certificates?.[0]?.issuer_fingerprint).toBe(
      inventory.issuers?.[0]?.fingerprint,
    );
  });

  it('reads the revoke-all count by the name the handler gives it', () => {
    const revoked: SessionsRevoked = { subject_id: 'sub-1', revoked_sessions: 3 };
    expect(revoked.revoked_sessions).toBe(3);
  });

  it('reads a federated session by id and issued_at', () => {
    // internal/persistence/iam_sessions.go — IAMSession. There is no
    // `session_id`, no `created_at`, no `user_agent` and no `ip_address` on it.
    const sessions: IAMSessions = {
      sessions: [
        {
          id: '9f1b2c3d-0000-4000-8000-000000000001',
          kind: 'browser',
          subject_id: 'sub-1',
          issued_at: '2026-08-01T08:00:00Z',
          expires_at: '2026-08-01T20:00:00Z',
          last_seen_at: '2026-08-01T09:30:00Z',
          acr: 'urn:mace:incommon:iap:silver',
          amr: ['pwd', 'otp'],
        },
      ],
      current_session_id: '9f1b2c3d-0000-4000-8000-000000000001',
    };
    expect(sessions.sessions?.[0]?.id).toBe(sessions.current_session_id);
  });

  it('reads the signing queue by its five states', () => {
    // internal/signing/types.go — RuntimeStatus. `pending` and `signed` have
    // never been on this wire.
    const gpg: GPGStatus = {
      enabled: true,
      ready: true,
      key_id: 'ABCDEF0123456789',
      mode: 'isolated-outbound-pull',
      private_key_here: false,
      queue: { queued: 2, claimed: 1, completed: 40, failed: 0, canceled: 0 },
    };
    expect(gpg.queue?.queued).toBe(2);
    expect(gpg.queue?.claimed).toBe(1);
  });
});
