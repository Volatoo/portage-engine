import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useHref, useLocation, useParams } from 'react-router';

import { api } from '../../api/endpoints';
import { openShellSocket } from '../../api/stream';
import type { ShellConnectionState, ShellSocket } from '../../api/stream';
import { busyAttributes, isCompositionEnter } from '../../api/write';
import { LanguageControl } from '../../app/LanguageControl';
import type { PageProps } from '../../app/routes';
import { useMessages } from '../../i18n/context';
import type { MessageKey } from '../../i18n/messages';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import { createFormWriter, formField } from '../login/formWriter';
import { loginURL } from '../login/urls';
import { shellGate } from './preflight';
import { TERMINAL_OPTIONS, loadTerminal } from './xterm';
import type { VendoredTerminal } from './xterm';
import '../../styles/pages/shell.css';

/**
 * The instance terminal.
 *
 * Two things it got wrong. The screen is pinned to 220 columns because
 * internal/server/handlers_shell.go opens the SSH command with `stty cols 220
 * rows 50` hardcoded, and the page put that screen in a box with no
 * `overflow-x` — so the document itself scrolled sideways at every width
 * (documentElement.scrollWidth measured 1731 at both 1280 and 375), which
 * carried the Back control off the right edge of a phone with no way to reach
 * it. Containing the screen in its own scroll container and keeping the head
 * sticky is the half of that fix which needs no protocol change; the other half
 * is a resize message on the socket and a server that reads it, and that is
 * recorded as a handoff rather than half-implemented here against a server that
 * would ignore it.
 *
 * And the socket was opened straight into the control plane's step-up
 * requirement. A refused upgrade reaches page script as an untyped error event
 * with no status and no body, so a reader was told "connection error" for a
 * shell that was one re-authentication away from opening. `/api/shell/preflight`
 * answers the same question over plain HTTP, where the refusal can name the
 * credential that would satisfy it, and the socket is gated on it.
 */

type Phase =
  | 'authorizing'
  /** A local session, which may re-state its password without leaving the page. */
  | 'step-up'
  /** Preflight said yes; the terminal owns the region below the head. */
  | 'terminal'
  /** Nothing left to try. The status line carries the reason. */
  | 'refused';

interface Status {
  key: MessageKey;
  tone: 'ok' | 'err' | '';
}

/** The socket's own vocabulary, in the words the head shows. */
const CONNECTION_STATUS: Record<ShellConnectionState, Status> = {
  connecting: { key: 'shell.authorizing', tone: '' },
  connected: { key: 'shell.connected', tone: 'ok' },
  closed: { key: 'shell.closed', tone: 'err' },
  error: { key: 'shell.error', tone: 'err' },
};

