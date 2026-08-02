import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { act } from 'react';
import type { ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import type { Root } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { MessagesProvider } from '../../i18n/context';
import type { Language } from '../../i18n/messages';
import { DocsPage } from '../docs/DocsPage';
import { LandingPage } from '../landing/LandingPage';
import { PackagesPage } from '../packages/PackagesPage';
import { StatusPage } from '../status/StatusPage';

/**
 * The anonymous surface, pinned by the failures it shipped with rather than by
 * the code paths it takes.
 *
 * Everything here is mounted rather than rendered to a string, because every
 * one of these defects is a second-frame defect: a response labelled with a
 * filter typed after it, a live region rebuilt on a timer, a code block
 * templated from a payload. The first frame was never the problem.
 */

const SOURCE_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..');

const BINHOSTS = {
  binhosts: [
    {
      profile_id: 'pe/amd64/no-multilib/systemd/base-v1',
      arch: 'amd64',
      profile_path: 'default/linux/amd64/23.0/no-multilib/systemd',
      binhost_path: 'releases/amd64/binpackages/23.0/x86-64_pe-systemd-base-v1',
      default: true,
      sync_path: '/binpkgs/releases/amd64/binpackages/23.0/x86-64_pe-systemd-base-v1',
    },
  ],
};

const HELLO = {
  name: 'app-misc/hello',
  version: '2.12.3',
  arch: 'amd64',
  use_flags: ['nls'],
  profile_id: 'pe/amd64/no-multilib/systemd/base-v1',
  profile_path: 'default/linux/amd64/23.0/no-multilib/systemd',
  binhost_path: 'releases/amd64/binpackages/23.0/x86-64_pe-systemd-base-v1',
  download_path:
    '/binpkgs/releases/amd64/binpackages/23.0/x86-64_pe-systemd-base-v1/app-misc/hello/hello-2.12.3-1.gpkg.tar',
};

const ONE_PACKAGE = { packages: [HELLO], total: 1, limit: 50, offset: 0 };
const NO_PACKAGES = { packages: [], total: 0, limit: 50, offset: 0 };

const UPDATED_AT = '2026-08-01T12:34:56Z';
const OPERATIONAL = {
  status: 'operational',
  version: 'dev',
  updated_at: UPDATED_AT,
  components: [
    { name: 'Web and API', status: 'operational' },
    { name: 'Package repository', status: 'operational' },
    { name: 'Build service', status: 'operational' },
  ],
};

/** Answer each public path with a body, or with a refusal. */
function serve(routes: Record<string, unknown>) {
  // The transport always calls fetch with a path string, which is why the stub
  // takes one: a stub that accepted a Request as well would be describing a
  // call this console does not make.
  return vi.fn((url: string) => {
    for (const [path, body] of Object.entries(routes)) {
      if (url.startsWith(path)) {
        if (body instanceof Error) {
          return Promise.resolve(
            new Response(body.message, { status: 503, headers: { 'Content-Type': 'text/plain' } }),
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify(body), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
        );
      }
    }
    return Promise.resolve(new Response('not found', { status: 404 }));
  });
}

interface Mounted {
  container: HTMLElement;
  root: Root;
  text(): string;
  flush(): Promise<void>;
}

async function mount(node: ReactNode, lang: Language = 'en', at = '/'): Promise<Mounted> {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  await act(async () => {
    root.render(
      <MessagesProvider lang={lang}>
        <MemoryRouter initialEntries={[at]}>{node}</MemoryRouter>
      </MessagesProvider>,
    );
    await Promise.resolve();
  });
  return {
    container,
    root,
    text: () => container.textContent ?? '',
    flush: async () => {
      await act(async () => {
        await Promise.resolve();
      });
    },
  };
}

/**
 * Type into a controlled input the way a person does.
 *
 * React installs its own value setter on the element, so assigning `.value`
 * directly leaves React's copy stale and the change event carries the old
 * string. The prototype setter is the one that updates both.
 */
async function typeInto(input: HTMLInputElement, value: string): Promise<void> {
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set?.call(input, value);
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await Promise.resolve();
  });
}

let mounted: Mounted | null = null;

beforeEach(() => {
  // React's own flag, set through Object.assign so the test file does not have
  // to declare a global the application never touches.
  Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });
});

