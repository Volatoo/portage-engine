import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { act, useEffect } from 'react';
import type { ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import type { Root } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter, Route, Routes, useNavigate } from 'react-router';
import type { NavigateFunction } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { request } from '../api/client';
import type { BootPayload } from '../boot/payload';
import { MessagesProvider } from '../i18n/context';
import type { Language } from '../i18n/messages';
import { App } from './App';
import { AppChrome, PublicChrome } from './Chrome';
import { UNRESOLVED_IDENTITY, isGranted, useIdentity } from './capabilities';
import type { IdentityContext } from './capabilities';
import { ConsoleErrorBoundary } from './fallbacks';
import { CAPABILITY_SYSTEM_ADMIN, CONSOLE_ROUTES, NAV_ROUTES } from './routes';
import { useSessionGuard } from './session';

/**
 * Rendered as static markup rather than mounted, because that is exactly the
 * frame this stage is about: what the reader sees before any effect has run.
 * Every assertion below is about that first paint.
 */

function paint(node: React.ReactNode, lang: Language, at = '/overview'): string {
  return renderToStaticMarkup(
    <MessagesProvider lang={lang}>
      <MemoryRouter initialEntries={[at]}>{node}</MemoryRouter>
    </MessagesProvider>,
  );
}

/**
 * The other half of this file: the cases that need effects to have run.
 *
 * Everything above asserts a first paint against an identity written by hand,
 * which is the right shape for a question about markup and the wrong one for a
 * question about the gate — a hand-built `IdentityContext` proves the chrome
 * filters a set, and proves nothing about the branch that decides what goes in
 * it. `useIdentity`'s refusal branch survived deletion under all four of those
 * cases. So the identity below is always the one the hook built out of an
 * answer, and the answer is always a stubbed HTTP response.
 */

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const mounted: { root: Root; container: HTMLElement }[] = [];

afterEach(() => {
  // Unmounted after the assertions, never before: unmounting empties the
  // container, and a tree read after that is a green assertion about nothing.
  for (const { root, container } of mounted.splice(0)) {
    act(() => {
      root.unmount();
    });
    container.remove();
  }
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

/**
 * Every destination the tree offers. Read as hrefs rather than off innerHTML,
 * because the rule that bans that property is the console's own invariant and a
 * test file is not exempt from it.
 */
function destinations(container: HTMLElement): string[] {
  return [...container.querySelectorAll('a')].map((anchor) => anchor.getAttribute('href') ?? '');
}

async function mount(node: ReactNode): Promise<HTMLElement> {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  await act(async () => {
    root.render(node);
    await Promise.resolve();
  });
  // A macrotask, because an answer arrives through more than one microtask: the
  // fetch settles, the body is parsed, and only then is there anything to paint.
  await act(async () => {
    await new Promise((settled) => setTimeout(settled, 0));
  });
  mounted.push({ root, container });
  return container;
}

/** Answers every request the same way, and records what was asked for. */
function answering(status: number, body: unknown): string[] {
  const asked: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((path: string) => {
      asked.push(path);
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status,
          headers: { 'Content-Type': 'application/json' },
        }),
      );
    }),
  );
  return asked;
}

/** An answer that never comes, which is the state before IAM has said anything. */
function answeringNever(): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => new Promise<Response>(() => undefined)),
  );
}

const GRANTED = {
  principal: { subject: 'operator@example.test', system_admin: true },
  projects: [{ project_id: 'p1', project_name: 'Default', role: 'owner' }],
};

/** The chrome, built from whatever `useIdentity` made of the answer above. */
function IdentityProbe({ authEnabled }: { authEnabled: boolean }) {
  const identity = useIdentity(authEnabled);
  return (
    <MessagesProvider lang="en">
      <MemoryRouter initialEntries={['/overview']}>
        <AppChrome {...chrome(identity)}>
          <p />
        </AppChrome>
      </MemoryRouter>
    </MessagesProvider>
  );
}

const BOOT: BootPayload = {
  lang: 'en',
  html_lang: 'en',
  auth_enabled: true,
  oidc_enabled: false,
  local_login_enabled: true,
  identity_providers: [],
  principal: null,
  route: { name: 'overview', path: '/ui/overview', job_id: '', instance_id: '', user_code: '' },
  asset_base: '/static/ui/',
};

