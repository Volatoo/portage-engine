import { useEffect, useRef } from 'react';

import { busyAttributes, isCompositionEnter } from '../api/write';
import { useMessages } from '../i18n/context';
import { formField } from '../pages/login/formWriter';

/**
 * The administrator credential a step-up refusal asks for.
 *
 * `<dialog>` rather than a div, for the reason the confirmation dialog uses one:
 * the modality, the focus trap, the Escape key and the inert background are the
 * browser's. And a dialog rather than the two `window.prompt()` calls the
 * console this replaces made — a password typed into a plain-text box with no
 * label, no `autocomplete` for a password manager to see, and no way back to the
 * first field once the second had opened.
 *
 * It never replaces what is already on screen. The reader clicked a control on a
 * page they were reading; a credential request is a thing on top of that page,
 * not instead of it.
 */

export interface Credentials {
  username: string;
  password: string;
}

export interface StepUpDialogProps {
  open: boolean;
  /**
   * Unique per instance. Two write sites on one page each own a prompt, and two
   * labels pointing at one id is a label pointing at the wrong field.
   */
  fieldID: string;
  busy: boolean;
  /** A credential was offered and refused. The dialog stays up to be retried. */
  failed: boolean;
  onSubmit: (credentials: Credentials) => void;
  onCancel: () => void;
}

export function StepUpDialog({
  open,
  fieldID,
  busy,
  failed,
  onSubmit,
  onCancel,
}: StepUpDialogProps) {
  const messages = useMessages();
  const ref = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const node = ref.current;
    if (node === null || !open) {
      return undefined;
    }
    // Read before the dialog opens, because opening it moves focus inside: this
    // is the control the reader activated, and it is where they have to be put
    // back. Every dismissal path runs through the cleanup below — confirmed,
    // cancelled, or Escape — because all three of them unmount this component
    // rather than re-rendering it with `open` false.
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
      // Closed rather than merely removed. A modal that is unmounted while open
      // takes the reading position down with it — the browser restores focus on
      // close and has nothing to restore it from once the element is gone — and
      // the reader lands on <body>, four Tab presses from where they were.
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

  return (
    <dialog
      className="stepup-dialog"
      ref={ref}
      onCancel={onCancel}
      aria-labelledby={`${fieldID}-title`}
    >
      <h2 id={`${fieldID}-title`}>{messages.t('stepup.required')}</h2>
      <form
        onSubmit={(submitted) => {
          submitted.preventDefault();
          // And stopped here, not merely defaulted away. `createPortal` moves
          // this dialog into the top layer but leaves it where it was declared
          // in the REACT tree, so a submit raised inside it still reaches every
          // React handler above the write site — and two of those sites sit
          // inside the settings page's own form, whose onSubmit is a PUT of
          // forty-five controls nobody asked to write. Where the prompt is
          // rendered is the structural fix; this is the belt.
          submitted.stopPropagation();
          const fields = new FormData(submitted.currentTarget);
          onSubmit({
            username: formField(fields, 'username'),
            password: formField(fields, 'password'),
          });
        }}
        onKeyDown={(pressed) => {
          // An Enter that only confirmed an IME candidate is not a submission,
          // and it is checked at the form so neither field has to remember.
          if (
            isCompositionEnter({ key: pressed.key, isComposing: pressed.nativeEvent.isComposing })
          ) {
            pressed.preventDefault();
          }
        }}
      >
        <div className="field">
          <label htmlFor={`${fieldID}-user`}>{messages.t('stepup.user')}</label>
          <input
            type="text"
            id={`${fieldID}-user`}
            name="username"
            autoComplete="username"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            aria-describedby={`${fieldID}-status`}
          />
        </div>
        <div className="field">
          <label htmlFor={`${fieldID}-pass`}>{messages.t('stepup.password')}</label>
          <input
            type="password"
            id={`${fieldID}-pass`}
            name="password"
            autoComplete="current-password"
            aria-describedby={`${fieldID}-status`}
          />
        </div>
        {/* Mounted in both states and empty in one of them, so a refusal
            arrives as a change to a region that was already there rather than
            as a region appearing under the reader's pointer. */}
        <p className="save-msg err" id={`${fieldID}-status`} role="status">
          {failed ? messages.t('stepup.failed') : ''}
        </p>
        <div className="form-actions stepup-actions">
          <button type="button" className="btn" onClick={onCancel}>
            {messages.t('common.cancel')}
          </button>
          <button type="submit" className="btn blue" {...busyAttributes(busy)}>
            {messages.t('login.submit')}
          </button>
        </div>
      </form>
    </dialog>
  );
}
