import { useEffect, useRef } from 'react';
import type { RefObject } from 'react';

/**
 * Opens a `<dialog>` modally for as long as it is mounted, and hands the reading
 * position back to the control that opened it when it goes.
 *
 * Both modals this console raises — the credential prompt and the destructive
 * confirmation — were opened with `showModal()` and then taken down by
 * unmounting rather than closed. The browser restores focus on close and has
 * nothing to restore it from once the element is gone, so every dismissal
 * dropped a keyboard reader onto `<body>`: several Tab presses from where they
 * were in the credential prompt's case, and on /builds a whole list away from
 * the button they had just used.
 *
 * The rule lives here rather than in each dialog because it was written twice,
 * verbatim, to fix that one bug — and two copies of a focus-management effect
 * are two places for the next dismissal path to be forgotten. There is one
 * dismissal path in this file, and confirming, cancelling and Escape all reach
 * it the same way: all three unmount the dialog rather than re-rendering it with
 * `open` false, so the cleanup below is what runs for every one of them.
 *
 * @param open Whether the dialog should be showing. The callers mount their
 *   dialog only while it is being asked and pass `true`, but the flag is honoured
 *   so a caller that renders one permanently and toggles the flag gets the same
 *   open-and-restore behaviour rather than a dialog that never opens.
 * @returns The ref to put on the `<dialog>` element.
 */
export function useModalDialog(open: boolean): RefObject<HTMLDialogElement | null> {
  const ref = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const node = ref.current;
    if (node === null || !open) {
      return undefined;
    }
    // Read before the dialog opens, because opening it moves focus inside: this
    // is the control the reader activated, and it is where they have to be put
    // back.
    const opener = document.activeElement;
    // showModal is what makes the rest of the page inert. Guarded because a
    // test renderer may implement the element without it, and a dialog that
    // opens non-modally is still a dialog.
    if (typeof node.showModal === 'function') {
      node.showModal();
    } else {
      node.setAttribute('open', '');
    }
    return () => {
      // Closed rather than merely removed, so the element the browser is
      // restoring focus from is still in the document when it does it.
      if (typeof node.close === 'function') {
        node.close();
      } else {
        node.removeAttribute('open');
      }
      // And stated outright, not left to the close: the fallback path above
      // never opened a top-layer dialog, so nothing is coming back on its own.
      if (opener instanceof HTMLElement && opener.isConnected) {
        opener.focus();
      }
    };
  }, [open]);

  return ref;
}