/** The whole console, at an address, the way a browser arrives at one. */
function mountApp(path: string, boot: BootPayload = BOOT): Promise<HTMLElement> {
  window.history.pushState({}, '', path);
  return mount(<App boot={boot} />);
}

const ADMIN: IdentityContext = {
  granted: new Set([CAPABILITY_SYSTEM_ADMIN]),
  displayName: 'operator@example.test',
  projects: [{ project_id: 'p1', project_name: 'Default', role: 'owner' }],
  resolved: true,
  failure: null,
};

/**
 * The chrome's props, with the identity as the only variable.
 *
 * The switcher is a controlled control — it has to be, because the value it
 * holds is the header every page sends — so every render of the chrome supplies
 * a project and a way to change it.
 */
function chrome(identity: IdentityContext) {
  return {
    identity,
    authEnabled: true,
    projectID: identity.projects[0]?.project_id ?? '',
    onProjectChange: () => undefined,
    onLanguageChange: () => undefined,
  };
}

const IAM_DOWN: IdentityContext = {
  ...UNRESOLVED_IDENTITY,
  resolved: true,
  failure: 'identity store unavailable',
};

describe('capability gating fails closed', () => {
  it('hides a gated destination before IAM has answered', () => {
    const markup = paint(
      <AppChrome {...chrome(UNRESOLVED_IDENTITY)}>
        <p />
      </AppChrome>,
      'en',
    );
    // The three admin destinations are absent from the first paint. Never
    // rendered-then-removed: that is a destination an operator can see, click,
    // and be refused by.
    expect(markup).not.toContain('/ui/monitor');
    expect(markup).not.toContain('/ui/image-factory');
    expect(markup).not.toContain('/ui/settings');
    // The ungated ones are there, so the rail is not simply empty.
    expect(markup).toContain('/overview');
    expect(markup).toContain('/builds');
  });

  it('leaves them hidden when IAM refuses, rather than granting by absence', () => {
    const markup = paint(
      <AppChrome {...chrome(IAM_DOWN)}>
        <p />
      </AppChrome>,
      'en',
    );
    expect(markup).not.toContain('monitor');
    expect(markup).not.toContain('settings');
    // And the reader is told the identity is unavailable rather than left with
    // the loading sentence, which means something different.
    expect(markup).toContain('IAM unavailable');
  });

  it('shows them once IAM has actually granted the capability', () => {
    const markup = paint(
      <AppChrome {...chrome(ADMIN)}>
        <p />
      </AppChrome>,
      'en',
    );
    expect(markup).toContain('/monitor');
    expect(markup).toContain('/image-factory');
    expect(markup).toContain('/settings');
    expect(markup).toContain('operator@example.test');
  });

  it('grants nothing from an empty capability set, whatever the route asks for', () => {
    expect(isGranted(UNRESOLVED_IDENTITY, CAPABILITY_SYSTEM_ADMIN)).toBe(false);
    expect(isGranted(UNRESOLVED_IDENTITY, undefined)).toBe(true);
    expect(isGranted(ADMIN, CAPABILITY_SYSTEM_ADMIN)).toBe(true);
  });
});

describe('language is settled before the first paint', () => {
  it('paints Chinese chrome with no English frame in front of it', () => {
    const markup = paint(
      <AppChrome {...chrome(ADMIN)}>
        <p />
      </AppChrome>,
      'zh',
    );
    expect(markup).toContain('总览');
    expect(markup).toContain('构建任务');
    expect(markup).toContain('退出登录');
    // The English strings those replace are not anywhere in the frame.
    expect(markup).not.toContain('>Overview<');
    expect(markup).not.toContain('>Sign Out<');
  });

  it('offers the other language in the other language', () => {
    // A control labelled in the language the reader cannot read is a control
    // they cannot find.
    expect(
      paint(
        <PublicChrome onLanguageChange={() => undefined}>
          <p />
        </PublicChrome>,
        'en',
      ),
    ).toContain('中文');
    expect(
      paint(
        <PublicChrome onLanguageChange={() => undefined}>
          <p />
        </PublicChrome>,
        'zh',
      ),
    ).toContain('English');
  });
});

