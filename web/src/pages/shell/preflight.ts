/**
 * What the console does about each answer `/api/shell/preflight` can give.
 *
 * The endpoint exists because a WebSocket handshake has no way to report a
 * refusal: a control plane that declines the upgrade reaches page script as an
 * untyped `error` event with no status and no body, so a shell that needed fresh
 * authentication looked exactly like a shell whose builder was gone. Asking over
 * plain HTTP first turns that into a named refusal — and this table turns the
 * refusal into the one thing the reader can do about it.
 *
 * Split out of the component so all five branches are stated once and can be
 * asserted without a browser. The console this replaces had them as a chain of
 * `if (body.method === ...)` inside an async function inside a `<script>`.
 */

import type { ApiOutcome } from '../../api/client';
import type { ShellPreflight } from '../../api/types';
import type { MessageKey } from '../../i18n/messages';

export type ShellGate =
  /** The credential is fresh enough. Open the socket. */
  | { next: 'open' }
  /** No session at all. The sign-in page, with this shell as the return trip. */
  | { next: 'sign-in' }
  /** A federated session that needs prompt=login at the provider. */
  | { next: 'reauthenticate'; statusKey: MessageKey }
  /** A local session that can re-state its password without leaving the page. */
  | { next: 'local-step-up' }
  /**
   * Nothing this deployment holds would satisfy the requirement, or the request
   * itself failed. Either way there is no action, only a sentence.
   */
  | { next: 'refused'; statusKey: MessageKey };

export function shellGate(outcome: ApiOutcome<ShellPreflight>): ShellGate {
  if (outcome.kind === 'ok') {
    return { next: 'open' };
  }
  if (outcome.kind === 'unauthorized') {
    return { next: 'sign-in' };
  }
  if (outcome.kind === 'step-up') {
    if (outcome.method === 'local') {
      return { next: 'local-step-up' };
    }
    if (outcome.method === 'federated') {
      return { next: 'reauthenticate', statusKey: 'shell.stepup.reauth' };
    }
    // `unavailable` is not a failure to authenticate; it is a deployment that
    // holds no credential capable of authenticating this, and telling the reader
    // to try again would be telling them to do something that cannot work.
    return { next: 'refused', statusKey: 'shell.stepup.unavailable' };
  }
  return { next: 'refused', statusKey: 'shell.error' };
}