afterEach(() => {
  if (mounted !== null) {
    const root = mounted.root;
    act(() => {
      root.unmount();
    });
    mounted.container.remove();
    mounted = null;
  }
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('the package catalogue says what an operator has to copy', () => {
  it('renders the sync path as text rather than as a title attribute', async () => {
    vi.stubGlobal(
      'fetch',
      serve({ '/api/public/binhosts': BINHOSTS, '/api/public/packages': ONE_PACKAGE }),
    );
    mounted = await mount(<PackagesPage />, 'en', '/packages');
    await mounted.flush();

    const cell = mounted.container.querySelector('.package-sync');
    expect(cell?.textContent).toBe(BINHOSTS.binhosts[0]?.sync_path);
    // The whole defect: a `title` is unreachable by touch, unreachable by
    // keyboard, and unread by most screen readers. Nothing on this page keeps a
    // string there and nowhere else.
    expect(mounted.container.querySelectorAll('[title]')).toHaveLength(0);
  });

  it('takes the sync path from the server rather than rebuilding it from a prefix', async () => {
    // The binhost inventory is what publishes `sync_path`. When it cannot be
    // read the page falls back to the path the package itself carries — it does
    // not glue "/binpkgs/" onto it and call the result a sync path.
    vi.stubGlobal(
      'fetch',
      serve({
        '/api/public/binhosts': new Error('inventory unavailable'),
        '/api/public/packages': ONE_PACKAGE,
      }),
    );
    mounted = await mount(<PackagesPage />, 'en', '/packages');
    await mounted.flush();

    expect(mounted.container.querySelector('.package-sync')?.textContent).toBe(HELLO.binhost_path);
    expect(mounted.text()).toContain('inventory unavailable');
  });

  it('counts one package as one package', async () => {
    vi.stubGlobal(
      'fetch',
      serve({ '/api/public/binhosts': BINHOSTS, '/api/public/packages': ONE_PACKAGE }),
    );
    mounted = await mount(<PackagesPage />, 'en', '/packages');
    await mounted.flush();

    const summary = mounted.container.querySelector('.package-summary')?.textContent ?? '';
    expect(summary).toBe('1 published package');
    expect(summary).not.toContain('packages');
  });

  it('says the catalogue reports no signature rather than inferring one', async () => {
    vi.stubGlobal(
      'fetch',
      serve({ '/api/public/binhosts': BINHOSTS, '/api/public/packages': ONE_PACKAGE }),
    );
    mounted = await mount(<PackagesPage />, 'en', '/packages');
    await mounted.flush();

    const badge = mounted.container.querySelectorAll('tbody .status')[0];
    expect(badge?.className).toContain('gray');
    expect(badge?.textContent).toBe('not reported');
    // Never the word for a state nobody measured.
    expect(badge?.textContent).not.toContain('signed');
  });

  it('resolves a reported signature through the one status vocabulary', async () => {
    vi.stubGlobal(
      'fetch',
      serve({
        '/api/public/binhosts': BINHOSTS,
        '/api/public/packages': {
          ...ONE_PACKAGE,
          packages: [{ ...HELLO, signed: true, signing_key_id: 'ABCD1234' }],
        },
      }),
    );
    mounted = await mount(<PackagesPage />, 'zh', '/packages');
    await mounted.flush();

    const badge = mounted.container.querySelectorAll('tbody .status')[0];
    expect(badge?.className).toContain('green');
    expect(badge?.textContent).toBe('已签名');
  });
});

describe('empty and filtered-empty are two different facts', () => {
  it('invites publishing when nothing has ever been published', async () => {
    vi.stubGlobal(
      'fetch',
      serve({ '/api/public/binhosts': BINHOSTS, '/api/public/packages': NO_PACKAGES }),
    );
    mounted = await mount(<PackagesPage />, 'en', '/packages');
    await mounted.flush();

    const box = mounted.container.querySelector('.empty');
    expect(box?.getAttribute('data-state')).toBe('empty');
    expect(box?.textContent).toContain('No packages have been published');
  });

  it('offers a way out of the filter when a search matched nothing', async () => {
    vi.stubGlobal(
      'fetch',
      serve({ '/api/public/binhosts': BINHOSTS, '/api/public/packages': NO_PACKAGES }),
    );
    mounted = await mount(<PackagesPage />, 'en', '/packages?q=zzz');
    await mounted.flush();

    const box = mounted.container.querySelector('.empty');
    expect(box?.getAttribute('data-state')).toBe('filtered-empty');
    expect(box?.textContent).toContain('Clear the search');
  });

  it('labels a response with the filter that produced it, not with the box contents', async () => {
    // The defect: the empty branch read the inputs at render time, so
    // clearing the search box — without submitting it — relabelled a response
    // that had been produced by a search as "nothing has ever been published".
    vi.stubGlobal(
      'fetch',
      serve({ '/api/public/binhosts': BINHOSTS, '/api/public/packages': NO_PACKAGES }),
    );
    mounted = await mount(<PackagesPage />, 'en', '/packages?q=zzz');
    await mounted.flush();
    expect(mounted.container.querySelector('.empty')?.getAttribute('data-state')).toBe(
      'filtered-empty',
    );

    const input = mounted.container.querySelector<HTMLInputElement>('#package-query');
    expect(input?.value).toBe('zzz');
    await typeInto(input as HTMLInputElement, '');
    await mounted.flush();

    // Still the answer to a search, because that is the request that produced
    // it. It becomes the other sentence when an unfiltered request answers.
    expect(mounted.container.querySelector('.empty')?.getAttribute('data-state')).toBe(
      'filtered-empty',
    );
  });
});

describe('the status page does not re-announce itself every thirty seconds', () => {
  it('keeps the live region to state text, with the rows outside it', async () => {
    vi.stubGlobal('fetch', serve({ '/api/public/status': OPERATIONAL }));
    mounted = await mount(<StatusPage />, 'en', '/status');
    await mounted.flush();

    const headline = mounted.container.querySelector('.status-headline');
    expect(headline?.getAttribute('role')).toBe('status');
    expect(headline?.textContent).toBe('All systems operational');
    // Not the component list, and not the timestamp: those are the two things
    // that made the old region speak on every poll.
    expect(headline?.textContent).not.toContain('Web and API');
    expect(headline?.textContent).not.toContain('2026');

    // And nothing above a component row is a live region.
    for (const row of mounted.container.querySelectorAll('.status-row')) {
      for (let node = row.parentElement; node !== null; node = node.parentElement) {
        expect(node.getAttribute('role')).not.toBe('status');
        expect(node.getAttribute('aria-live')).toBeNull();
      }
    }
  });

  it('mutates nothing inside the live region when a poll finds the same state', async () => {
    vi.useFakeTimers();
    vi.stubGlobal('fetch', serve({ '/api/public/status': OPERATIONAL }));
    mounted = await mount(<StatusPage />, 'en', '/status');
    await act(async () => {
      await Promise.resolve();
    });

    const headline = mounted.container.querySelector('.status-headline');
    expect(headline).not.toBeNull();
    let mutations = 0;
    const observer = new MutationObserver((records) => {
      mutations += records.length;
    });
    observer.observe(headline as Node, { childList: true, characterData: true, subtree: true });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(31_000);
    });
    observer.disconnect();

    // A poll that finds the same state changes no text, so a screen reader is
    // told nothing. The old page rebuilt the component list under the region on
    // every tick and read all of it aloud, indefinitely, on the happy path.
    expect(mutations).toBe(0);
  });

  it('reports a state it has never heard of as unknown, not as a fault', async () => {
    vi.stubGlobal(
      'fetch',
      serve({
        '/api/public/status': {
          ...OPERATIONAL,
          status: 'maintenance',
          components: [{ name: 'Object storage', status: 'maintenance' }],
        },
      }),
    );
    mounted = await mount(<StatusPage />, 'en', '/status');
    await mounted.flush();

    const badge = mounted.container.querySelector('.status-row .status');
    expect(badge?.className).toContain('gray');
    expect(badge?.textContent).toBe('maintenance');
    // The catch-all this replaces was red plus the word "Degraded".
    expect(mounted.text()).not.toContain('Degraded');
    expect(mounted.text()).not.toContain('degraded');
    // A component with no entry in the label table renders under its own name.
    expect(mounted.text()).toContain('Object storage');
  });

  it('spells a known component and its state in the reader language', async () => {
    vi.stubGlobal('fetch', serve({ '/api/public/status': OPERATIONAL }));
    mounted = await mount(<StatusPage />, 'zh', '/status');
    await mounted.flush();

    expect(mounted.text()).toContain('软件包仓库');
    expect(mounted.container.querySelector('.status-row .status')?.textContent).toBe('正常');
    expect(mounted.container.querySelector('.status-headline')?.textContent).toBe('服务运行正常');
  });

  it('names the build honestly and the timezone explicitly', async () => {
    vi.stubGlobal('fetch', serve({ '/api/public/status': OPERATIONAL }));
    mounted = await mount(<StatusPage />, 'en', '/status');
    await mounted.flush();

    const meta = mounted.container.querySelector('.status-meta')?.textContent ?? '';
    // "Version dev" names a release nobody can pin an incident to.
    expect(meta).toContain('Development build');
    expect(meta).not.toContain('Version dev');

    const options: Intl.DateTimeFormatOptions = {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
    };
    const zoned = new Intl.DateTimeFormat('en', { ...options, timeZoneName: 'short' }).format(
      new Date(UPDATED_AT),
    );
    expect(zoned).not.toBe(new Intl.DateTimeFormat('en', options).format(new Date(UPDATED_AT)));
    expect(meta).toContain(zoned);
  });

  it('formats that instant in the app locale and not the operating system one', async () => {
    vi.stubGlobal('fetch', serve({ '/api/public/status': OPERATIONAL }));
    mounted = await mount(<StatusPage />, 'zh', '/status');
    await mounted.flush();

    const options: Intl.DateTimeFormatOptions = {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
      timeZoneName: 'short',
    };
    const when = new Date(UPDATED_AT);
    expect(mounted.container.querySelector('.status-meta')?.textContent).toContain(
      new Intl.DateTimeFormat('zh-CN', options).format(when),
    );
    expect(mounted.container.querySelector('.status-meta')?.textContent).not.toContain(
      new Intl.DateTimeFormat('en', options).format(when),
    );
  });
});