describe('the chrome is one source', () => {
  it('carries the language control on the public bar as well as the rail', () => {
    // It goes missing from public layouts even in projects that have one shell,
    // and the cost lands on the reader with no session and no way to change it.
    const markup = paint(
      <PublicChrome onLanguageChange={() => undefined}>
        <p />
      </PublicChrome>,
      'en',
      '/packages',
    );
    expect(markup).toContain('lang-btn');
    expect(markup).toContain('/packages');
    expect(markup).toContain('/docs');
    expect(markup).toContain('/status');
  });

  it('renders the rail chrome twice, because the phone-width bar is the only nav under 700px', () => {
    const markup = paint(
      <AppChrome {...chrome(ADMIN)}>
        <p />
      </AppChrome>,
      'en',
    );
    // `.sidebar` is display:none under 700px and `.topbar` replaces it, so the
    // identity, the language control and sign out exist in both.
    expect(markup.split('lang-btn').length - 1).toBe(2);
    expect(markup.split('data-iam-identity').length - 1).toBe(2);
    expect(markup.split('/logout').length - 1).toBe(2);
    // And the switcher, which is the one of them that is not a convenience:
    // ResolveProject refuses a request with no X-Project-ID from a reader
    // holding more than one project, so under 700px an unbound bar would be a
    // console that cannot load a build list at all.
    expect(markup.split('project-switcher').length - 1).toBe(3);
  });

  it('binds the switcher to the selection, in both copies', () => {
    const twoProjects: IdentityContext = {
      ...ADMIN,
      projects: [
        { project_id: 'p1', project_name: 'Default', role: 'owner' },
        { project_id: 'p2', project_name: 'Platform', role: 'viewer' },
      ],
    };
    const markup = paint(
      <AppChrome {...chrome(twoProjects)} projectID="p2">
        <p />
      </AppChrome>,
      'en',
    );
    // Selected in both, because both are one control over one value.
    expect(markup.split('value="p2" selected').length - 1).toBe(2);
    // The role rides on the label: the same project grants different things to
    // different people, and which of them this reader is decides whether the
    // controls on the page they are about to open will be there at all.
    expect(markup).toContain('Platform · viewer');
    expect(markup).toContain('Default · owner');
  });

  it('disables the switcher when IAM named no project, rather than offering an empty list', () => {
    const markup = paint(
      <AppChrome {...chrome(IAM_DOWN)}>
        <p />
      </AppChrome>,
      'en',
    );
    expect(markup.split('disabled').length - 1).toBe(2);
  });

  it('carries the quota rail in the sidebar, and only there', () => {
    // Every other case about the rail mounts <ProjectQuota /> on its own, so
    // all of them stay green if the chrome stops rendering it — which is the
    // state this whole surface arrived in. The chrome is the only place that
    // fact lives, so it is asserted against the chrome.
    const markup = paint(
      <AppChrome {...chrome(ADMIN)}>
        <p />
      </AppChrome>,
      'en',
    );
    // Once, unlike the identity and the switcher above: twenty numbers about
    // one project do not go in a bar that is one line tall.
    expect(markup.split('policy-summary').length - 1).toBe(1);
    // Inside the sidebar's project context and not loose in the shell, because
    // it reads the same selection the switcher above it writes.
    const context = /class="project-context">([\s\S]*?)<div class="foot"/.exec(markup);
    expect(context?.[1] ?? '').toContain('policy-summary');
  });

  it('omits sign out entirely when the deployment has auth off', () => {
    const markup = paint(
      <AppChrome {...chrome(ADMIN)} authEnabled={false}>
        <p />
      </AppChrome>,
      'en',
    );
    expect(markup).not.toContain('/logout');
  });

  it('still states where the packages are served from, as the old rail did', () => {
    // ui.go's last foot line, and the one fact in the rail that is not about the
    // reader: the path an operator puts in binrepos.conf. shell.css was even
    // written around it — the comment on .sidebar's overflow names "the binhost
    // line" as one of the three things that fell off the bottom of the viewport.
    // The port dropped it, and nothing else in the console says it.
    const markup = paint(
      <AppChrome {...chrome(ADMIN)}>
        <p />
      </AppChrome>,
      'en',
    );
    expect(markup).toContain('binhost:');
    expect(markup).toContain('/binpkgs');
  });
});

