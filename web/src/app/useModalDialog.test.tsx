import { act, useState } from 'react';
import type { ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import type { Root } from 'react-dom/client';
import { afterEach, describe, expect, it } from 'vitest';

import { MessagesProvider } from '../i18n/context';
import { ConfirmDialog } from '../pages/builds/parts';
import { StepUpDialog } from './StepUpDialog';

/**
 * The one focus rule both modals obey, asserted once per dismissal path per
 * dialog.
 *
 * The bug it pins: both dialogs were opened with `showModal()` and then taken
 * down by unmounting rather than closed, so the browser had nothing left to
 * restore focus from and every dismissal dropped a keyboard reader onto
 * `<body>`. The rule now lives in `useModalDialog`, and the reason this file
 * covers six cases rather than one is that a hook is only as good as the paths
 * that reach it: a dialog wired up wrongly, or a dismissal that stops unmounting
 * the component, loses the restoration without touching the hook at all.
 *
 * jsdom implements neither `showModal` nor `close`, which is why the hook
 * carries a fallback, why the restoration has to be stated outright rather than
 * left to the platform, and why each case below moves focus into the dialog by
 * hand before dismissing it — without that the reader never left the opener and
 * the assertion would pass against a dialog that restores nothing.
 */

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const mounted: { root: Root; container: HTMLElement }[] = [];

afterEach(() => {
  for (const { root, container } of mounted.splice(0)) {
    act(() => {
      root.unmount();
    });
    container.remove();
  }
});

function mount(node: ReactNode): HTMLElement {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  act(() => {
    root.render(<MessagesProvider lang="en">{node}</MessagesProvider>);
  });
  mounted.push({ root, container });
  return container;
}

/** The control carrying exactly this label, anywhere under `scope`. */
function control(scope: ParentNode, label: string): HTMLElement {
  const found = [...scope.querySelectorAll('button')].filter(
    (node) => (node.textContent ?? '').trim() === label,
  );
  expect(found.length, `no single control labelled "${label}"`).toBe(1);
  return found[0] as HTMLElement;
}

/** Activates a control the way a reader does: focus first, then the click. */
function press(node: HTMLElement): void {
  node.focus();
  act(() => {
    node.click();
  });
}

/**
 * Escape, as the platform delivers it.
 *
 * A modal dismissed with Escape raises `cancel` on the element itself and no
 * click on anything, and jsdom does not synthesise it from a key press — so the
 * event is dispatched here. This is the path that gets forgotten, because
 * nothing about a keydown test would notice it missing.
 */
function escape(dialog: HTMLElement): void {
  control(dialog, 'Cancel').focus();
  act(() => {
    dialog.dispatchEvent(new Event('cancel', { bubbles: true, cancelable: true }));
  });
}

/** A page with one control, which raises the credential prompt when activated. */
function StepUpHarness() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button
        type="button"
        id="opener"
        onClick={() => {
          setOpen(true);
        }}
      >
        Write
      </button>
      {open ? (
        <StepUpDialog
          open
          fieldID="probe"
          busy={false}
          failed={false}
          onSubmit={() => {
            setOpen(false);
          }}
          onCancel={() => {
            setOpen(false);
          }}
        />
      ) : null}
    </>
  );
}

/** The same, for the confirmation the destructive bulk actions pass through. */
function ConfirmHarness() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button
        type="button"
        id="opener"
        onClick={() => {
          setOpen(true);
        }}
      >
        Delete
      </button>
      {open ? (
        <ConfirmDialog
          open
          titleKey="builds.cleanup.confirm"
          detail="12 records"
          confirmLabel="Confirm"
          busy={false}
          onConfirm={() => {
            setOpen(false);
          }}
          onCancel={() => {
            setOpen(false);
          }}
        />
      ) : null}
    </>
  );
}

/** Opens the harness's dialog and answers with it and the control that opened it. */
function raise(harness: ReactNode, selector: string): { dialog: HTMLElement; opener: HTMLElement } {
  const container = mount(harness);
  const opener = container.querySelector('#opener') as HTMLElement;
  press(opener);
  const dialog = container.querySelector(selector);
  expect(dialog, `no dialog matching ${selector} was raised`).not.toBeNull();
  return { dialog: dialog as HTMLElement, opener };
}

describe('the credential prompt hands the reader back to the control that opened it', () => {
  it('opens modally, so the page behind it is inert', () => {
    const { dialog } = raise(<StepUpHarness />, 'dialog.stepup-dialog');
    // jsdom has no showModal, so what is asserted is the fallback the hook
    // takes in its place: the element is open, which is the part a test in this
    // environment can see at all.
    expect(dialog.hasAttribute('open')).toBe(true);
  });

  it('returns focus after a credential is submitted', () => {
    const { dialog, opener } = raise(<StepUpHarness />, 'dialog.stepup-dialog');
    press(control(dialog, 'Sign In'));
    expect(document.querySelector('dialog.stepup-dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it('returns focus after the prompt is cancelled', () => {
    const { dialog, opener } = raise(<StepUpHarness />, 'dialog.stepup-dialog');
    press(control(dialog, 'Cancel'));
    expect(document.querySelector('dialog.stepup-dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it('returns focus when the prompt is dismissed with Escape', () => {
    const { dialog, opener } = raise(<StepUpHarness />, 'dialog.stepup-dialog');
    escape(dialog);
    expect(document.querySelector('dialog.stepup-dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });
});

describe('the confirmation hands the reader back to the control that opened it', () => {
  it('opens modally, so the list behind it is inert', () => {
    const { dialog } = raise(<ConfirmHarness />, 'dialog.confirm-dialog');
    expect(dialog.hasAttribute('open')).toBe(true);
  });

  it('returns focus after the action is confirmed', () => {
    const { dialog, opener } = raise(<ConfirmHarness />, 'dialog.confirm-dialog');
    press(control(dialog, 'Confirm'));
    expect(document.querySelector('dialog.confirm-dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it('returns focus after the confirmation is cancelled', () => {
    const { dialog, opener } = raise(<ConfirmHarness />, 'dialog.confirm-dialog');
    press(control(dialog, 'Cancel'));
    expect(document.querySelector('dialog.confirm-dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it('returns focus when the confirmation is dismissed with Escape', () => {
    const { dialog, opener } = raise(<ConfirmHarness />, 'dialog.confirm-dialog');
    escape(dialog);
    expect(document.querySelector('dialog.confirm-dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });
});
