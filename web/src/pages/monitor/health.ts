/**
 * Three of /monitor's six rollups answer 503 with the whole status document in
 * the body, and that is the answer the card is for.
 *
 * `handleLedgerStatus`, `handleRuntimeMetadataStatus` and `handleCacheStatus`
 * (internal/server) all write the status header from the health they just
 * computed and then encode the status anyway — a ledger with nineteen write
 * errors is 503 `{ok: false, write_errors: 19, last_reconcile: {…}}`. Read as a
 * transport failure that becomes an error box reading "Failed to load: " and
 * then the whole JSON document, and the two numbers the card exists to show are
 * on screen as punctuation.
 *
 * The console this replaces knew this for the ledger — it used a bare `fetch`
 * there and tolerated `!ok` when the body carried an `ok` field — and did not
 * know it for the other two, whose cards were written expecting `ok: false`
 * (`statusBadge(response.ok && integrityOK ? 'passed' : 'failed')`) and could
 * never reach it, because the shared helper threw first. Same tolerance for all
 * three: the surface either has the document or it does not.
 *
 * This sits on top of `request()` rather than beside it — no second transport,
 * no second 401 rule — and it recovers the body from the message the error
 * outcome already carries, which for these three is the raw JSON precisely
 * because the envelope has no top-level `error` or `details` string for
 * `readFailure` to prefer. Where it does (the cache's no-client branch names its
 * initialisation failure there), the parse fails and the error outcome stands,
 * which is the honest answer for that branch anyway. A `degraded` outcome that
 * carries the parsed body is the shared-layer shape this wants; it is a handoff.
 */

import type { ApiOutcome } from '../../api/client';

/**
 * A body is the health envelope when it is an object carrying a boolean `ok`.
 *
 * Deliberately not a check for the fields any one card reads: every one of the
 * three envelopes has a different payload and all three have `ok`, and a check
 * that named payload fields would silently stop recognising an envelope the day
 * a field was renamed rather than failing where the rename happened.
 */
function asEnvelope(message: string): unknown {
  let parsed: unknown;
  try {
    parsed = JSON.parse(message);
  } catch {
    return null;
  }
  if (typeof parsed !== 'object' || parsed === null) {
    return null;
  }
  return typeof (parsed as { ok?: unknown }).ok === 'boolean' ? parsed : null;
}

/**
 * Turn "the service says it is unhealthy" back into a payload the card can
 * render. Anything else — a 500, a 401, a timeout, a body that is not the
 * envelope — is left exactly as it arrived.
 */
export function tolerateDegraded<T>(outcome: ApiOutcome<T>): ApiOutcome<T> {
  if (outcome.kind !== 'error' || outcome.status !== 503) {
    return outcome;
  }
  const envelope = asEnvelope(outcome.message);
  return envelope === null ? outcome : { kind: 'ok', value: envelope as T };
}