describe('the route table is the vocabulary', () => {
  it('covers every path the server will serve the shell for', () => {
    expect(CONSOLE_ROUTES.map((route) => route.path).sort()).toEqual(
      [
        '/',
        '/build/:jobID',
        '/builds',
        '/device',
        '/docs',
        '/image-factory',
        '/login',
        '/logs/:jobID',
        '/monitor',
        '/overview',
        '/packages',
        '/settings',
        '/shell/:instanceID',
        '/status',
      ].sort(),
    );
  });

  it('declares a capability on exactly the three admin destinations', () => {
    expect(
      NAV_ROUTES.filter((route) => route.capability !== undefined).map((route) => route.name),
    ).toEqual(['monitor', 'image-factory', 'settings']);
  });

  it('declares it on the route and not on the link, so a deep link is gated too', () => {
    // The nav entry is a label and nothing else now. A capability that lived
    // there gated the sidebar and gated nothing else: the path still matched,
    // the page still rendered, and it filled with the control plane's refusals.
    for (const route of CONSOLE_ROUTES) {
      if (route.nav !== undefined) {
        expect(Object.keys(route.nav), route.name).toEqual(['labelKey']);
      }
    }
  });

  it('gives every route a chrome, so none can render without one by omission', () => {
    for (const route of CONSOLE_ROUTES) {
      expect(['app', 'public', 'bare'], route.name).toContain(route.chrome);
    }
  });
});

describe('the gate is built from what IAM answered, not from a hand-written identity', () => {
  it('grants the capability the principal in the answer actually carries', async () => {
    answering(200, GRANTED);
    const container = await mount(<IdentityProbe authEnabled={true} />);
    expect(destinations(container)).toContain('/monitor');
    expect(destinations(container)).toContain('/image-factory');
    expect(destinations(container)).toContain('/settings');
    expect(container.textContent).toContain('operator@example.test');
  });

  it('grants nothing through the branch a refusal actually takes', async () => {
    // The case the four static-markup assertions above cannot reach: this is
    // `outcome.kind !== 'ok'` executing, not an object shaped like its result.
    answering(503, { error: 'identity store unavailable' });
    const container = await mount(<IdentityProbe authEnabled={true} />);
    expect(destinations(container)).not.toContain('/monitor');
    expect(destinations(container)).not.toContain('/image-factory');
    expect(destinations(container)).not.toContain('/settings');
    expect(container.textContent).toContain('IAM unavailable');
    // …and the ungated destinations are still there, so this is a gate and not
    // an empty rail.
    expect(destinations(container)).toContain('/overview');
  });

  it('resolves rather than hanging when the answer carries no principal', async () => {
    // A 200 the type says cannot happen. `IAMMe` describes what the control
    // plane documents; a proxy answering 200 with `null` is what can arrive.
    // Read unguarded it threw inside a `then` nobody caught, so the promise
    // rejected, nothing ever set the identity, and the chrome sat on "Loading
    // identity…" against a request that had already come back.
    for (const answer of [null, {}, { principal: null }]) {
      answering(200, answer);
      const container = await mount(<IdentityProbe authEnabled={true} />);
      const text = container.textContent ?? '';
      expect(text, JSON.stringify(answer)).not.toContain('Loading identity');
      expect(text, JSON.stringify(answer)).toContain('IAM unavailable');
    }
  });
});

describe('auth off and IAM refusing are different answers', () => {
  it('shows the admin half with auth off, because the server serves it to anyone', async () => {
    // AUTH_ENABLED=false installs no auth middleware at all (Router in
    // internal/dashboard/dashboard.go) and the control plane behind it answers
    // as anonymous-compatibility with SystemAdmin set (authenticateRequest in
    // internal/server/iam.go). ui.go showed all three destinations there, and a
    // console that hides them is not failing closed — nothing is closed.
    answering(503, { error: 'identity store unavailable' });
    const container = await mount(<IdentityProbe authEnabled={false} />);
    expect(destinations(container)).toContain('/monitor');
    expect(destinations(container)).toContain('/image-factory');
    expect(destinations(container)).toContain('/settings');
  });

  it('keeps them hidden when auth is on and IAM refuses, which is the other case', async () => {
    answering(503, { error: 'identity store unavailable' });
    const container = await mount(<IdentityProbe authEnabled={true} />);
    expect(destinations(container)).not.toContain('/monitor');
    expect(destinations(container)).not.toContain('/image-factory');
    expect(destinations(container)).not.toContain('/settings');
  });
});

