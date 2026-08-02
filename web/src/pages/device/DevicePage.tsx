import { useEffect, useRef, useState } from 'react';
import { Link, useHref, useLocation } from 'react-router';

import type { ApiOutcome } from '../../api/client';
import { api } from '../../api/endpoints';
import { resolveState } from '../../api/state';
import type { IAMMe } from '../../api/types';
import { busyAttributes } from '../../api/write';
import { LanguageControl } from '../../app/LanguageControl';
import type { PageProps } from '../../app/routes';
import { useMessages } from '../../i18n/context';
import type { MessageKey } from '../../i18n/messages';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import { loginURL } from '../login/urls';
import { createDecisionGate, normalizeUserCode } from './decision';
import type { Decision, DecisionGate } from './decision';

/**
 * Authorize a CLI device code.
 *
 * Three things this page already did better than the rest of the console, and
 * all three are carried over rather than re-derived: the decision is guarded so
 * a double click, a burst of activations and a same-tick approve-and-deny each
 * produce exactly one backend call (decision.ts); the refusal is bound to the
 * control it is about with aria-describedby, role="alert" and aria-invalid; and
 * the result's state is a `data-state` attribute, so the stylesheet and a screen
 * reader read the same value instead of a class and a sentence that can drift.
 *
 * What is fixed here is the branch that had nowhere to go. A local administrator
 * session cannot approve a device code — the CLI would end up holding a
 * federated capability minted from a local one — and the page said so by
 * returning early: Approve and Deny stayed inert, Retry stayed hidden, the
 * result line stayed empty, and the only link on the card went to the console.
 * A refusal that offers no way out reads as a broken page rather than a refused
 * one, and `/login` has supported `?return_to=` the whole time. So the refusal
 * now states itself and carries the trip that satisfies it, with this page and
 * its code in the return path.
 */

/** What the result line is currently saying, and how the stylesheet dresses it. */
type ResultCode = '' | 'required' | 'stepup' | 'invalid' | 'request-fail' | 'approved' | 'denied';

/**
 * One table, so a result cannot be worded one way and coloured another.
 *
 * `data-state` carries the second column. The empty state is a real member: the
 * line holds its height whether or not it is saying anything, so a result
 * arriving does not move the buttons that produced it.
 */
const RESULTS: Record<
  Exclude<ResultCode, ''>,
  { key: MessageKey; state: 'error' | 'success' | '' }
> = {
  required: { key: 'device.federated.required', state: 'error' },
  // Not an error: the reader is being handed to their provider and will come
  // back. Colouring it red would report a failure that has not happened.
  stepup: { key: 'device.stepup', state: '' },
  invalid: { key: 'device.invalid', state: 'error' },
  'request-fail': { key: 'device.request.fail', state: 'error' },
  approved: { key: 'device.approved', state: 'success' },
  denied: { key: 'device.denied', state: 'success' },
};

/** The authentication `internal/iam` stamps on a session established federally. */
const FEDERATED_SESSION = 'federated-session';