describe('the documentation describes this deployment', () => {
  it('templates host, profile id and sync path from what the console already has', async () => {
    vi.stubGlobal('fetch', serve({ '/api/public/binhosts': BINHOSTS }));
    mounted = await mount(<DocsPage />, 'en', '/docs');
    await mounted.flush();

    const blocks = [...mounted.container.querySelectorAll('.docs-command')].map(
      (node) => node.textContent ?? '',
    );
    expect(blocks).toHaveLength(3);
    const all = blocks.join('\n');
    expect(all).toContain(`-server=${window.location.origin}`);
    expect(all).toContain('-profile-id=pe/amd64/no-multilib/systemd/base-v1');
    expect(all).toContain(`sync-uri = ${window.location.origin}${BINHOSTS.binhosts[0]?.sync_path}`);

    // The three literals that described somebody else's install.
    expect(all).not.toContain('https://SERVER');
    expect(all).not.toContain('pe/amd64/glibc/systemd/base-v1');
    expect(all).not.toContain('TARGET');
  });

  it('holds the blocks open while the inventory is unread, so nothing under them moves', async () => {
    vi.stubGlobal('fetch', serve({ '/api/public/binhosts': new Error('binhosts unavailable') }));
    mounted = await mount(<DocsPage />, 'en', '/docs');
    await mounted.flush();

    const blocks = mounted.container.querySelectorAll('.docs-command');
    expect(blocks).toHaveLength(3);
    // Empty rather than filled with a guess, and the reader is told why.
    expect([...blocks].every((node) => (node.textContent ?? '') === '')).toBe(true);
    expect(mounted.text()).toContain('binhosts unavailable');
  });
});