/** A page whose render fails, which is the only way to reach a boundary. */
function Exploding(): ReactNode {
  throw new Error('read a property of undefined');
}

describe('nothing the console can be asked for renders an empty document', () => {
  it('says what failed and offers the way out, instead of a white page', async () => {
    // React reports a caught error on the console as well; silenced so the run
    // reads as the pass it is.
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const container = await mount(
      <MessagesProvider lang="en">
        <ConsoleErrorBoundary>
          <Exploding />
        </ConsoleErrorBoundary>
      </MessagesProvider>,
    );
    const text = container.textContent ?? '';
    expect(text).not.toBe('');
    expect(text).toContain('could not render');
    // The thrown message, because an operator forwarding a screenshot is the
    // only channel this failure has.
    expect(text).toContain('read a property of undefined');
    // And something to do about it.
    expect(container.querySelector('button')).not.toBeNull();
  });

  it('answers an address no route matches with a page and a way somewhere', async () => {
    answering(503, { error: 'identity store unavailable' });
    const container = await mountApp('/ui/no-such-page');
    expect(container.textContent).toContain('No such page');
    // Under the public bar, because a reader who mistyped an address may hold no
    // session, and sending them through sign-in to be told the page does not
    // exist is two wrong answers where there was one.
    expect(destinations(container)).toContain('/ui/docs');
    expect(destinations(container)).toContain('/ui/status');
  });

  it('answers a parameter route with no parameter, which the server used to serve', async () => {
    // matchConsoleRoute prefix-matched /ui/build/ and served the shell for it;
    // the router spells that route /build/:jobID and matches nothing without an
    // id. The server 404s it now, and this is the second half of the same fact.
    answering(503, { error: 'identity store unavailable' });
    for (const path of ['/ui/build/', '/ui/logs/', '/ui/shell/']) {
      const container = await mountApp(path);
      expect(container.textContent, path).toContain('No such page');
    }
  });
});

describe('a capability gates the route and not only the link to it', () => {
  it('refuses a deep link to an admin page rather than rendering it', async () => {
    const asked = answering(503, { error: 'identity store unavailable' });
    const container = await mountApp('/ui/settings');
    const text = container.textContent ?? '';
    expect(text).toContain('Not available to you');
    // Named, because an operator told only "not available" opens a ticket and
    // one told which grant is missing asks for that grant.
    expect(text).toContain(CAPABILITY_SYSTEM_ADMIN);
    // The page did not render: /api/iam/me is the only thing that was asked
    // for, where the settings page alone issues six.
    expect(asked).toEqual(['/api/iam/me']);
  });

  it('renders the page for the reader IAM granted it', async () => {
    const asked = answering(200, GRANTED);
    const container = await mountApp('/ui/settings');
    expect(container.textContent).not.toContain('Not available to you');
    expect(asked.length).toBeGreaterThan(1);
  });

  it('says nothing about permission until IAM has answered', async () => {
    // "Not available to you" in front of the administrator it is untrue about,
    // for as long as a round trip takes, is the rendered-then-removed defect in
    // the other direction.
    answeringNever();
    const container = await mountApp('/ui/settings');
    expect(container.textContent).not.toContain('Not available to you');
  });
});

/** Records every full navigation the guard makes, since jsdom performs none. */
function watchNavigations(): string[] {
  const seen: string[] = [];
  vi.stubGlobal('location', {
    get href(): string {
      return seen.at(-1) ?? '';
    },
    set href(value: string) {
      seen.push(value);
    },
  });
  return seen;
}

function Guarded({
  routeIsPublic,
  onReady,
}: {
  routeIsPublic: boolean;
  onReady: (navigate: NavigateFunction) => void;
}) {
  useSessionGuard(routeIsPublic);
  const navigate = useNavigate();
  // Published from an effect and not assigned during render: the console's lint
  // rules say so, and they are right that a value written mid-render is a value
  // the next render may not have.
  useEffect(() => {
    onReady(navigate);
  }, [navigate, onReady]);
  return null;
}

/**
 * Two gated routes and one public one, so a route change here is a real one,
 * and a handle on the client-side navigation that makes it.
 */
