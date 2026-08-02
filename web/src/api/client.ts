/**
 * One request function, and the three things the console this replaces got
 * wrong encoded into its result type rather than left to each caller.
 *
 * 1. A step-up refusal is an outcome, not an error. The control plane answers
 *    428 with `code: "step_up_required"` and a `method` naming the credential
 *    that would satisfy it. Flattened into `throw new Error("HTTP 428")` — which
 *    is what a generic error path does with it — the reader is told the request
 *    failed and given nothing to do about it, and the one page that handled it
 *    handled it by prompting for a password inside a catch block.
 *
 * 2. A 401 is its own outcome too, and it is announced: `onSessionExpired` tells
 *    whoever is listening, and the shell — which is the only thing that knows
 *    whether this route needs a session — decides in one place that the reader
 *    goes to sign in. That is the difference between a decision made once and a
 *    `location.href` buried in a transport helper that every page, including the
 *    sign-in page itself, unknowingly inherits.
 *
 * 3. Everything else carries the server's own sentence. Several handlers answer
 *    with `http.Error`, which is `text/plain`; parsing only JSON threw away the
 *    prose naming the rejected field and left the reader with a bare `HTTP 400`.
 */

import type { StepUpMethod } from './types';

/** What a request produced. Exhaustive: every branch is a screen. */
export type ApiOutcome<T> =
  | { kind: 'ok'; value: T }
  | { kind: 'unauthorized' }
  | { kind: 'step-up'; method: StepUpMethod; message: string }
  | { kind: 'error'; status: number; message: string };

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE';
  /** Serialised as JSON with the header set. */
  body?: unknown;
  /** Sent as `X-Project-ID`, so a request is scoped to the chosen project. */
  projectID?: string;
  signal?: AbortSignal;
}

const STEP_UP_CODES = new Set(['step_up_required', 'step_up_unavailable']);

/**
 * Who to tell when a request came back 401.
 *
 * One listener, not a list: there is exactly one thing to decide — where an
 * expired session sends the reader — and a second subscriber would be a second
 * answer to it. The transport announces and returns the outcome; it never
 * navigates, because whether this route needs a session is not something a
 * transport helper can know.
 */
let expiredListener: (() => void) | null = null;

export function onSessionExpired(listener: () => void): () => void {
  expiredListener = listener;
  return () => {
    if (expiredListener === listener) {
      expiredListener = null;
    }
  };
}

function isStepUpMethod(value: unknown): value is StepUpMethod {
  return value === 'local' || value === 'federated' || value === 'unavailable';
}

/**
 * Read whatever the server said, in whichever of its two shapes.
 *
 * Returns the raw text as well, because a `text/plain` rejection is the message
 * and a JSON one hides it under `error` or `details`.
 */
async function readFailure(response: Response): Promise<{ body: unknown; message: string }> {
  let raw: string;
  try {
    raw = await response.text();
  } catch {
    // A body that cannot be read is not more informative than the status.
    return { body: null, message: `HTTP ${String(response.status)}` };
  }
  try {
    const body: unknown = JSON.parse(raw);
    if (typeof body === 'object' && body !== null) {
      const fields = body as Record<string, unknown>;
      const stated = fields['details'] ?? fields['error'];
      if (typeof stated === 'string' && stated !== '') {
        return { body, message: stated };
      }
    }
    return { body, message: raw.trim() || `HTTP ${String(response.status)}` };
  } catch {
    return { body: null, message: raw.trim() || `HTTP ${String(response.status)}` };
  }
}

export async function request<T>(
  path: string,
  options: RequestOptions = {},
): Promise<ApiOutcome<T>> {
  const headers = new Headers();
  if (options.projectID !== undefined && options.projectID !== '') {
    headers.set('X-Project-ID', options.projectID);
  }
  if (options.body !== undefined) {
    headers.set('Content-Type', 'application/json');
  }
  headers.set('Accept', 'application/json');

  let response: Response;
  try {
    response = await fetch(path, {
      method: options.method ?? 'GET',
      headers,
      ...(options.body === undefined ? {} : { body: JSON.stringify(options.body) }),
      ...(options.signal === undefined ? {} : { signal: options.signal }),
    });
  } catch (cause) {
    // A transport failure is indistinguishable from an outage to the reader, and
    // both want the same screen: the error state, with a retry.
    return {
      kind: 'error',
      status: 0,
      message: cause instanceof Error ? cause.message : String(cause),
    };
  }

  if (response.status === 401) {
    // Announced on every 401, including the fifteen-second poll's: a session
    // that expires while a page is being watched is the case the console this
    // replaces handled and this port did not, and the poll would otherwise keep
    // running against a credential the server has already forgotten.
    expiredListener?.();
    return { kind: 'unauthorized' };
  }
  if (!response.ok) {
    const { body, message } = await readFailure(response);
    if (response.status === 428 && typeof body === 'object' && body !== null) {
      const fields = body as Record<string, unknown>;
      if (typeof fields['code'] === 'string' && STEP_UP_CODES.has(fields['code'])) {
        // `method` is absent on the control plane's own 428, which only ever
        // means a fresh credential of whatever kind established this session.
        const method = isStepUpMethod(fields['method']) ? fields['method'] : 'federated';
        return { kind: 'step-up', method, message };
      }
    }
    return { kind: 'error', status: response.status, message };
  }

  if (response.status === 204) {
    return { kind: 'ok', value: undefined as T };
  }
  try {
    return { kind: 'ok', value: (await response.json()) as T };
  } catch (cause) {
    return {
      kind: 'error',
      status: response.status,
      message: cause instanceof Error ? cause.message : String(cause),
    };
  }
}
