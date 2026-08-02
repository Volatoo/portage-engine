import { act } from 'react';
import type { ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { ImageFactoryStatus } from '../../api/types';
import { ProjectScopeContext } from '../../app/project';
import { MessagesProvider } from '../../i18n/context';
import { EN } from '../../i18n/messages.en';
import type { Language } from '../../i18n/messages';
import { Block } from '../monitor/parts';
import { ImageFactoryPage } from './ImageFactoryPage';

/**
 * /image-factory already had the best empty state in the product, and the point
 * of these is that the port did not lose it: the sentence is rendered whole, it
 * is rendered only when the server said so, and it is not confused with either
 * of the two facts it exists to be told apart from.
 *
 * Mounted rather than rendered to a string, because everything this route says
 * arrives in an effect and a static render never runs one.
 */

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const GO_ZERO = '0001-01-01T00:00:00Z';

function response(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

interface Painted {
  /** What a reader would read out. */
  text: string;
  /** Whether a node in some state is on screen — the attribute, not a class. */
  inState(state: string): boolean;
  live(): Element | null;
  /** What each state block declares about holding room, in document order. */
  reserves: (string | null)[];
}

/**
 * Mount, settle, read, unmount.
 *
 * The DOM is queried rather than serialised: `innerHTML` is banned by lint on
 * this project for the reason it is banned everywhere here — the escape hatch
 * back to string markup — and a query says what is being asserted anyway.
 */
async function paint(node: ReactNode, lang: Language = 'en'): Promise<Painted> {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  await act(async () => {
    root.render(
      <MessagesProvider lang={lang}>
        {/* The chrome settles which project every request is made in, and a
            page waits for it: mounted without one, nothing here would ask the
            control plane anything. */}
        <ProjectScopeContext value={{ projectID: 'p1', ready: true }}>{node}</ProjectScopeContext>
      </MessagesProvider>,
    );
    await Promise.resolve();
  });
  const text = container.textContent ?? '';
  const states = [...container.querySelectorAll('[data-state]')].map((node_) =>
    node_.getAttribute('data-state'),
  );
  const live = container.querySelector('.mon-live');
  const reserves = [...container.querySelectorAll('.mon-block')].map((node_) =>
    node_.getAttribute('data-reserve'),
  );
  await act(async () => {
    root.unmount();
    await Promise.resolve();
  });
  container.remove();
  return { text, inState: (state) => states.includes(state), live: () => live, reserves };
}

function factoryStatus(overrides: Partial<ImageFactoryStatus> = {}): ImageFactoryStatus {
  return {
    configured: false,
    catalog: {
      version: 7,
      profiles: [
        {
          id: 'desktop-amd64',
          arch: 'amd64',
          profile_path: 'default/linux/amd64/23.0/desktop',
          binhost_path: 'binhosts/desktop-amd64',
          image_id: 'img-3',
          egress_policy_id: 'restricted',
          channel: 'stable',
          default: true,
          package_sets: ['@world'],
        },
      ],
      images: [],
      mirror_bundles: [],
      egress_policies: [],
    },
    ...overrides,
  };
}

function serving(status: number, body: unknown): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(response(status, body))),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the not-configured sentence is the one that must not change', () => {
  it('is in the catalogue exactly as the console this replaces wrote it', () => {
    expect(EN['factory.notconfigured']).toBe(
      'IMAGE_FACTORY_STATUS_PATH is not configured; catalog data is available, but milestone evidence is not guessed by the UI.',
    );
  });

  it('renders it whole when the catalogue loaded and the evidence path is unset', async () => {
    serving(200, factoryStatus());
    const painted = await paint(<ImageFactoryPage />);
    expect(painted.text).toContain(EN['factory.notconfigured']);
    // And the catalogue the sentence says is still available is beside it.
    expect(painted.text).toContain('desktop-amd64');
  });

  it('does not say it when the evidence path IS configured', async () => {
    serving(
      200,
      factoryStatus({
        configured: true,
        status: {
          schema_version: 1,
          updated_at: '2026-07-30T09:00:00Z',
          overall_state: 'in_progress',
          milestones: [],
          desktop_e2e: { state: 'not_started' },
        },
      }),
    );
    const painted = await paint(<ImageFactoryPage />);
    expect(painted.text).not.toContain(EN['factory.notconfigured']);
  });

  it('does not say it when the request failed, which is a different fact', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('image factory unavailable', { status: 500 }))),
    );
    const painted = await paint(<ImageFactoryPage />);
    expect(painted.text).not.toContain(EN['factory.notconfigured']);
    // The error state says so in its own box, with the server's own sentence.
    expect(painted.inState('error')).toBe(true);
    expect(painted.text).toContain('image factory unavailable');
  });
});

