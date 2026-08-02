/**
 * Loading, empty, filtered-empty, error and partial are five different screens,
 * and which one a surface is on is decided here once.
 *
 * Six pages in the console this replaces each re-derived it and each lost a
 * different member: a filtered result showed the copy that invites you to
 * publish your first package; a failed load rendered into the same box as an
 * account with nothing in it, distinguished only by a sentence that happened to
 * begin "Failed to load"; and a surface with no loading state moved the page
 * 102px when its payload landed.
 *
 * `ok` is the sixth member and the reason the happy path costs nothing: it
 * writes no node, so a polling surface holds exactly the geometry it held on the
 * previous poll.
 */

import type { ApiOutcome } from './client';

export type ResourceState<T> =
  | { state: 'loading' }
  | { state: 'ok'; data: T }
  /** Nothing exists yet. The screen that invites the reader to create the first one. */
  | { state: 'empty' }
  /** Things exist; this query matches none. The query is on screen and clearable. */
  | { state: 'filtered-empty'; query: string }
  /** The load failed. Carries the server's sentence and offers a retry. */
  | { state: 'error'; message: string; status: number }
  /**
   * The surface loaded and one section of it did not. It sits tighter than the
   * others because the rest of the page is still there to be read.
   */
  | { state: 'partial'; data: T; missing: readonly string[] };

export interface ResolveInput<T> {
  /** null until the first request resolves. Never render "empty" before this. */
  outcome: ApiOutcome<T> | null;
  /** How many rows the payload holds. Omitted for a resource that is not a list. */
  count?: number;
  /** The filter the reader typed, if any. Its presence is what makes empty "filtered". */
  query?: string;
  /** Names of sections that failed while the rest of the payload arrived. */
  missing?: readonly string[];
}

/**
 * The one decision.
 *
 * Order matters and each step is load-bearing: no outcome means loading and not
 * empty; an unauthorized or step-up outcome becomes an error only so a caller
 * that ignores it still shows something honest.
 *
 * Neither is left to this function to explain. A 401 is announced by the
 * transport and the shell navigates to sign-in, so the box it produces is what
 * the reader sees for the moment before that navigation — which is why it
 * carries the status and no sentence, and why `isSignedOut` exists for the
 * surfaces that would otherwise print an empty one. A step-up refusal is handled
 * where the write was issued and carries the control plane's own sentence.
 */
export function resolveState<T>(input: ResolveInput<T>): ResourceState<T> {
  const { outcome } = input;
  if (outcome === null) {
    return { state: 'loading' };
  }
  if (outcome.kind === 'error') {
    return { state: 'error', message: outcome.message, status: outcome.status };
  }
  if (outcome.kind === 'unauthorized') {
    // No sentence: the server sent none, and the English word `unauthorized` is
    // not a translated string. The status is the whole fact, and `isSignedOut`
    // is how a surface turns it into words in the reader's language.
    return { state: 'error', message: '', status: 401 };
  }
  if (outcome.kind === 'step-up') {
    return { state: 'error', message: outcome.message, status: 428 };
  }
  if (input.missing !== undefined && input.missing.length > 0) {
    return { state: 'partial', data: outcome.value, missing: input.missing };
  }
  if (input.count === 0) {
    const query = input.query ?? '';
    return query === '' ? { state: 'empty' } : { state: 'filtered-empty', query };
  }
  return { state: 'ok', data: outcome.value };
}

/**
 * The `data-state` a state box wears.
 *
 * The stylesheet reads this attribute and so does a screen reader, so the two
 * cannot come to different conclusions about which of the five a box is in —
 * which is what happened when the state was a class name and the announcement
 * was a separate string.
 */
export function stateAttribute<T>(resolved: ResourceState<T>): string {
  return resolved.state;
}

/**
 * Whether this failure is the session ending rather than the request failing.
 *
 * Decided here so the surfaces that render a state box agree about it, and
 * worded there, because this layer holds no message catalogue and a sentence
 * assembled without one is the English literal this replaces.
 */
export function isSignedOut<T>(resolved: ResourceState<T>): boolean {
  return resolved.state === 'error' && resolved.status === 401;
}
