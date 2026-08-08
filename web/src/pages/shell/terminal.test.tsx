import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { describe, expect, it } from 'vitest';

import type { ApiOutcome } from '../../api/client';
import type { ShellPreflight } from '../../api/types';
import { CONSOLE_BASE, CONSOLE_ROUTES } from '../../app/routes';
import type { ConsoleRoute } from '../../app/routes';
import type { BootPayload } from '../../boot/payload';
import { MessagesProvider } from '../../i18n/context';
import type { Language } from '../../i18n/messages';
import { ShellPage } from './ShellPage';
import { shellGate } from './preflight';
import { TERMINAL_COLUMNS, TERMINAL_OPTIONS, TERMINAL_ROWS } from './xterm';

const SHELL_ROUTE = CONSOLE_ROUTES.find((route) => route.name === 'shell') as ConsoleRoute;

function boot(): BootPayload {
  return {
    lang: 'en',
    html_lang: 'en',
    auth_enabled: true,
    oidc_enabled: false,
    local_login_enabled: true,
    identity_providers: [],
    principal: null,
    step_up_method: 'local',
    route: {
      name: 'shell',
      path: '/shell/i-42',
      job_id: '',
      instance_id: 'i-42',
      user_code: '',
    },
    asset_base: '/static/ui/',
  };
}

function paint(lang: Language): string {
  return renderToStaticMarkup(
    <MessagesProvider lang={lang}>
      <MemoryRouter initialEntries={['/shell/i-42']} basename={CONSOLE_BASE}>
        <ShellPage route={SHELL_ROUTE} boot={boot()} onLanguageChange={() => undefined} />
      </MemoryRouter>
    </MessagesProvider>,
  );
}

describe('the socket is gated on the preflight, not opened into a refusal', () => {
  it('opens when the credential is already fresh enough', () => {
    const ok: ApiOutcome<ShellPreflight> = {
      kind: 'ok',
      value: { step_up: true, method: 'local' },
    };
    expect(shellGate(ok)).toEqual({ next: 'open' });
  });

  it('sends a reader with no session to sign in rather than to a dead socket', () => {
    expect(shellGate({ kind: 'unauthorized' })).toEqual({ next: 'sign-in' });
  });

  it('asks a local session for its password here, without leaving the page', () => {
    expect(
      shellGate({ kind: 'step-up', method: 'local', message: 'fresh step-up required' }),
    ).toEqual({ next: 'local-step-up' });
  });

  it('sends a federated session back through its provider', () => {
    expect(
      shellGate({ kind: 'step-up', method: 'federated', message: 'fresh step-up required' }),
    ).toEqual({ next: 'reauthenticate', statusKey: 'shell.stepup.reauth' });
  });

  it('says so, and offers nothing, when the deployment holds no such credential', () => {
    // Distinct from every other refusal: there is no action that would work, so
    // inviting one would be inviting the reader to fail.
    expect(
      shellGate({
        kind: 'step-up',
        method: 'unavailable',
        message: 'this dashboard holds no step-up credential for the web shell',
      }),
    ).toEqual({ next: 'refused', statusKey: 'shell.stepup.unavailable' });
  });

  it('falls to the connection sentence for a preflight that simply failed', () => {
    expect(shellGate({ kind: 'error', status: 502, message: 'bad gateway' })).toEqual({
      next: 'refused',
      statusKey: 'shell.error',
    });
  });
});

describe('the head is reachable and the screen is contained', () => {
  it('renders Back and the instance before anything has been authorized', () => {
    const markup = paint('en');
    expect(markup).toContain('href="/monitor"');
    expect(markup).toContain('>Back<');
    expect(markup).toContain('i-42');
    // A node that carries the connection state and nothing else, so a state
    // change does not re-announce a screenful of shell output.
    expect(markup).toContain('id="shell-status"');
    expect(markup).toContain('role="status"');
    expect(markup).toContain('authorizing');
  });

  it('holds no terminal until the preflight has answered', () => {
    // Opening the socket first is what turned a step-up requirement into an
    // untyped error event with nothing in it to act on.
    expect(paint('en')).not.toContain('id="term"');
  });

  it('paints Chinese with no English frame in front of it', () => {
    const markup = paint('zh');
    expect(markup).toContain('返回');
    expect(markup).toContain('实例终端');
    expect(markup).not.toContain('>Back<');
  });
});

describe('the screen scrolls inside its own box', () => {
  const styles = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'styles');
  // Comments first: both files explain the number they no longer contain, and a
  // check that reads the explanation reads green whichever way the rule went.
  const read = (...name: string[]): string =>
    readFileSync(join(styles, ...name), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
  const page = read('pages', 'shell.css');
  const panels = read('panels.css');

  it('gives the terminal a scroll container of its own', () => {
    // The screen is wider than every viewport until a resize channel exists, so
    // this is what stops the document scrolling sideways at all widths and
    // carrying Back off the right edge with it.
    expect(panels).toMatch(/#term \{[^}]*overflow-x: auto/);
    expect(panels).toMatch(/\.shell-head \{[^}]*position: sticky/);
  });

  it('sizes the terminal by the space left over and not by a measured number', () => {
    // A head that wraps — which it does at 375px — makes any hardcoded head
    // height wrong, and a terminal 46px too tall puts a scrollbar back on the
    // document this page is being fixed for.
    expect(page).toMatch(/\.shell-page \{[^}]*height: 100vh/);
    expect(page).toMatch(/\.shell-page \{[^}]*flex-direction: column/);
    expect(page).not.toContain('calc(100vh');
    expect(page).toMatch(/#term \{[^}]*min-width: 0/);
    expect(page).toMatch(/#term \{[^}]*min-height: 0/);
  });

  it('keeps the gate the same shape as the terminal it stands in for', () => {
    expect(page).toMatch(/\.shell-gate \{[^}]*flex: 1 1 auto/);
  });
});

describe('the geometry matches the one the server is drawing at', () => {
  it('pins the screen to the size handlers_shell.go hardcodes', () => {
    // internal/server/handlers_shell.go opens the SSH command with
    // `stty cols 220 rows 50`. Any other value here renders a screen the remote
    // side is not producing, so this is not a preference until a resize message
    // exists on both ends.
    expect(TERMINAL_COLUMNS).toBe(220);
    expect(TERMINAL_ROWS).toBe(50);
    expect(TERMINAL_OPTIONS.cols).toBe(TERMINAL_COLUMNS);
    expect(TERMINAL_OPTIONS.rows).toBe(TERMINAL_ROWS);
  });
});
