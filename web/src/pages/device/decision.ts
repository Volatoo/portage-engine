/**
 * The device page's two rules that have nothing to do with React: what a valid
 * authorization code looks like, and how many times a decision may be sent.
 *
 * Both are lifted out of the component on purpose. The idempotency guard is the
 * one thing the device page in the console this replaces got right that the rest
 * of that console got wrong, and it was verified by hand — a double click, six
 * activations 60ms apart, an approve and a deny dispatched in the same tick, and
 * clicks after the decision had already landed all produced exactly one backend
 * call. Keeping it here is what lets those four cases be facts a test states
 * rather than a property someone re-measures in a browser.
 */

import type { ApiOutcome } from '../../api/client';
import type { DeviceDecision } from '../../api/types';
import { singleWriter } from '../../api/write';

/** The server's own vocabulary for what the reader decided. */
export type Decision = 'approve' | 'deny';

/**
 * The code's alphabet, from the issuer: no I, no O, no 0, no 1.
 *
 * That exclusion is the reason the input renders in the monospace face. The code
 * is read off a terminal and compared with this page character by character, and
 * the four glyphs a proportional face makes ambiguous are exactly the four the
 * alphabet already refuses — so the comparison only works if the face the reader
 * is comparing against is the one the terminal is using.
 */
const USER_CODE = /^[A-HJ-NP-Z2-9]{8}$/;

/**
 * The code as the server spells it, or '' when this is not one.
 *
 * Case and punctuation are the reader's, not the code's: it is being retyped
 * from another screen, so a lowercase paste and a missing hyphen are both the
 * same code. `toUpperCase` and not `toLocaleUpperCase` — the alphabet is ASCII,
 * and a Turkish locale's dotless i would turn a valid code into an invalid one.
 */
export function normalizeUserCode(raw: string): string {
  const compact = raw.toUpperCase().replace(/[^A-Z0-9]/g, '');
  if (!USER_CODE.test(compact)) {
    return '';
  }
  return `${compact.slice(0, 4)}-${compact.slice(4)}`;
}

export interface DecisionGate {
  /**
   * Sends the decision, or answers null because this activation is not the one
   * that counts — either a send is already in flight or the decision has
   * already landed.
   */
  decide(userCode: string, decision: Decision): Promise<ApiOutcome<DeviceDecision> | null>;
  /** A send is in flight. Drives aria-busy. */
  pending(): boolean;
  /** The decision has landed and this page is finished writing. */
  settled(): boolean;
  /** Called once, by the caller that saw the decision land. */
  settle(): void;
}

/**
 * One gate for both buttons, because there is one resource.
 *
 * Approve and Deny write the same device code, so a per-button flag would let an
 * approve and a deny dispatched in the same tick both reach the control plane —
 * which is not a duplicate request but a contradictory one, and the server has
 * to break the tie for a reader who only pressed one thing. The flag belongs to
 * the resource.
 *
 * `singleWriter` runs the write synchronously up to its first await, so the
 * decision below is read before any other activation can reach this closure:
 * a second call in the same tick finds the writer already in flight and is
 * discarded rather than queued behind the first.
 */
export function createDecisionGate(
  send: (userCode: string, decision: Decision) => Promise<ApiOutcome<DeviceDecision>>,
): DecisionGate {
  let requested: { userCode: string; decision: Decision } | null = null;
  let settled = false;
  const writer = singleWriter(() => {
    const current = requested;
    if (current === null) {
      throw new Error('device: the gate ran with no decision requested');
    }
    return send(current.userCode, current.decision);
  });
  return {
    pending: () => writer.pending(),
    settled: () => settled,
    settle() {
      settled = true;
    },
    decide(userCode, decision) {
      if (settled) {
        // A click after the terminal has already been told is not a second
        // decision to make; it is a reader checking that the page heard them.
        return Promise.resolve(null);
      }
      if (writer.pending()) {
        // Discarded, never queued. A queued approve behind a deny is a decision
        // the reader did not make arriving at the control plane after the one
        // they did, and the page would have shown them the first one's answer.
        return Promise.resolve(null);
      }
      requested = { userCode, decision };
      return writer.run();
    },
  };
}
