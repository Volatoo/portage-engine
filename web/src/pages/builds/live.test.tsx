import { act, useCallback } from 'react';
import type { ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import type { Root } from 'react-dom/client';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { ApiOutcome } from '../../api/client';
import { ProjectScopeContext } from '../../app/project';
import type { ProjectScope } from '../../app/project';
import { CONSOLE_ROUTES } from '../../app/routes';
import type { ConsoleRoute } from '../../app/routes';
import type { BootPayload } from '../../boot/payload';
import { MessagesProvider } from '../../i18n/context';
import { BuildDetailPage } from './BuildDetailPage';
import { usePolledResource } from './data';
import { readLogDocument } from './logdoc';
import type { LogDocument } from './logdoc';
import { LogPane } from './logpane';

/**
 * What these four routes do while they are being watched, which is the only
 * state they are ever in: a poll answering, a second answer overtaking it, a
 * project changing under both, and a log growing a line at a time.
 *
 * Mounted rather than painted to static markup, unlike builds.test.tsx beside
 * it — every fact here is decided by an effect, and a tree that never commits
 * has no scroll position, no second request and no stream.
 */

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const mounted: { root: Root; container: HTMLElement }[] = [];

afterEach(() => {
  // Unmounted after the assertions and not before them: unmounting empties the
  // container, and a tree read after that is a tree with nothing in it.
  for (const { root, container } of mounted.splice(0)) {
    act(() => {
      root.unmount();
    });
    container.remove();
  }
  vi.unstubAllGlobals();
});

interface Deferred<T> {
  promise: Promise<T>;
  settle: (value: T) => void;
}

/** A request the test decides the answer to, and when it arrives. */
function deferred<T>(): Deferred<T> {
  let settle: (value: T) => void = () => undefined;
  const promise = new Promise<T>((resolve) => {
    settle = resolve;
  });
  return { promise, settle };
}

function pending<T>(queue: readonly Deferred<T>[], index: number): Deferred<T> {
  const held = queue[index];
  if (held === undefined) {
    throw new Error(`no request in flight at ${String(index)}`);
  }
  return held;
}

interface View {
  container: HTMLElement;
  /** Re-render in place, which is what a prop changing under a page is. */
  render: (tree: ReactNode) => Promise<void>;
  click: (selector: string) => Promise<void>;
  /** Let every settled promise and the render it causes land. */
  settle: () => Promise<void>;
}

async function open(
  node: ReactNode,
  scope: ProjectScope = { projectID: 'p1', ready: true },
): Promise<View> {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  const render = async (tree: ReactNode): Promise<void> => {
    await act(async () => {
      root.render(
        <MessagesProvider lang="en">
          <ProjectScopeContext value={scope}>
            <MemoryRouter initialEntries={['/build/j1']}>{tree}</MemoryRouter>
          </ProjectScopeContext>
        </MessagesProvider>,
      );
      await Promise.resolve();
    });
  };
  await render(node);
  mounted.push({ root, container });
  const settle = async (): Promise<void> => {
    // A macrotask, because an answer arrives through more than one microtask.
    await act(async () => {
      await new Promise((done) => setTimeout(done, 0));
    });
  };
  await settle();
  return {
    container,
    render,
    settle,
    click: async (selector: string): Promise<void> => {
      const control = container.querySelector(selector);
      if (!(control instanceof HTMLElement)) {
        throw new Error(`no control matching ${selector}`);
      }
      await act(async () => {
        control.click();
        await Promise.resolve();
      });
    },
  };
}

function reading(view: View, selector: string): string {
  return view.container.querySelector(selector)?.textContent ?? '(missing)';
}

/** The polled resource, with everything a page reads off it on the page. */
function Held({ load }: { load: (signal: AbortSignal) => Promise<ApiOutcome<string>> }) {
  const resource = usePolledResource(load, 60000);
  return (
    <>
      <span className="held">{resource.lastOk ?? '(nothing)'}</span>
      <span className="answer">{resource.outcome === null ? '(none)' : resource.outcome.kind}</span>
      <span className="loading">{String(resource.loading)}</span>
      <button
        type="button"
        className="again"
        onClick={() => {
          void resource.refresh();
        }}
      >
        refresh
      </button>
    </>
  );
}

/** The same, behind the `useCallback` every page builds its request with. */
function ScopedHeld({
  projectID,
  answer,
}: {
  projectID: string;
  answer: (projectID: string) => Promise<ApiOutcome<string>>;
}) {
  const load = useCallback(() => answer(projectID), [answer, projectID]);
  return <Held load={load} />;
}

describe('a poll answers for the project it was asked about', () => {
  /** One prepared answer per project, neither arriving until the test says. */
  function switcher(): {
    answer: (projectID: string) => Promise<ApiOutcome<string>>;
    a: Deferred<ApiOutcome<string>>;
    b: Deferred<ApiOutcome<string>>;
  } {
    const a = deferred<ApiOutcome<string>>();
    const b = deferred<ApiOutcome<string>>();
    return {
      a,
      b,
      answer: (projectID: string): Promise<ApiOutcome<string>> => {
        if (projectID === 'p-a') {
          return a.promise;
        }
        if (projectID === 'p-b') {
          return b.promise;
        }
        throw new Error(`no answer prepared for ${projectID}`);
      },
    };
  }

  it('takes the previous project off the screen the moment the reader switches', async () => {
    // The defect: a maintainer of project A opening project B read A's jobs
    // under B's header for as long as B's request was in flight, because the
    // held payload outlived the request it was an answer to.
    const { answer, a } = switcher();
    const view = await open(<ScopedHeld projectID="p-a" answer={answer} />);
    a.settle({ kind: 'ok', value: 'p-a jobs' });
    await view.settle();
    expect(reading(view, '.held')).toBe('p-a jobs');

    await view.render(<ScopedHeld projectID="p-b" answer={answer} />);
    expect(reading(view, '.held')).toBe('(nothing)');
    expect(reading(view, '.answer')).toBe('(none)');
    // Which is what loading is: a request is out and nothing has answered it.
    expect(reading(view, '.loading')).toBe('true');
  });

  it('leaves it off when the project the reader switched to is refused', async () => {
    // And this is the case it stayed on screen forever: a refusal carries no
    // payload, so nothing ever arrived to replace the previous project's.
    const { answer, a, b } = switcher();
    const view = await open(<ScopedHeld projectID="p-a" answer={answer} />);
    a.settle({ kind: 'ok', value: 'p-a jobs' });
    await view.settle();

    await view.render(<ScopedHeld projectID="p-b" answer={answer} />);
    b.settle({
      kind: 'error',
      status: 403,
      message: 'access to project p-b denied',
    });
    await view.settle();
    expect(reading(view, '.held')).toBe('(nothing)');
    expect(reading(view, '.answer')).toBe('error');
  });

  it('shows the newest answer even when an older request lands after it', async () => {
    // Refresh and the poll are two requests in flight at once and they answer
    // in whatever order the network settles them. The reading on screen is the
    // one the reader asked for last, not the one that happened to be slowest.
    const queue: Deferred<ApiOutcome<string>>[] = [];
    const load = (): Promise<ApiOutcome<string>> => {
      const next = deferred<ApiOutcome<string>>();
      queue.push(next);
      return next.promise;
    };
    const view = await open(<Held load={load} />);
    pending(queue, 0).settle({ kind: 'ok', value: 'first' });
    await view.settle();
    expect(reading(view, '.held')).toBe('first');

    await view.click('.again');
    await view.click('.again');
    expect(queue).toHaveLength(3);
    pending(queue, 2).settle({ kind: 'ok', value: 'newest' });
    await view.settle();
    pending(queue, 1).settle({ kind: 'ok', value: 'overtaken' });
    await view.settle();
    expect(reading(view, '.held')).toBe('newest');
  });
});

const LINE_PIXELS = 20;
const PANE_PIXELS = 100;
const scrolled = new WeakMap<Element, number>();

/**
 * A <pre> that scrolls.
 *
 * jsdom lays nothing out, so every box in it is zero pixels tall and every
 * scroll position is zero — which reads as a pane permanently at its own end.
 * These three properties are the only ones the follow behaviour asks about, and
 * the clamp on `scrollTop` is the one a real container applies: nothing can
 * scroll past its last line.
 */
function scrollablePanes(): () => void {
  const pane = HTMLPreElement.prototype;
  Object.defineProperty(pane, 'scrollHeight', {
    configurable: true,
    get(this: HTMLPreElement): number {
      return (this.textContent ?? '').split('\n').length * LINE_PIXELS;
    },
  });
  Object.defineProperty(pane, 'clientHeight', {
    configurable: true,
    get(): number {
      return PANE_PIXELS;
    },
  });
  Object.defineProperty(pane, 'scrollTop', {
    configurable: true,
    get(this: HTMLPreElement): number {
      return scrolled.get(this) ?? 0;
    },
    set(this: HTMLPreElement, value: number) {
      scrolled.set(this, Math.max(0, Math.min(value, this.scrollHeight - PANE_PIXELS)));
    },
  });
  return () => {
    for (const property of ['scrollHeight', 'clientHeight', 'scrollTop']) {
      Reflect.deleteProperty(pane, property);
    }
  };
}

function logOf(lines: number): LogDocument {
  const text = Array.from(
    { length: lines },
    (_unused, index) => `[build] line ${String(index + 1)}`,
  ).join('\n');
  return readLogDocument({ job_id: 'j1', logs: text, bytes: text.length, stages: [] });
}

function livePane(log: LogDocument): ReactNode {
  return (
    <LogPane
      log={log}
      activeFilter="all"
      jobID="j1"
      loadError={null}
      onRetry={() => undefined}
      follow
    />
  );
}

function paneOf(view: View): HTMLPreElement {
  const pane = view.container.querySelector('pre.log-view');
  if (!(pane instanceof HTMLPreElement)) {
    throw new Error('no log pane rendered');
  }
  return pane;
}

/** The reader moving the pane, which is a position and the event it fires. */
function scrollTo(pane: HTMLPreElement, position: number): void {
  pane.scrollTop = position;
  pane.dispatchEvent(new Event('scroll'));
}

describe('a live log follows the build it is a log of', () => {
  it('stays on the newest line while the reader is reading the newest line', async () => {
    // The defect: whether the reader was at the bottom was asked of a pane that
    // had already been given the new lines, so a reader sitting at the end
    // measured as one who had scrolled up by exactly the length of the append,
    // and the log stopped following on the first tick that appended anything.
    const restore = scrollablePanes();
    try {
      const view = await open(livePane(logOf(10)));
      const pane = paneOf(view);
      scrollTo(pane, pane.scrollHeight);
      expect(pane.scrollTop).toBe(10 * LINE_PIXELS - PANE_PIXELS);

      await view.render(livePane(logOf(15)));
      expect(pane.scrollTop).toBe(15 * LINE_PIXELS - PANE_PIXELS);
      await view.render(livePane(logOf(16)));
      expect(pane.scrollTop).toBe(16 * LINE_PIXELS - PANE_PIXELS);
    } finally {
      restore();
    }
  });

  it('opens on the newest line, which is the line a live log is opened for', async () => {
    const restore = scrollablePanes();
    try {
      const view = await open(livePane(logOf(10)));
      expect(paneOf(view).scrollTop).toBe(10 * LINE_PIXELS - PANE_PIXELS);
    } finally {
      restore();
    }
  });

  it('does not move a reader who has scrolled up to an earlier stage', async () => {
    const restore = scrollablePanes();
    try {
      const view = await open(livePane(logOf(10)));
      const pane = paneOf(view);
      scrollTo(pane, 0);

      await view.render(livePane(logOf(15)));
      expect(pane.scrollTop).toBe(0);
      await view.render(livePane(logOf(16)));
      expect(pane.scrollTop).toBe(0);
    } finally {
      restore();
    }
  });
});

const BOOT: BootPayload = {
  lang: 'en',
  html_lang: 'en',
  auth_enabled: true,
  oidc_enabled: false,
  local_login_enabled: true,
  identity_providers: [],
  principal: null,
  step_up_method: 'local',
  route: {
    name: 'build-detail',
    path: '/ui/build/j1',
    job_id: 'j1',
    instance_id: '',
    user_code: '',
  },
  asset_base: '/static/ui/',
};

function routeNamed(name: string): ConsoleRoute {
  const found = CONSOLE_ROUTES.find((route) => route.name === name);
  if (found === undefined) {
    throw new Error(`no route named ${name}`);
  }
  return found;
}

/** Every URL an `EventSource` was opened on, in order. */
function recordEventStreams(): string[] {
  const opened: string[] = [];
  vi.stubGlobal(
    'EventSource',
    class {
      constructor(url: string) {
        opened.push(url);
      }
      addEventListener(): void {
        // The page listens; nothing in this test sends.
      }
      removeEventListener(): void {
        // Nor unlistens to anything.
      }
      close(): void {
        // Closed on unmount, which is the behaviour under test elsewhere.
      }
    },
  );
  return opened;
}

describe('the job event stream is scoped like every other request', () => {
  it('names the project the page is reading, so the fan-out can be resolved', async () => {
    // /api/events/jobs resolves its project from the selector the same way
    // handlers_build.go does, and ResolveProject refuses an unscoped request
    // from any reader holding more than one project — so an unscoped
    // subscription is one that is refused for exactly the readers it is for.
    const opened = recordEventStreams();
    vi.stubGlobal(
      'fetch',
      vi.fn(
        (path: string) =>
          new Promise<Response>((answer) => {
            const body = path.includes('/iam/me')
              ? {
                  principal: { subject: 'operator', display_name: 'operator', system_admin: false },
                  projects: [{ project_id: 'p-a', project_name: 'A', role: 'maintainer' }],
                }
              : path.includes('/logs')
                ? { job_id: 'j1', logs: '', bytes: 0, stages: [] }
                : { job_id: 'j1', status: 'running', project_id: 'p-a' };
            answer(
              new Response(JSON.stringify(body), {
                status: 200,
                headers: { 'Content-Type': 'application/json' },
              }),
            );
          }),
      ),
    );
    await open(
      <BuildDetailPage
        route={routeNamed('build-detail')}
        boot={BOOT}
        onLanguageChange={() => undefined}
      />,
      { projectID: 'p-a', ready: true },
    );
    expect(opened).toHaveLength(1);
    expect(opened[0]).toContain('project_id=p-a');
  });
});
