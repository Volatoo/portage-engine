import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useHref, useLocation } from 'react-router';

import { request } from '../../api/client';
import { busyAttributes, isCompositionEnter } from '../../api/write';
import { LanguageControl } from '../../app/LanguageControl';
import type { PageProps } from '../../app/routes';
import { useMessages } from '../../i18n/context';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import { createFormWriter, formField } from './formWriter';
import { providerChoices } from './providers';
import { LOCAL_LOGIN_ENDPOINT, loginURL, safeReturnTo } from './urls';

/**
 * Sign in, as a form.
 *
 * The console this replaces had a form that was not one. Neither input carried a
 * `name`, the `<form>` carried neither `method` nor `action`, and every byte of
 * the submission was assembled in a submit handler — so a browser that ran no
 * script answered the Sign In button by navigating to `/login?`, which is the
 * same page with the credentials silently dropped. There is no `<noscript>`
 * anywhere in that console to say so. On `DEPLOYMENT_MODE=public` the local form
 * is not rendered at all and the provider anchor works without script, which is
 * why the failure only ever reached trusted-LAN installs — the deployments most
 * likely to be reached from a hardened browser in the first place.
 *
 * So the form here is a form: named controls, `method="post"`, and an `action`
 * that is the credential route rather than this page. The submit handler is an
 * enhancement over that, not a replacement for it — it reads the credentials out
 * of the form element with `FormData` and posts them to the form's own `action`,
 * so there is exactly one statement of where a credential goes and what it is
 * called, and the scripted and unscripted paths cannot disagree about either.
 *
 * The remaining half of that fix is the server's: handleLoginSubmit decodes
 * `application/json` only and answers a JSON body rather than a redirect, so an
 * unscripted POST reaches it and is rejected as a malformed request. That is
 * recorded as a handoff; the half that is this console's is here.
 */

/** internal/dashboard/dashboard.go handleLoginSubmit's response body. */
interface LocalLoginResult {
  token: string;
  user: string;
  /** The server's own safeReturnTo of what we sent. '' when there was nothing. */
  redirect_to: string;
}

/**
 * Why the credentials were refused, as a code rather than as a sentence.
 *
 * Storing the rendered string is what makes an error stop following the language
 * control: the reader switches to Chinese and the refusal they are looking at
 * stays in the language it was produced in. A code is re-read through the
 * message layer on every render, so it switches with everything else.
 */
type LoginFailure =
  | { kind: 'badcreds' }
  | { kind: 'rejected'; status: number }
  | { kind: 'transport'; detail: string };

