import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { describe, expect, it } from 'vitest';

import type { BootIdentityProvider, BootPayload } from '../../boot/payload';
import { MessagesProvider } from '../../i18n/context';
import { messagesFor } from '../../i18n/messages';
import type { Language } from '../../i18n/messages';
import { CONSOLE_BASE, CONSOLE_ROUTES } from '../../app/routes';
import type { ConsoleRoute } from '../../app/routes';
import { LoginPage } from './LoginPage';
import { providerChoices } from './providers';
import { LOCAL_LOGIN_ENDPOINT, loginURL, safeReturnTo } from './urls';

/**
 * Every assertion below is about the first paint or about a pure function,
 * because that is where each of these defects lived. The form that was not a
 * form was wrong in its markup before any handler ran; the English button on a
 * Chinese page was wrong in the string it was built from.
 */

const LOGIN_ROUTE = CONSOLE_ROUTES.find((route) => route.name === 'login') as ConsoleRoute;

function boot(overrides: Partial<BootPayload> = {}): BootPayload {
  return {
    lang: 'en',
    html_lang: 'en',
    auth_enabled: true,
    oidc_enabled: false,
    local_login_enabled: true,
    identity_providers: [],
    principal: null,
    step_up_method: 'local',
    route: { name: 'login', path: '/ui/login', job_id: '', instance_id: '', user_code: '' },
    asset_base: '/static/ui/',
    ...overrides,
  };
}

function provider(id: string, displayName: string, stepUp = true): BootIdentityProvider {
  return { id, display_name: displayName, supports_step_up: stepUp };
}

function paint(payload: BootPayload, lang: Language, at = '/ui/login'): string {
  return renderToStaticMarkup(
    <MessagesProvider lang={lang}>
      <MemoryRouter initialEntries={[at]} basename={CONSOLE_BASE}>
        <LoginPage route={LOGIN_ROUTE} boot={payload} onLanguageChange={() => undefined} />
      </MemoryRouter>
    </MessagesProvider>,
  );
}

describe('the sign-in form is a form', () => {
  it('posts named credentials to the credential route, not to itself', () => {
    const markup = paint(boot(), 'en');
    // The three facts a scriptless browser needs, and the three the console
    // this replaces had none of: a method, a destination, and a name on each
    // control. Without them the Sign In button navigated to `/login?` and threw
    // the credentials away without saying so.
    expect(markup).toContain('method="post"');
    expect(markup).toContain(`action="${LOCAL_LOGIN_ENDPOINT}"`);
    expect(markup).toContain('name="username"');
    expect(markup).toContain('name="password"');
    // And never a GET, which would put a password in the address bar, in the
    // history and in every proxy log between here and the server.
    expect(markup).not.toContain('method="get"');
  });

  it('carries the return path into the action, so the trip survives the POST', () => {
    const markup = paint(
      boot(),
      'en',
      '/ui/login?return_to=%2Fui%2Fdevice%3Fuser_code%3DABCD-2345',
    );
    expect(markup).toContain('action="/login?return_to=%2Fui%2Fdevice%3Fuser_code%3DABCD-2345"');
  });

  it('binds the refusal to both inputs and announces it from a node of its own', () => {
    const markup = paint(boot(), 'en');
    expect(markup.split('aria-describedby="login-error"').length - 1).toBe(2);
    expect(markup).toContain('id="login-error"');
    expect(markup).toContain('role="alert"');
    // Not rejected yet, and it says so rather than leaving the state unstated.
    expect(markup).toContain('aria-invalid="false"');
  });

  it('renders no local form at all when the deployment refuses local admin login', () => {
    const markup = paint(boot({ local_login_enabled: false, oidc_enabled: true }), 'en');
    expect(markup).not.toContain('name="password"');
    expect(markup).not.toContain('<form');
  });
});

describe('the federated control is written in the reader’s language', () => {
  it('is the only operable control on an SSO-only Chinese deployment, in Chinese', () => {
    const markup = paint(boot({ local_login_enabled: false, oidc_enabled: true }), 'zh');
    expect(markup).toContain('用身份提供商登录');
    expect(markup).toContain('href="/auth/oidc/start"');
    // The English string it replaced is nowhere in the page.
    expect(markup).not.toContain('Sign in with');
  });

  it('interpolates a named provider through the message layer in both languages', () => {
    const [choice] = providerChoices(true, [provider('authentik', 'Authentik')], {
      stepUp: false,
      returnTo: '',
    });
    expect(choice?.href).toBe('/auth/provider/authentik/start');
    expect(messagesFor('en').t(choice?.labelKey ?? 'login.oidc', choice?.labelValues)).toBe(
      'Sign in with Authentik',
    );
    expect(messagesFor('zh').t(choice?.labelKey ?? 'login.oidc', choice?.labelValues)).toBe(
      '使用 Authentik 登录',
    );
  });

  it('carries step_up and the return path onto every provider start URL', () => {
    const choices = providerChoices(true, [provider('authentik', 'Authentik')], {
      stepUp: true,
      returnTo: '/ui/shell/i-42',
    });
    expect(choices[0]?.href).toBe(
      '/auth/provider/authentik/start?step_up=1&return_to=%2Fui%2Fshell%2Fi-42',
    );
  });

  it('offers nothing federated when the deployment has no provider', () => {
    expect(providerChoices(false, [], { stepUp: false, returnTo: '' })).toEqual([]);
  });
});