describe('one public navigation', () => {
  it('leaves the landing page carrying none of its own', () => {
    const markup = renderToStaticMarkup(
      <MessagesProvider lang="en">
        <MemoryRouter initialEntries={['/']}>
          <LandingPage />
        </MemoryRouter>
      </MessagesProvider>,
    );
    // The bar, the language control and the footer come from PublicChrome now.
    expect(markup).not.toContain('landing-nav');
    expect(markup).not.toContain('public-link');
    expect(markup).not.toContain('landing-footer');
    expect(markup).not.toContain('lang-btn');
    // What it does carry is its own content.
    expect(markup).toContain('Build once, install everywhere');
  });
});

describe('the landing layout has no floor its grid tracks cannot go under', () => {
  /**
   * The sweep this pins: 163 of 961 widths scrolled the document sideways, in
   * two bands — [320, 353] and [801, 929], the second being where the
   * three-column workflow is still on. Both had one cause: a grid item defaults
   * to a min-content floor, and a nowrap command line is one unbreakable run,
   * so the track could not shrink below the longest command. Neither half of
   * the fix works alone, which is why both are asserted.
   */
  const styles = (name: string): string =>
    readFileSync(join(SOURCE_ROOT, 'styles', name), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');

  it('lets a workflow step be narrower than the command inside it', () => {
    const surfaces = styles('surfaces.css');
    expect(surfaces).toMatch(/\.landing-flow \.step \{[^}]*min-width:\s*0/);
  });

  it('lets that command wrap, which is the other half', () => {
    const rule = /\.landing-flow \.step \.mono \{([^}]*)\}/.exec(styles('surfaces.css'))?.[1] ?? '';
    expect(rule).not.toBe('');
    expect(rule).not.toContain('nowrap');
    expect(rule).toContain('overflow-wrap: anywhere');
    expect(rule).toContain('white-space: pre-wrap');
  });

  it('gives the other three-column track the same floor', () => {
    expect(styles('pages/landing.css')).toMatch(/\.landing-card \{[^}]*min-width:\s*0/);
  });

  it('lets the hero actions fold instead of overflowing at 320px', () => {
    expect(styles('pages/landing.css')).toMatch(/\.landing-hero \.cta \{[^}]*flex-wrap:\s*wrap/);
  });
});
