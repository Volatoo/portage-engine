/**
 * A write in flight discards further activations. It never queues them.
 *
 * The settings page in the console this replaces queued them, and the queue is
 * the harm. `/api/settings/cloud` has exactly one writer and two controls that
 * use it — Save, and Start Test Build — and a successful write is what empties
 * the secret inputs, because an empty secret is the wire spelling of "keep the
 * stored one". A replayed submission therefore collected the fields the write it
 * was replaying had just cleared, and sent blanks for credentials the operator
 * had typed seconds earlier. The reader could not tell a kept credential from a
 * lost one: both look like a saved form.
 *
 * So the flag belongs to the resource, not to a button — two controls writing
 * one resource share one flag — and a discarded activation is answered with
 * `null`, never awaited and never replayed.
 */

export interface SingleWriter<T> {
  /** Runs, or returns null because a write is already in flight. */
  run(): Promise<T | null>;
  /** Whether a write is in flight right now. Drives `aria-busy`. */
  pending(): boolean;
}

export function singleWriter<T>(write: () => Promise<T>): SingleWriter<T> {
  let inFlight = false;
  return {
    pending: () => inFlight,
    async run() {
      if (inFlight) {
        return null;
      }
      inFlight = true;
      try {
        return await write();
      } finally {
        inFlight = false;
      }
    },
  };
}

/**
 * The busy attributes for a control mid-write.
 *
 * `aria-disabled`, never `disabled`: `disabled` drops the control out of the tab
 * order in the middle of the operation and a keyboard reader loses the element
 * they just used. The handler guard is what actually stops the second
 * activation — that is `singleWriter` above, and it is what makes a double
 * click, a held Enter, an Enter confirming an IME candidate and a slow network
 * all produce exactly one write.
 *
 * `aria-busy` says the control is working; the progress itself is announced from
 * a live region and not from the control, because a control that narrates its
 * own state is re-read in full on every focus.
 */
export function busyAttributes(busy: boolean): {
  'aria-disabled': 'true' | 'false';
  'aria-busy'?: 'true';
} {
  return busy ? { 'aria-disabled': 'true', 'aria-busy': 'true' } : { 'aria-disabled': 'false' };
}

/**
 * True when a keyboard event is an Enter that only confirmed an IME candidate.
 *
 * Both shipped languages are typed through an IME at least some of the time, and
 * the Enter that closes the candidate window is not a submission. Checked at the
 * form, so no individual control has to remember.
 */
export function isCompositionEnter(event: { key: string; isComposing: boolean }): boolean {
  return event.key === 'Enter' && event.isComposing;
}