describe('a multi-provider deployment is offered every provider it has', () => {
  const configured = [
    provider('authentik', 'Authentik'),
    provider('google', 'Google'),
    provider('github', 'GitHub', false),
  ];

  it('renders one anchor per provider, each at its own start route', () => {
    // Three providers, three destinations. A single /auth/oidc/start resolves
    // against whichever one is configured first, so it is not a smaller version
    // of this card — it is a card two of these three readers cannot sign in
    // through.
    const markup = paint(boot({ oidc_enabled: true, identity_providers: configured }), 'en');
    for (const [id, name] of [
      ['authentik', 'Authentik'],
      ['google', 'Google'],
      ['github', 'GitHub'],
    ]) {
      expect(markup).toContain(`href="/auth/provider/${id ?? ''}/start"`);
      expect(markup).toContain(`Sign in with ${name ?? ''}`);
    }
    expect(markup).not.toContain('/auth/oidc/start');
  });

  it('names each provider in the reader’s language, not in the server’s', () => {
    const markup = paint(
      boot({ local_login_enabled: false, oidc_enabled: true, identity_providers: configured }),
      'zh',
    );
    expect(markup).toContain('使用 Authentik 登录');
    expect(markup).toContain('使用 GitHub 登录');
    expect(markup).not.toContain('Sign in with');
  });

  it('drops a provider that cannot re-prove the reader from a step-up card', () => {
    const markup = paint(
      boot({ oidc_enabled: true, identity_providers: configured }),
      'en',
      '/ui/login?step_up=1&return_to=%2Fui%2Fshell%2Fi-42',
    );
    expect(markup).toContain(
      'href="/auth/provider/authentik/start?step_up=1&amp;return_to=%2Fui%2Fshell%2Fi-42"',
    );
    // GitHub has no prompt=login, so a button for it would hand back the very
    // session the control plane just refused.
    expect(markup).not.toContain('/auth/provider/github/start');
  });

  it('falls back to the single legacy destination when the filter empties the card', () => {
    // A github-only deployment asked for a step-up: there is no per-provider
    // route left to offer, and the legacy route resolves against the first
    // configured provider, so it is the only way forward the reader has.
    const choices = providerChoices(true, [provider('github', 'GitHub', false)], {
      stepUp: true,
      returnTo: '',
    });
    expect(choices).toHaveLength(1);
    expect(choices[0]?.href).toBe('/auth/oidc/start?step_up=1');
  });
});

describe('the return path cannot leave this origin', () => {
  it('accepts a console path and keeps its own query intact', () => {
    expect(safeReturnTo('/ui/device?user_code=ABCD-2345')).toBe('/ui/device?user_code=ABCD-2345');
    expect(
      loginURL({
        loginPath: '/ui/login',
        stepUp: true,
        returnTo: '/ui/device?user_code=ABCD-2345',
      }),
    ).toBe('/ui/login?step_up=1&return_to=%2Fui%2Fdevice%3Fuser_code%3DABCD-2345');
  });

  it('refuses every spelling of somewhere else', () => {
    for (const hostile of [
      '//evil.test/',
      '/\\evil.test/',
      'https://evil.test/',
      'javascript:alert(1)',
      '',
      '   ',
    ]) {
      expect(safeReturnTo(hostile), hostile).toBe('');
    }
  });

  it('drops a return path a control character has been smuggled into', () => {
    expect(safeReturnTo(`/ui/overview${String.fromCharCode(13)}Set-Cookie: x=1`)).toBe('');
  });
});

describe('no markup is built from a string', () => {
  it('never reaches for innerHTML on any of the three session surfaces', () => {
    // eslint bans the escape hatch; this is the second half, which also covers
    // the vendored-terminal loader where a `<script>` element is built by hand.
    const here = dirname(fileURLToPath(import.meta.url));
    for (const path of [
      join(here, 'LoginPage.tsx'),
      join(here, '..', 'device', 'DevicePage.tsx'),
      join(here, '..', 'shell', 'ShellPage.tsx'),
      join(here, '..', 'shell', 'xterm.ts'),
    ]) {
      const source = readFileSync(path, 'utf8');
      expect(source, path).not.toContain('dangerouslySetInnerHTML');
      expect(source, path).not.toContain('innerHTML');
    }
  });
});