async function mountGuard(at = '/overview'): Promise<(to: string) => void> {
  const held: { navigate: NavigateFunction | null } = { navigate: null };
  const onReady = (navigate: NavigateFunction): void => {
    held.navigate = navigate;
  };
  await mount(
    <MemoryRouter initialEntries={[at]}>
      <Routes>
        <Route path="/overview" element={<Guarded routeIsPublic={false} onReady={onReady} />} />
        <Route path="/builds" element={<Guarded routeIsPublic={false} onReady={onReady} />} />
        <Route path="/packages" element={<Guarded routeIsPublic={true} onReady={onReady} />} />
      </Routes>
    </MemoryRouter>,
  );
  return (to: string) => {
    act(() => {
      void held.navigate?.(to);
    });
  };
}

/** One 401, which is what the transport announces an expired session with. */
async function expire(): Promise<void> {
  await act(async () => {
    await request('/api/status');
  });
}

describe('an expired session sends the reader to sign in, once', () => {
  it('navigates on a 401, carrying the route it left so the reader comes back to it', async () => {
    answering(401, null);
    const navigations = watchNavigations();
    await mountGuard();
    await expire();
    expect(navigations).toEqual(['/login?return_to=%2Foverview']);
  });

  it('navigates once for eight 401s, because /monitor polls eight rollups', async () => {
    // Assigning location.href seven more times restarts the navigation and
    // loses the return trip, which is the whole reason the one-shot ref exists.
    answering(401, null);
    const navigations = watchNavigations();
    await mountGuard();
    await act(async () => {
      await Promise.all(Array.from({ length: 8 }, () => request('/api/status')));
    });
    expect(navigations).toHaveLength(1);
  });

  it('does not navigate from a public route, where a 401 is about the request', async () => {
    // The four public pages ask the control plane things an anonymous reader may
    // ask. Sending them to sign in would be a redirect loop through the page
    // that answers it.
    answering(401, null);
    const navigations = watchNavigations();
    await mountGuard('/packages');
    await expire();
    expect(navigations).toEqual([]);
  });

  it('navigates again after a route change, because the ref is about one navigation', async () => {
    // Set once and never cleared, a second expiry anywhere later in the document
    // was silent: the reader kept the error box and the still-running poll this
    // hook exists to replace.
    answering(401, null);
    const navigations = watchNavigations();
    const navigate = await mountGuard();
    await expire();
    expect(navigations).toHaveLength(1);
    navigate('/builds');
    await expire();
    expect(navigations).toEqual(['/login?return_to=%2Foverview', '/login?return_to=%2Fbuilds']);
  });
});

describe('the shell says what it needs before it can say anything', () => {
  it('tells a reader with scripting off, where the old console served HTML', () => {
    // internal/dashboard/ui.go rendered every one of these routes server-side, so
    // that reader read the page. This one is a bundle: without the element below
    // they get #root and nothing else, which reads as an outage.
    const shell = readFileSync(
      join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'index.html'),
      'utf8',
    );
    const noscript = /<noscript>([\s\S]*?)<\/noscript>/.exec(shell);
    expect(noscript).not.toBeNull();
    const said = noscript?.[1] ?? '';
    // The sentences themselves are Go's, because the server is what knows the
    // reader's language before a bundle has run — webassets.NoScriptCopy holds
    // them and console_test.go holds them to the old console's own wording. What
    // is checkable from here is that this element still defers to it: a literal
    // sentence in this file would be an English-only noscript that no Go test
    // would notice, since it would render the same in both languages.
    expect(said).toMatch(/\{\{\.NoScript\.\w+\}\}/);
    const leftToTheTemplate = said
      .replace(/\{\{[^}]*\}\}/g, '') // Go's sentences
      .replace(/<[^>]*>/g, '') // the markup around them
      .replace(/Portage Engine/g, ''); // the product, which is not translated
    expect(
      leftToTheTemplate.match(/[A-Za-z]+/g) ?? [],
      'a word written here renders English in both languages, and no Go test sees it',
    ).toEqual([]);
    // …and names the console that still answers without it, for as long as both
    // are mounted.
    for (const path of ['/overview', '/builds', '/packages', '/docs', '/status']) {
      expect(said).toContain(`href="${path}"`);
    }
  });
});