export function LoginPage({ route, boot, onLanguageChange }: PageProps) {
  const messages = useMessages();
  const here = useLocation();
  const consoleHome = useHref('/overview');
  useDocumentTitle(route.titleKey);

  const [failure, setFailure] = useState<LoginFailure | null>(null);
  const [busy, setBusy] = useState(false);

  const query = new URLSearchParams(here.search);
  const returnTo = safeReturnTo(query.get('return_to') ?? '');
  const stepUp = query.get('step_up') === '1';
  const action = loginURL({ loginPath: LOCAL_LOGIN_ENDPOINT, stepUp: false, returnTo });

  // One anchor per provider the deployment initialised, as the card this
  // replaces renders one per entry in pageData's list. A single unnamed
  // destination is not a smaller version of that: `/auth/oidc/start` resolves
  // against whichever provider is configured first, so on a deployment with
  // three of them the other two are unreachable from here and the reader is sent
  // to one they may hold no account with, under a label that does not say which.
  const choices = providerChoices(boot.oidc_enabled, boot.identity_providers, { stepUp, returnTo });

  // Created once and shared by every activation of the one control that writes:
  // a held Enter, a double click and an Enter that only confirmed an IME
  // candidate all have to produce a single POST, and a second POST of the same
  // credentials is a second session for the reader and a second line in whatever
  // reads the audit log. Nothing is queued — a discarded activation is
  // discarded, never replayed behind the first.
  //
  // The form element is handed in by the event that caused the write, and every
  // byte of the request is read off it: the field names, the credentials and the
  // destination. So there is exactly one statement of where a credential goes
  // and what it is called, and the scripted path and the browser's own
  // submission cannot come to disagree about either.
  const [writer] = useState(() =>
    createFormWriter(async (form: HTMLFormElement): Promise<void> => {
      const fields = new FormData(form);
      const outcome = await request<LocalLoginResult>(
        form.getAttribute('action') ?? LOCAL_LOGIN_ENDPOINT,
        {
          method: 'POST',
          body: {
            username: formField(fields, 'username'),
            password: formField(fields, 'password'),
          },
        },
      );
      if (outcome.kind === 'ok') {
        // A full navigation, not a router push: the session cookie the response
        // just set is what the server reads to stamp the next boot payload, and
        // a client-side transition would render the console with the payload of
        // a reader who was not signed in yet. Sanitised again on the way out —
        // the value is the server's own, and it is still about to be navigated
        // to.
        const target = safeReturnTo(outcome.value.redirect_to);
        window.location.href = target === '' ? consoleHome : target;
        return;
      }
      if (outcome.kind === 'unauthorized') {
        setFailure({ kind: 'badcreds' });
      } else if (outcome.kind === 'error' && outcome.status === 0) {
        // Status 0 is the transport: the request never got an answer at all,
        // which is a different sentence from one the server refused.
        setFailure({ kind: 'transport', detail: outcome.message });
      } else {
        setFailure({ kind: 'rejected', status: outcome.kind === 'error' ? outcome.status : 0 });
      }
      // Back to the first field of the pair the refusal is about, reached
      // through the form rather than through a ref: the name is already the one
      // thing this page states once.
      const username = form.elements.namedItem('username');
      if (username instanceof HTMLInputElement) {
        username.focus();
      }
    }),
  );

  const submit = (event: FormEvent<HTMLFormElement>): void => {
    event.preventDefault();
    if (writer.pending()) {
      return;
    }
    setFailure(null);
    setBusy(true);
    void writer.submit(event.currentTarget).finally(() => {
      setBusy(false);
    });
  };

  const failureText = (): string => {
    if (failure === null) {
      return '';
    }
    if (failure.kind === 'badcreds') {
      return messages.t('login.badcreds');
    }
    if (failure.kind === 'transport') {
      return messages.t('login.neterr') + failure.detail;
    }
    // The status is a protocol token and not a quantity, so it is not put
    // through the locale number formatter: 428 is 428 in every language, and a
    // grouped or transliterated one would not be findable in a server log.
    return `${messages.t('login.fail')} (HTTP ${String(failure.status)})`;
  };

  const invalid = failure !== null;

  return (
    <div className="auth-wrap">
      <main className="auth-card">
        <p className="brand">Portage Engine</p>
        <h1>{messages.t('login.h1')}</h1>
        {choices.map((choice) => (
          <a className="btn blue" key={choice.key} href={choice.href}>
            {messages.t(choice.labelKey, choice.labelValues)}
          </a>
        ))}
        {choices.length > 0 && boot.local_login_enabled ? (
          <div className="auth-divider">{messages.t('login.or')}</div>
        ) : null}
        {boot.local_login_enabled ? (
          <form
            method="post"
            action={action}
            onSubmit={submit}
            onKeyDown={(event) => {
              // The Enter that closes an IME candidate window is not a
              // submission. Checked at the form so neither input has to
              // remember, and checked on the native event because React's
              // synthetic keyboard event does not carry isComposing.
              if (
                isCompositionEnter({ key: event.key, isComposing: event.nativeEvent.isComposing })
              ) {
                event.preventDefault();
              }
            }}
          >
            <div className="field">
              <label htmlFor="login-username">{messages.t('login.user')}</label>
              <input
                type="text"
                id="login-username"
                name="username"
                autoComplete="username"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                aria-describedby="login-error"
                aria-invalid={invalid}
              />
            </div>
            <div className="field">
              <label htmlFor="login-password">{messages.t('login.pass')}</label>
              <input
                type="password"
                id="login-password"
                name="password"
                autoComplete="current-password"
                aria-describedby="login-error"
                aria-invalid={invalid}
              />
            </div>
            {/* The refusal is about the pair and not about one box, so both
                inputs describe themselves with it; without the association it
                is a sentence sitting under a form rather than an answer to the
                field the reader is standing in. aria-invalid is the half the
                markup cannot carry — the describedby link is static, whether
                the pair is currently rejected is not. The node exists only to
                hold this sentence, which is what makes role="alert" safe here
                and what makes it unsafe on a container that also holds the
                page's content. It reserves its own line, so an arriving
                refusal does not move the button under the pointer. */}
            <p className="auth-err" id="login-error" role="alert">
              {failureText()}
            </p>
            <button className="btn blue" type="submit" {...busyAttributes(busy)}>
              {messages.t('login.submit')}
            </button>
          </form>
        ) : null}
        <p className="auth-note">
          <Link to="/">{messages.t('login.back')}</Link>
          {/* A `bare` route has no chrome to carry this, and a sign-in card is
              the one page a reader resolved into a language they cannot read is
              unable to get past in order to fix it. */}
          <LanguageControl onChange={onLanguageChange} />
        </p>
      </main>
    </div>
  );
}
