/**
 * The pieces the four build pages share: the status badge, the one state box,
 * the reserved rows a table holds before its payload lands, and the dialog a
 * bulk delete has to pass through.
 */

import { useEffect, useRef } from 'react';
import type { ReactNode } from 'react';

import { isSignedOut, stateAttribute } from '../../api/state';
import type { ResourceState } from '../../api/state';
import { busyAttributes } from '../../api/write';
import { useMessages } from '../../i18n/context';
import type { MessageKey } from '../../i18n/messages';

/**
 * The status badge is shared: the same dot-plus-word primitive the monitor and
 * public surfaces draw, so the two rules it enforces live in one file rather
 * than in three that agreed by coincidence.
 */
export { StatusBadge } from '../../components/StatusBadge';

export interface StateBoxProps {
  state: ResourceState<unknown>;
  /** The sentence for a surface with nothing in it yet. */
  emptyKey: MessageKey;
  /** The sentence for a query that matched nothing. A different fact. */
  filteredKey?: MessageKey;
  /** What a partial surface says about the section it lost. */
  note?: string;
  /**
   * A sentence in front of the state's own, for the case where the server's
   * words need framing: "build job not found" is what the control plane says,
   * and what it means for this page is a different sentence.
   */
  lead?: string;
  onRetry?: () => void;
  /** Controls that belong beside the state's own — clearing a filter, say. */
  actions?: ReactNode;
}

/**
 * The five states, in one box, with the state written on it as an attribute so
 * the stylesheet and a screen reader cannot come to different conclusions.
 *
 * It renders nothing at all on `ok`, which is what lets a polling surface hold
 * exactly the geometry it held on the previous poll. The live region is this
 * box and only this box: it exists to carry state text, so announcing it
 * announces a state change and not the page's whole contents — /status put
 * role="status" on a container holding its component list and re-announced all
 * of it every thirty seconds, forever.
 */
export function StateBox({
  state,
  emptyKey,
  filteredKey,
  note,
  lead,
  onRetry,
  actions,
}: StateBoxProps): ReactNode {
  const messages = useMessages();
  if (state.state === 'ok') {
    return null;
  }
  const sentence = ((): string => {
    switch (state.state) {
      case 'loading':
        return messages.t('common.loading');
      case 'empty':
        return messages.t(emptyKey);
      case 'filtered-empty':
        return messages.t(filteredKey ?? emptyKey);
      case 'partial':
        return note ?? '';
      case 'error':
        // A 401 carries no sentence of its own: the server sent none, and the
        // shell is already navigating to sign-in. This is what the box says for
        // the moment before that lands, in the reader's language rather than as
        // the English word the transport happens to call the outcome.
        return isSignedOut(state) ? messages.t('common.expired') : state.message;
    }
  })();
  const retryable = state.state === 'error' || state.state === 'partial';
  return (
    <div className="build-state" data-state={stateAttribute(state)}>
      {/* The live region holds the sentences and stops there. Retry and the
          page's own controls are siblings of it, not children: a button inside
          a region announces its label with every state change, so a reader who
          arrived at an error heard the sentence and then "Retry" again on the
          way back to loading. The other two ported surfaces put their controls
          outside for the same reason; this one is brought into line with them. */}
      <div className="empty" data-state={stateAttribute(state)} role="status" aria-live="polite">
        {lead === undefined ? null : <p>{lead}</p>}
        <p>{sentence}</p>
      </div>
      {retryable || actions !== undefined ? (
        <p className="state-actions">
          {onRetry !== undefined && retryable ? (
            <button type="button" className="btn" onClick={onRetry}>
              {messages.t('common.retry')}
            </button>
          ) : null}
          {actions}
        </p>
      ) : null}
    </div>
  );
}

/**
 * The rows a table holds while its first payload is out.
 *
 * `table-layout: fixed` already pins the column boundaries before any data
 * arrives; this pins the height, so the card below a list does not walk down the
 * page when the list lands. They are `aria-hidden` and carry no text: a
 * placeholder that reads as a value is worse than a gap, and the state box below
 * the table is what actually says the surface is loading.
 */
export function SkeletonRows({ rows, columns }: { rows: number; columns: number }) {
  return Array.from({ length: rows }, (_row, rowIndex) => (
    <tr key={rowIndex} className="skeleton-row" aria-hidden="true">
      {Array.from({ length: columns }, (_column, columnIndex) => (
        <td key={columnIndex}>
          <span className="skeleton-bar" />
        </td>
      ))}
    </tr>
  ));
}

export interface ConfirmDialogProps {
  open: boolean;
  titleKey: MessageKey;
  /** What the action will do, stated in numbers the reader can check. */
  detail: string;
  /** The label on the button that runs it. It names the action, never "OK". */
  confirmLabel: string;
  /**
   * Whether the action destroys something. The danger treatment is spent on the
   * ones that do and on nothing else — a retry that creates a fresh attempt
   * wearing the same red as a delete teaches a reader to ignore red.
   */
  destructive?: boolean;
  busy: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

/**
 * The dialog a destructive bulk action passes through.
 *
 * `<dialog>` rather than a div: the modality, the focus trap, the Escape key and
 * the inert background are the browser's, and every hand-built version of them
 * this console could write would be a worse one. The confirming control wears
 * the danger treatment and states the count, because "Remove all failed job
 * records?" and "12 records will be deleted" are not the same question.
 */
export function ConfirmDialog({
  open,
  titleKey,
  detail,
  confirmLabel,
  destructive = true,
  busy,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
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
      // the reader lands on <body>, which on /builds is the whole list away from
      // the control they just used.
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
      className="confirm-dialog"
      ref={ref}
      onCancel={onCancel}
      aria-labelledby="confirm-title"
    >
      <h2 id="confirm-title">{messages.t(titleKey)}</h2>
      <p className="confirm-detail">{detail}</p>
      <div className="confirm-actions">
        <button type="button" className="btn" onClick={onCancel}>
          {messages.t('common.cancel')}
        </button>
        <button
          type="button"
          className={destructive ? 'btn danger' : 'btn blue'}
          onClick={onConfirm}
          {...busyAttributes(busy)}
        >
          {confirmLabel}
        </button>
      </div>
    </dialog>
  );
}
