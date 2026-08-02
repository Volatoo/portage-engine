/**
 * A single writer whose write is handed the form the activation came from.
 *
 * Two of the three session surfaces post a form — sign-in and the shell's
 * step-up — and both need the same two things at once: exactly one request per
 * burst of activations, and the credentials read out of the live form element
 * rather than out of component state. Reaching for the element through a React
 * ref is what a component would normally do and is wrong here twice over: the
 * ref is read while the writer is being constructed, which is during render,
 * and a writer built once against a ref is a writer that has to be reasoned
 * about every time the tree it points into changes.
 *
 * Passing the element in at activation time removes both problems. `submit`
 * receives the form from the event that caused it, so the write reads the
 * fields exactly as the browser would have posted them, and the guard stays on
 * the resource rather than on the control.
 */

import { singleWriter } from '../../api/write';

export interface FormWriter {
  /** Runs, or answers null because a write for this form is already in flight. */
  submit(form: HTMLFormElement): Promise<void>;
  /** A write is in flight right now. Drives aria-busy. */
  pending(): boolean;
}

export function createFormWriter(write: (form: HTMLFormElement) => Promise<void>): FormWriter {
  let target: HTMLFormElement | null = null;
  const writer = singleWriter(() => {
    const form = target;
    if (form === null) {
      throw new Error('console: a form writer ran with no form');
    }
    return write(form);
  });
  return {
    pending: () => writer.pending(),
    async submit(form) {
      if (writer.pending()) {
        // Discarded, never queued. A replayed submission collects the fields
        // the write it is replaying has already caused to be cleared.
        return;
      }
      // `singleWriter` runs the write synchronously up to its first await, so
      // this is read before any other activation can reach the closure.
      target = form;
      await writer.run();
    },
  };
}

/**
 * One field of a submitted form, as a string.
 *
 * `FormData.get` answers `string | File`, and a `File` put through `String()`
 * is the literal text `[object Object]` — which as a password is a value the
 * server will refuse with no hint as to why.
 */
export function formField(fields: FormData, name: string): string {
  const value = fields.get(name);
  return typeof value === 'string' ? value : '';
}