export function DevicePage({ route, boot, onLanguageChange }: PageProps) {
  const messages = useMessages();
  useDocumentTitle(route.titleKey);
  const here = useLocation();
  const devicePath = useHref('/device');
  const loginPath = useHref('/login');
  // One gate for the life of the page, and for both buttons: they write the same
  // resource, so the flag that says "a decision is already on its way" belongs
  // to the resource and not to whichever control was pressed. Built through the
  // state initialiser rather than in an effect, because the first click can
  // arrive before an effect has run.
  const [gate] = useState<DecisionGate>(() =>
    createDecisionGate((userCode, decision) => api.deviceDecision(userCode, decision)),
  );

  // The URL is the authority for the code and the boot payload is the fallback:
  // both were normalised the same way (upper-cased and trimmed — the code is
  // read aloud off another screen, so case and stray spaces are the reader's),
  // but only the URL is still right after a client-side navigation.
  const fromQuery = new URLSearchParams(here.search).get('user_code') ?? '';
  const [code, setCode] = useState(
    fromQuery.trim() === '' ? boot.route.user_code : fromQuery.trim().toUpperCase(),
  );
  const [result, setResult] = useState<ResultCode>('');
  const [invalid, setInvalid] = useState(false);
  const [busy, setBusy] = useState(false);
  const [settled, setSettled] = useState(false);
  const [reload, setReload] = useState(0);
  const [identityOutcome, setIdentityOutcome] = useState<ApiOutcome<IAMMe> | null>(null);
  const codeRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const controller = new AbortController();
    void api.iamMe({ signal: controller.signal }).then((outcome) => {
      if (!controller.signal.aborted) {
        setIdentityOutcome(outcome);
      }
    });
    return () => {
      controller.abort();
    };
  }, [reload]);

  // The five states, decided once and not re-derived here. A step-up refusal and
  // a 401 both land as `error`, which is the right screen for both: the identity
  // could not be confirmed, and the answer is the same retry.
  const identity = resolveState({ outcome: identityOutcome });
  const principal = identity.state === 'ok' ? identity.data.principal : null;
  const federated = principal?.authentication === FEDERATED_SESSION;
  // Not stored. The refusal is a property of who the reader is, so a retry that
  // lands on a federated principal clears it with no second code path — and a
  // decision can never have run while it holds, because both actions are inert.
  const refused = identity.state === 'ok' && !federated;
  const shown: ResultCode = refused ? 'required' : result;
  const shownResult = shown === '' ? null : RESULTS[shown];

  const identityText = (): string => {
    if (identity.state === 'ok') {
      const name = principal?.preferred_username ?? principal?.subject ?? '-';
      const projects = identity.data.projects ?? [];
      return (
        messages.t('device.identity') +
        name +
        messages.t('device.projects') +
        // A count, so it goes through the locale formatter. The console this
        // replaces called String() here and printed grouping-free ASCII on a
        // page whose every other number was formatted.
        messages.number(projects.length)
      );
    }
    if (identity.state === 'error') {
      return messages.t('device.identity.fail');
    }
    return messages.t('device.identity.loading');
  };

  const decide = (decision: Decision): void => {
    // Fails closed: until IAM has confirmed a federated principal there is
    // nothing to send, and the guard is here as well as on the control because
    // aria-disabled leaves a control activatable on purpose.
    if (!federated || gate.settled() || gate.pending()) {
      return;
    }
    const userCode = normalizeUserCode(code);
    if (userCode === '') {
      setInvalid(true);
      setResult('invalid');
      codeRef.current?.focus();
      return;
    }
    // What was sent is what the reader is left looking at, hyphen and all.
    setCode(userCode);
    setInvalid(false);
    setResult('');
    setBusy(true);
    void gate
      .decide(userCode, decision)
      .then((outcome) => {
        if (outcome === null) {
          // Discarded. The activation that is in flight owns the screen.
          return;
        }
        // Two spellings of the same refusal. The client turns a 428 carrying
        // `code: step_up_required` into its own outcome; the device decision
        // proxy answers 428 without that body, and both mean the control plane
        // wants fresh authentication before it mints the CLI's own credential.
        if (outcome.kind === 'step-up' || (outcome.kind === 'error' && outcome.status === 428)) {
          setResult('stepup');
          // The return trip carries the code, which the reader would otherwise
          // have to read off the terminal and type a second time.
          window.location.href = loginURL({
            loginPath,
            stepUp: true,
            returnTo: `${devicePath}?user_code=${encodeURIComponent(userCode)}`,
          });
          return;
        }
        if (outcome.kind !== 'ok') {
          setResult('request-fail');
          return;
        }
        setResult(decision === 'approve' ? 'approved' : 'denied');
        gate.settle();
        setSettled(true);
      })
      .finally(() => {
        setBusy(false);
      });
  };

  const actionsEnabled = federated && !settled && !busy;

  return (
    <div className="auth-wrap">
      <main className="auth-card device-card">
        <p className="brand">Portage Engine</p>
        <h1>{messages.t('device.h1')}</h1>
        <p className="device-intro">{messages.t('device.intro')}</p>
        <form
          noValidate
          onSubmit={(event) => {
            // The card has two submits and neither is the default one. Approve
            // and Deny are the decision and they are deliberately not typed
            // `submit`, so an Enter in the code field must not pick one of them.
            event.preventDefault();
          }}
        >
          <div className="field">
            <label htmlFor="device-code">{messages.t('device.code')}</label>
            {/* The class is what puts this in the monospace face: `.field
                input.device-code` is declared after the typed-input rule at
                equal specificity in surfaces.css and wins on order. It is the
                one string on the page a reader compares with their terminal
                character by character, and the code's alphabet excludes I, O, 0
                and 1 precisely so that comparison can be made — which only
                works in the face the terminal is drawing. */}
            <input
              ref={codeRef}
              className="device-code"
              type="text"
              id="device-code"
              name="user_code"
              value={code}
              onChange={(event) => {
                setCode(event.target.value);
                setInvalid(false);
              }}
              readOnly={settled}
              autoComplete="one-time-code"
              autoCapitalize="characters"
              autoCorrect="off"
              spellCheck={false}
              maxLength={9}
              aria-describedby="device-hint device-result"
              aria-invalid={invalid}
            />
            <p className="hint" id="device-hint">
              {messages.t('device.hint')}
            </p>
          </div>
          {/* A node whose only job is to carry this sentence, which is what
              makes the live region safe: the component list around it is not
              re-announced because it is not inside it. Two lines are reserved
              because that is what a real principal string takes. */}
          <p className="device-identity" id="device-identity" role="status">
            {identityText()}
          </p>
          {identity.state === 'error' ? (
            <button
              className="btn"
              type="button"
              onClick={() => {
                // Back to loading here rather than inside the effect: the reader
                // asked, so the answer they are looking at stops being current
                // at the moment of the click and not a render later.
                setIdentityOutcome(null);
                setReload((previous) => previous + 1);
              }}
            >
              {messages.t('device.retry')}
            </button>
          ) : null}
          <div className="device-actions">
            {/* Two columns, never two stacked bars. `.device-actions .btn` sets
                `width: auto` against the card's own full-width button rule, and
                that is the whole reason Deny does not become an identical bar
                directly under Approve — which is the adjacency a two-column
                layout exists to prevent for a destructive action.
                aria-disabled and not disabled: a control that leaves the tab
                order mid-operation loses the keyboard reader who just used it,
                and the guard in `decide` is what actually refuses the click. */}
            <button
              className="btn blue"
              type="button"
              id="device-approve"
              onClick={() => {
                decide('approve');
              }}
              {...busyAttributes(busy)}
              aria-disabled={actionsEnabled ? 'false' : 'true'}
            >
              {messages.t('device.approve')}
            </button>
            <button
              className="btn danger"
              type="button"
              id="device-deny"
              onClick={() => {
                decide('deny');
              }}
              {...busyAttributes(busy)}
              aria-disabled={actionsEnabled ? 'false' : 'true'}
            >
              {messages.t('device.deny')}
            </button>
          </div>
          <p
            className="device-result"
            id="device-result"
            role="alert"
            data-state={shownResult?.state ?? ''}
          >
            {shownResult === null ? '' : messages.t(shownResult.key)}
          </p>
          {refused ? (
            // Below the sentence that refused, and outside `.device-actions`: it
            // is the answer to the refusal, not a third peer of the pair above
            // that cannot be used. The return path carries whatever is in the
            // field rather than whatever was in the URL, so a code typed by hand
            // survives the trip too and the reader lands back here with it
            // instead of on /overview reading it off the terminal again.
            <a
              className="btn blue"
              href={loginURL({
                loginPath,
                stepUp: false,
                returnTo:
                  code === '' ? devicePath : `${devicePath}?user_code=${encodeURIComponent(code)}`,
              })}
            >
              {messages.t('device.federated.signin')}
            </a>
          ) : null}
        </form>
        <p className="auth-note">
          <Link to="/overview">{messages.t('device.console')}</Link>
          {/* A `bare` route has no chrome to carry this, and the code in the
              field survives the switch because nothing here reloads. */}
          <LanguageControl onChange={onLanguageChange} />
        </p>
      </main>
    </div>
  );
}