export function ShellPage({ route, boot, onLanguageChange }: PageProps) {
  const messages = useMessages();
  useDocumentTitle(route.titleKey);
  const params = useParams();
  const here = useLocation();
  const loginPath = useHref('/login');
  // The console's own address for this page, basename included, so the trip
  // through the provider comes back to the terminal and not to /overview.
  const shellPath = useHref(here);

  const [phase, setPhase] = useState<Phase>('authorizing');
  const [status, setStatus] = useState<Status>({ key: 'shell.authorizing', tone: '' });
  const [busy, setBusy] = useState(false);
  const termRef = useRef<HTMLDivElement>(null);

  // The URL is the authority; the boot payload is what let this frame render at
  // all before the router had said the same thing.
  const instanceID = params['instanceID'] ?? boot.route.instance_id;

  useEffect(() => {
    let abandoned = false;
    // Asked outside every project, and not by omission.
    //
    // Eight other surfaces had lost the project header and were given it back;
    // this one is checked and stays without. `/api/shell/preflight` is answered
    // by handleShellPreflight in internal/dashboard/dashboard.go, which resolves
    // no project on either of its branches: a local session is decided against
    // the deployment's own step-up key, the admin credentials and the session
    // cookie, and a federated one against `principal.step_up` from the control
    // plane's /api/v1/iam/me — and handleIAMMe in internal/server/iam.go never
    // reaches authorizeProject, which is the only thing that resolves a
    // selector. The socket this gates is the same: handlers_shell.go matches
    // the instance by its id across every instance the control plane holds.
    //
    // So the question is "may this session open a terminal", which has no
    // project in it. A header would be forwarded upstream by serverGet and
    // ignored there, and reading the project scope here would make this page
    // wait for a selection that no part of its answer depends on.
    void api.shellPreflight().then((outcome) => {
      if (abandoned) {
        return;
      }
      const gate = shellGate(outcome);
      switch (gate.next) {
        case 'open':
          setPhase('terminal');
          return;
        case 'local-step-up':
          setPhase('step-up');
          setStatus({ key: 'stepup.required', tone: 'err' });
          return;
        case 'sign-in':
          window.location.href = loginURL({ loginPath, stepUp: false, returnTo: shellPath });
          return;
        case 'reauthenticate':
          setStatus({ key: gate.statusKey, tone: '' });
          // prompt=login at the provider and then back to this shell rather
          // than to the console, so the terminal the reader asked for is what
          // they land on instead of an overview page and a second navigation.
          window.location.href = loginURL({ loginPath, stepUp: true, returnTo: shellPath });
          return;
        case 'refused':
          setPhase('refused');
          setStatus({ key: gate.statusKey, tone: 'err' });
          return;
      }
    });
    return () => {
      abandoned = true;
    };
  }, [loginPath, shellPath]);

  useEffect(() => {
    if (phase !== 'terminal') {
      return undefined;
    }
    const container = termRef.current;
    if (container === null) {
      return undefined;
    }
    let disposed = false;
    let terminal: VendoredTerminal | null = null;
    let socket: ShellSocket | null = null;
    void loadTerminal()
      .then((Terminal) => {
        if (disposed) {
          return;
        }
        terminal = new Terminal(TERMINAL_OPTIONS);
        terminal.open(container);
        const attached = terminal;
        socket = openShellSocket(instanceID, {
          onState: (state) => {
            setStatus(CONNECTION_STATUS[state]);
            if (state === 'connected') {
              attached.focus();
            }
          },
          onData: (chunk) => {
            attached.write(chunk);
          },
        });
        const open = socket;
        attached.onData((data) => {
          open.send(data);
        });
      })
      .catch(() => {
        if (!disposed) {
          // The vendored script is served by the same origin that served this
          // page, so failing to fetch it is the same class of event as losing
          // the socket and takes the same sentence.
          setStatus({ key: 'shell.error', tone: 'err' });
        }
      });
    return () => {
      disposed = true;
      socket?.close();
      terminal?.dispose();
    };
  }, [phase, instanceID]);

  // One writer for the one credential this page can establish. A held Enter and
  // a double click both produce a single /auth/step-up, and a discarded
  // activation is discarded rather than replayed after the password field has
  // been cleared by the re-render the first one caused.
  const [stepUpWriter] = useState(() =>
    createFormWriter(async (form: HTMLFormElement): Promise<void> => {
      const fields = new FormData(form);
      const outcome = await api.stepUp(
        formField(fields, 'username'),
        formField(fields, 'password'),
      );
      if (outcome.kind === 'ok') {
        setPhase('terminal');
        setStatus({ key: 'shell.authorizing', tone: '' });
        return;
      }
      setStatus({ key: 'stepup.failed', tone: 'err' });
    }),
  );

  const submitStepUp = (event: FormEvent<HTMLFormElement>): void => {
    event.preventDefault();
    if (stepUpWriter.pending()) {
      return;
    }
    setBusy(true);
    void stepUpWriter.submit(event.currentTarget).finally(() => {
      setBusy(false);
    });
  };

  return (
    <div className="shell-page">
      <div className="shell-head">
        <Link className="btn" to="/monitor">
          {messages.t('shell.back')}
        </Link>
        <span className="brand">{messages.t('shell.title')}</span>
        <span className="iid" title={instanceID}>
          {instanceID}
        </span>
        {/* A node that exists to carry one word. role="status" is already
            aria-live="polite", and it sits on a span holding nothing else — the
            terminal beside it is not inside the region, so a connection change
            does not re-announce a screenful of shell output. */}
        <span
          className={status.tone === '' ? 'save-msg' : `save-msg ${status.tone}`}
          id="shell-status"
          role="status"
        >
          {messages.t(status.key)}
        </span>
        {/* A `bare` route has no chrome to carry this. Switching costs nothing
            here: the terminal effect keys on the phase and the instance, so the
            socket and its scrollback survive a language change. */}
        <LanguageControl onChange={onLanguageChange} />
      </div>
      {phase === 'terminal' ? (
        <div id="term" ref={termRef} />
      ) : (
        <div className="shell-gate">
          {phase === 'step-up' ? (
            // Re-stating a password is not something to send the reader away
            // for, and it is not something to ask for with window.prompt()
            // either — which is what the console this replaces did, twice in a
            // row: a password in a plain-text box with no label, no autocomplete
            // for a password manager to see, and no way back to the first field
            // once the second had opened. /auth/step-up speaks JSON and answers
            // no page, so this form carries no action and says so.
            <form
              className="shell-stepup"
              onSubmit={submitStepUp}
              onKeyDown={(event) => {
                if (
                  isCompositionEnter({
                    key: event.key,
                    isComposing: event.nativeEvent.isComposing,
                  })
                ) {
                  event.preventDefault();
                }
              }}
            >
              <div className="field">
                <label htmlFor="shell-stepup-user">{messages.t('stepup.user')}</label>
                <input
                  type="text"
                  id="shell-stepup-user"
                  name="username"
                  autoComplete="username"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  aria-describedby="shell-status"
                />
              </div>
              <div className="field">
                <label htmlFor="shell-stepup-pass">{messages.t('stepup.password')}</label>
                <input
                  type="password"
                  id="shell-stepup-pass"
                  name="password"
                  autoComplete="current-password"
                  aria-describedby="shell-status"
                />
              </div>
              <button className="btn blue" type="submit" {...busyAttributes(busy)}>
                {messages.t('login.submit')}
              </button>
            </form>
          ) : null}
        </div>
      )}
    </div>
  );
}