describe('the states this surface distinguishes', () => {
  it('separates an empty catalogue from a failed load', async () => {
    serving(200, {
      configured: false,
      catalog: { version: 0, profiles: [], images: [], mirror_bundles: [], egress_policies: [] },
    });
    const painted = await paint(<ImageFactoryPage />);
    expect(painted.inState('empty')).toBe(true);
    expect(painted.inState('error')).toBe(false);
  });

  it('says a snapshot reported no blockers rather than implying none was asked', async () => {
    serving(200, factoryStatus());
    const painted = await paint(<ImageFactoryPage />);
    expect(painted.text).toContain(EN['factory.noBlockers']);
  });

  it('gives the state text a live region of its own that carries nothing else', async () => {
    serving(200, factoryStatus());
    const painted = await paint(<ImageFactoryPage />);
    const live = painted.live();
    expect(live?.getAttribute('role')).toBe('status');
    expect(live?.getAttribute('aria-live')).toBe('polite');
    // Empty on the happy path: the region exists to carry a sentence, never the
    // page's own contents, so a refresh cannot re-announce the catalogue.
    expect(live?.textContent).toBe('');
  });
});

describe('the state blocks borrowed from /monitor', () => {
  /**
   * Block is /monitor's, and /monitor's contract for it is "a wrapper that holds
   * the height of the payload landing inside it". This route hands it no payload
   * at all — it renders its own table and its own milestone list, and uses the
   * block only to carry the state sentence — so the same node under the same
   * class name is doing a different job, and the floor that makes the swap free
   * on one route is a permanent gap on this one. The console this replaces used
   * a bare <div> at all three places.
   */
  it('hold no height, in every state, because nothing lands in them', async () => {
    serving(200, factoryStatus());
    const painted = await paint(<ImageFactoryPage />);
    // The page's own state, the catalogue's, and the milestone list's.
    expect(painted.reserves).toEqual(['none', 'none', 'none']);
  });

  it('leaves the reserve on for the caller that has a payload to reserve for', async () => {
    const painted = await paint(<Block state={{ state: 'loading' }}>{null}</Block>);
    expect(painted.reserves).toEqual(['payload']);
  });
});

describe('a milestone that never completed does not report a date', () => {
  it('says "never" for a Go zero completed_at, and offers no duration for a step that never ran', async () => {
    serving(
      200,
      factoryStatus({
        configured: true,
        status: {
          schema_version: 1,
          updated_at: GO_ZERO,
          overall_state: 'planned',
          milestones: [
            {
              id: 'M3',
              title: 'Desktop image',
              state: 'planned',
              completed_at: GO_ZERO,
              steps: [
                {
                  id: 'M3.1',
                  title: 'Compose rootfs',
                  state: 'planned',
                  started_at: GO_ZERO,
                  completed_at: GO_ZERO,
                },
              ],
            },
          ],
          desktop_e2e: { state: 'not_started' },
        },
      }),
    );
    const painted = await paint(<ImageFactoryPage />);
    expect(painted.text).toContain(EN['common.never']);
    expect(painted.text).not.toContain('1/1/1');
    expect(painted.text).not.toContain('0001');
    // Neither end is real, so no duration is offered at all, rather than a step
    // that has not run being measured as two thousand years long.
    expect(painted.text).not.toContain(EN['factory.duration']);
  });
});
