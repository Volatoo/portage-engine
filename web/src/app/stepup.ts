/**
 * What the console does when a write is answered with 428.
 *
 * `stepUpRequired` in internal/server/iam.go asks for a fresh credential before
 * `/api/v1/builds/delete`, `/api/v1/iam/sessions/revoke-all`,
 * `/api/v1/builds/cleanup-failed` and every non-GET under `/api/v1/settings/`.
 * That is Delete Job, Clean Up Failed, Save, Test Connection, Start Test Build
 * and Revoke All — six controls whose whole purpose is to be clicked.
 *
 * The console this replaces handled it centrally in `api()`: a federated session
 * went to the provider with `prompt=login`, a local one was asked for
 * administrator credentials, and — the part that matters — the original request
 * was retried once with the credential in hand. Turning the refusal into a
 * sentence beside the button loses that: the reader is told what the control
 * plane wants and given no way to give it.
 *
 * The decision is here, as a function over the outcome, so all four branches are
 * stated once and can be asserted without a browser. Asking, and where to send a
 * federated reader, are the caller's — they are a dialog and a navigation, and
 * neither belongs in a transport rule.
 */

import { createElement, useCallback, useEffect, useId, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { useHref, useLocation } from 'react-router';

import type { ApiOutcome } from '../api/client';
import type { StepUpMethod } from '../api/types';
import { api } from '../api/endpoints';
import { loginURL } from '../pages/login/urls';
import { StepUpDialog } from './StepUpDialog';
import type { Credentials } from './StepUpDialog';

export interface StepUpHandlers {
  /**
   * Obtains a fresh local credential. Resolves true once one is established and
   * false when the reader declined, which is a refusal to be reported, not an
   * error to be raised.
   */
  elevate: () => Promise<boolean>;
  /** Hands the reader to the identity provider for a fresh session. */
  reauthenticate: () => void;
  /**
   * How THIS session was established, for the 428s that do not say.
   *
   * The control plane's own step-up refusal carries no `method`, so without
   * this every deployment would be treated alike — and treating them alike is
   * what a federated default did: a legacy or local operator lost a filled-in
   * settings form to a navigation that only a provider round-trip needs.
   */
  sessionMethod: () => StepUpMethod;
}

/**
 * The credential that can satisfy a step-up, given how the session was made.
 *
 * `principal.authentication` in internal/server/iam.go spells four kinds. Two
 * of them are held by an identity provider and can only be refreshed there;
 * the other two are held by this deployment and can be re-entered in place.
 * internal/dashboard/ui.go:1712 split them the same way and on the same field.
 */
export function stepUpMethodFor(authentication: string): StepUpMethod {
  return authentication === 'oidc' || authentication === 'federated-session'
    ? 'federated'
    : 'local';
}

/**
 * Run a write; satisfy a step-up refusal and run it again, exactly once.
 *
 * Once, deliberately: a control plane that answers the retry with a second 428
 * is not going to accept a third attempt, and a loop here would be a credential
 * prompt the reader cannot dismiss. The second outcome is returned whatever it
 * is, so the write site reports it the same way it reports any other refusal.
 */
export async function withStepUp<T>(
  run: () => Promise<ApiOutcome<T>>,
  handlers: StepUpHandlers,
): Promise<ApiOutcome<T>> {
  const first = await run();
  if (first.kind !== 'step-up') {
    return first;
  }
  const method = first.method === 'unstated' ? handlers.sessionMethod() : first.method;
  if (method === 'unavailable') {
    // Not a failure to authenticate: the deployment holds no credential that
    // could satisfy this. Asking for one would be asking for something that
    // cannot work, so the server's own sentence is all there is.
    return first;
  }
  if (method === 'federated') {
    handlers.reauthenticate();
    return first;
  }
  const elevated = await handlers.elevate();
  if (!elevated) {
    return first;
  }
  return run();
}

/**
 * Issues a write. A step-up refusal is satisfied where the write was made —
 * inline for a local session, at the provider for a federated one — and the
 * write is repeated once with the credential in hand.
 *
 * Named on its own because it travels on its own: a write site inside a form
 * takes the guard as a prop and leaves the prompt to be rendered by whoever owns
 * the page, which is the only place a prompt can be rendered without its submit
 * reaching that form's own handler.
 */
export type StepUpGuard = <T>(run: () => Promise<ApiOutcome<T>>) => Promise<ApiOutcome<T>>;

export interface StepUp {
  guard: StepUpGuard;
  /** The credential prompt. Renders nothing until one is being asked for. */
  prompt: ReactNode;
}

/**
 * `withStepUp` bound to this page: a dialog for the asking, and this page's own
 * address for the trip through the identity provider.
 *
 * `guard` never changes identity, because the writers that call it are built
 * once and must not be rebuilt — a rebuilt `singleWriter` comes back with its
 * in-flight flag clear, which is the second activation the flag exists to
 * discard. Everything that does change is read through a ref.
 */
export function useStepUp(authentication: string): StepUp {
  const fieldID = useId();
  const loginPath = useHref('/login');
  const here = useHref(useLocation());

  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  /** Resolves the promise `elevate` is waiting on: true elevated, false declined. */
  const answer = useRef<((elevated: boolean) => void) | null>(null);
  const target = useRef({ loginPath, here, authentication });
  useEffect(() => {
    target.current = { loginPath, here, authentication };
  });

  const settle = useCallback((elevated: boolean) => {
    const waiting = answer.current;
    answer.current = null;
    setAsking(false);
    setBusy(false);
    waiting?.(elevated);
  }, []);

  const guard = useCallback(
    <T>(run: () => Promise<ApiOutcome<T>>): Promise<ApiOutcome<T>> =>
      withStepUp(run, {
        elevate: () =>
          new Promise<boolean>((resolve) => {
            answer.current = resolve;
            setFailed(false);
            setAsking(true);
          }),
        sessionMethod: () => stepUpMethodFor(target.current.authentication),
        reauthenticate: () => {
          // prompt=login at the provider and then back to the page the write was
          // made from, so the reader lands where they were rather than on an
          // overview page and a second navigation.
          const trip = target.current;
          window.location.href = loginURL({
            loginPath: trip.loginPath,
            stepUp: true,
            returnTo: trip.here,
          });
        },
      }),
    [],
  );

  const submit = useCallback(
    (credentials: Credentials) => {
      if (busy) {
        // Discarded, never queued: a second /auth/step-up would be a second
        // credential check for one activation.
        return;
      }
      setBusy(true);
      void api.stepUp(credentials.username, credentials.password).then((outcome) => {
        if (outcome.kind === 'ok') {
          settle(true);
          return;
        }
        // The dialog stays up. A mistyped password is the common case and
        // closing on it would throw away the write the reader was making.
        setBusy(false);
        setFailed(true);
      });
    },
    [busy, settle],
  );

  return {
    guard,
    // Portalled to the body rather than left where it was declared. The prompt
    // carries a form of its own, and a form inside a form is not a thing the
    // platform has.
    //
    // The portal answers the DOCUMENT half of that and only that half: it moves
    // the dialog, and it leaves this node exactly where it was declared in the
    // React tree, which is the tree React propagates a submit along. So a
    // prompt raised by a write site inside a form still reaches that form's
    // onSubmit — a full unrequested PUT of the settings page, whose success
    // empties the four secret inputs. The other half is the caller's: a write
    // site that lives inside a form takes `guard` alone and leaves the prompt
    // to the page, which renders it outside.
    prompt: asking
      ? createPortal(
          createElement(StepUpDialog, {
            open: true,
            fieldID,
            busy,
            failed,
            onSubmit: submit,
            onCancel: () => {
              settle(false);
            },
          }),
          document.body,
        )
      : null,
  };
}
