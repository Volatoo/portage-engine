import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join, relative, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

import { act, useState } from 'react';
import type { ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import type { Root } from 'react-dom/client';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { onSessionExpired, request } from '../api/client';
import type { ProjectPolicy } from '../api/types';
import type { BootPayload } from '../boot/payload';
import { MessagesProvider } from '../i18n/context';
import { ConfirmDialog } from '../pages/builds/parts';
import { ProjectQuota } from './ProjectQuota';
import { PROJECT_STORAGE_KEY, ProjectScopeContext } from './project';
import type { ProjectScope } from './project';
import { CONSOLE_ROUTES } from './routes';
import type { ConsoleRoute } from './routes';
import { useStepUp, withStepUp } from './stepup';

/**
 * The shared runtime, which is the layer every page consumes and therefore the
 * one whose losses no page's own test could see.
 *
 * Each case below is the failure it actually produced: a build list that could
 * not load for anyone holding two projects, a write that answered a click with a
 * sentence, a poll still running against a session the server had forgotten, a
 * rail that no longer said the project was suspended, and a credential prompt
 * whose answer saved a form nobody had asked to save.
 */

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

/** Everything mounted by a case, taken down after it. */
const mounted: { root: Root; container: HTMLElement }[] = [];

afterEach(() => {
  // Unmounted after the assertions and not before them: unmounting empties the
  // container, and a tree read after that is a tree with nothing in it — which
  // is a green assertion about a surface that never rendered.
  for (const { root, container } of mounted.splice(0)) {
    act(() => {
      root.unmount();
    });
    container.remove();
  }
  vi.unstubAllGlobals();
});

interface Call {
  path: string;
  method: string;
  projectID: string | null;
}

/**
 * Answers every request out of `reply` and records what the header carried.
 *
 * A reply is either a payload, which is wrapped in a 200, or a whole `Response`
 * for the cases that need a refusal — a 428 that raises the credential prompt,
 * or a revocation the server says no to.
 */
function captureRoutes(reply: (path: string) => unknown): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((path: string, init?: RequestInit) => {
      calls.push({
        path,
        method: init?.method ?? 'GET',
        projectID: new Headers(init?.headers).get('X-Project-ID'),
      });
      const answer = reply(path);
      return Promise.resolve(
        answer instanceof Response
          ? answer
          : new Response(JSON.stringify(answer), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            }),
      );
    }),
  );
  return calls;
}

/** One payload for everything, for a case asserting the request and not the screen. */
function captureFetch(body: unknown): Call[] {
  return captureRoutes(() => body);
}

interface Mounted {
  container: HTMLElement;
  text: string;
  found: (selector: string) => Element[];
}

/** A macrotask, which is what a payload takes: fetch settles, body parses, paint. */
async function settle(): Promise<void> {
  await act(async () => {
    await new Promise((done) => setTimeout(done, 0));
  });
}

async function mount(node: ReactNode, scope: ProjectScope): Promise<Mounted> {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  await act(async () => {
    root.render(
      <MessagesProvider lang="en">
        <ProjectScopeContext value={scope}>
          <MemoryRouter>{node}</MemoryRouter>
        </ProjectScopeContext>
      </MessagesProvider>,
    );
    await Promise.resolve();
  });
  await settle();
  mounted.push({ root, container });
  return {
    container,
    get text() {
      return container.textContent ?? '';
    },
    found: (selector: string): Element[] => [...container.querySelectorAll(selector)],
  };
}

/** The control carrying exactly this label, anywhere under `scope`. */
function control(scope: ParentNode, label: string): HTMLElement {
  const found = [...scope.querySelectorAll('button')].filter(
    (node) => (node.textContent ?? '').trim() === label,
  );
  expect(found.length, `no single control labelled "${label}"`).toBe(1);
  return found[0] as HTMLElement;
}

/** Activates a control the way a reader does: focus first, then the click. */
async function press(node: HTMLElement): Promise<void> {
  node.focus();
  await act(async () => {
    node.click();
    await Promise.resolve();
  });
  await settle();
}

function routeNamed(name: string): ConsoleRoute {
  const found = CONSOLE_ROUTES.find((route) => route.name === name);
  if (found === undefined) {
    throw new Error(`no route named ${name}`);
  }
  return found;
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

/** A route's own page, rendered the way the router renders it. */
function pageFor(name: string, route: Partial<BootPayload['route']> = {}): ReactNode {
  const declared = routeNamed(name);
  const Page = declared.component;
  if (Page === undefined) {
    throw new Error(`${name} has no component`);
  }
  return (
    <Page
      route={declared}
      boot={{ ...BOOT, route: { ...BOOT.route, name, ...route } }}
      onLanguageChange={() => undefined}
    />
  );
}

describe('a request is made in the project the chrome selected', () => {
  it('asks nothing at all until the selection is settled', async () => {
    // What `await iamReady` did in the console this replaces. A request issued
    // in front of the IAM answer is the unscoped one the control plane refuses,
    // and the surface it lands on shows the refusal rather than the payload.
    const calls = captureFetch([]);
    await mount(pageFor('overview'), { projectID: '', ready: false });
    expect(calls).toEqual([]);
  });

  it('remembers the choice under the key the console this replaces wrote', () => {
    // internal/dashboard/ui.go read `pe_project_id` out of localStorage on
    // every request and wrote it from the switcher. Spelling it differently
    // here would silently reset the project of every reader who already had
    // one, on a control whose whole purpose is to be sticky.
    expect(PROJECT_STORAGE_KEY).toBe('pe_project_id');
  });
});

/* ---- the header, on every surface that has one to send ------------------- */

/**
 * `internal/persistence/iam.go` ResolveProject refuses an empty selector for any
 * non-system-admin holding more than one project, and every route below resolves
 * through it. Without the header those surfaces do not load at all for exactly
 * the readers the project switcher was put there for.
 *
 * This used to be one case against /overview. Seven other surfaces read the same
 * scope and none of them was covered: dropping the header from /monitor, from
 * /image-factory, from the settings resource or from any of the nine writes
 * passed. So the table is the whole set, it is checked against the source below
 * so it cannot fall behind, and the writes are activated rather than assumed.
 */
const SCOPED_PROJECT = 'proj-b';

const JOB = {
  job_id: 'job-1',
  project_id: SCOPED_PROJECT,
  status: 'failed',
  package_name: 'app-misc/jq',
  version: '1.7',
  arch: 'amd64',
  created_at: '2026-08-01T10:00:00Z',
  updated_at: '2026-08-01T10:05:00Z',
  error: 'provisioning refused',
  artifacts: [],
};

const SESSION = {
  id: 'sess-1',
  issued_at: '2026-08-01T09:00:00Z',
  last_seen_at: '2026-08-01T10:00:00Z',
  expires_at: '2026-08-02T09:00:00Z',
  acr: 'pwd',
  amr: ['pwd'],
  revoked_at: '',
};

/** A control plane that answers everything, so a page reaches its controls. */
function controlPlane(path: string): unknown {
  if (path.startsWith('/api/iam/me')) {
    return {
      principal: { subject: 'op', display_name: 'Operator', system_admin: true },
      projects: [{ project_id: SCOPED_PROJECT, project_name: 'B', role: 'owner' }],
      identity_providers: [],
    };
  }
  if (path.startsWith('/api/iam/sessions/revoke-all')) {
    // Refused on purpose. A revoke-all that succeeds sends the reader to
    // /logout, and the request — which is the whole assertion — has already
    // been made and recorded by then.
    return new Response('revocation refused', { status: 503 });
  }
  if (path.startsWith('/api/iam/sessions')) {
    return { sessions: [SESSION], current_session_id: 'sess-0' };
  }
  if (path.startsWith('/api/builds/logs')) {
    return { job_id: 'job-1', lines: [], text: '' };
  }
  if (path.startsWith('/api/builds/detail')) {
    return JOB;
  }
  if (path.startsWith('/api/builds/submit')) {
    return { job_id: 'job-2' };
  }
  if (path.startsWith('/api/builds/cleanup-failed')) {
    return { deleted: 1 };
  }
  if (path.startsWith('/api/builds?')) {
    return [JOB];
  }
  if (path.startsWith('/api/settings/cloud')) {
    // The two booleans are spelled out because the form binds them to checked
    // inputs: absent, they arrive as undefined and React reports the control
    // going from controlled to uncontrolled, which is noise about the stub.
    return {
      provider: 'pve',
      pve_node: 'auto',
      pve_insecure: false,
      ssh_insecure_host_key: false,
      remote_builders: [],
      ok: true,
      nodes: [],
    };
  }
  if (path.startsWith('/api/projects/policy')) {
    return policy();
  }
  if (path.startsWith('/api/instances')) {
    return [];
  }
  return {};
}

/**
 * Every module that reads the project scope, and the surface that mounts it.
 *
 * The keys are checked against the source tree, so a ninth reader added
 * tomorrow fails this file rather than quietly shipping unscoped.
 */
const SCOPE_READERS: Record<string, string> = {
  'app/ProjectQuota.tsx': 'the project rail',
  'pages/builds/OverviewPage.tsx': '/overview',
  'pages/builds/BuildsPage.tsx': '/builds',
  'pages/builds/BuildDetailPage.tsx': '/build/:jobID',
  'pages/builds/LogsPage.tsx': '/logs/:jobID',
  'pages/monitor/MonitorPage.tsx': '/monitor',
  'pages/image-factory/ImageFactoryPage.tsx': '/image-factory',
  'pages/settings/SettingsPage.tsx': '/settings',
  'pages/settings/GpgPanel.tsx': '/settings',
  'pages/settings/SecurityPanel.tsx': '/settings',
  'pages/settings/TestBuildCard.tsx': '/settings',
  'pages/settings/TestConnectionCard.tsx': '/settings',
};

/** The surfaces the table above names, each mounted once. */
const SCOPED_SURFACES: { name: string; node: () => ReactNode }[] = [
  { name: 'the project rail', node: () => <ProjectQuota /> },
  { name: '/overview', node: () => pageFor('overview') },
  { name: '/builds', node: () => pageFor('builds') },
  { name: '/build/:jobID', node: () => pageFor('build-detail', { job_id: 'job-1' }) },
  { name: '/logs/:jobID', node: () => pageFor('logs', { job_id: 'job-1' }) },
  { name: '/monitor', node: () => pageFor('monitor') },
  { name: '/image-factory', node: () => pageFor('image-factory') },
  { name: '/settings', node: () => pageFor('settings') },
];

/**
 * The three paths that are deliberately made outside any project.
 *
 * `/api/iam/me` is the chrome asking who the reader is, which is the question
 * whose answer NAMES the projects; `/api/keys/public` and `/auth/step-up` are
 * not project resources at all. Everything else the console asks for is asked
 * for in a project, and this list is short so that adding to it is a decision.
 */
const UNSCOPED_PATHS = ['/api/iam/me', '/api/keys/public', '/auth/step-up'];

function scopeFailures(calls: readonly Call[]): string[] {
  return calls
    .filter((call) => !UNSCOPED_PATHS.some((path) => call.path.startsWith(path)))
    .filter((call) => call.projectID !== SCOPED_PROJECT)
    .map((call) => `${call.method} ${call.path}`);
}

describe('every surface that reads the project scope sends it', () => {
  it('names the same modules the source does, so the table cannot fall behind', () => {
    const source = dirname(dirname(fileURLToPath(import.meta.url)));
    const walk = (directory: string): string[] =>
      readdirSync(directory, { withFileTypes: true }).flatMap((entry) =>
        entry.isDirectory()
          ? walk(join(directory, entry.name))
          : [join(directory, entry.name)].filter((name) => /\.tsx?$/.test(name)),
      );
    const readers = walk(source)
      .filter((path) => !/\.test\.tsx?$/.test(path))
      // project.ts declares the hook; reading it there is not consuming it.
      .filter((path) => !path.endsWith(`app${sep}project.ts`))
      .filter((path) => readFileSync(path, 'utf8').includes('useProjectScope('))
      .map((path) => relative(source, path).split(sep).join('/'))
      .sort();
    expect(readers).toEqual(Object.keys(SCOPE_READERS).sort());
    // And each of them is reached by a surface this file actually mounts.
    const mountable = new Set(SCOPED_SURFACES.map((surface) => surface.name));
    expect(Object.values(SCOPE_READERS).filter((surface) => !mountable.has(surface))).toEqual([]);
  });

  for (const surface of SCOPED_SURFACES) {
    it(`${surface.name} puts X-Project-ID on every request it makes`, async () => {
      const calls = captureRoutes(controlPlane);
      await mount(surface.node(), { projectID: SCOPED_PROJECT, ready: true });
      expect(calls.length, 'this surface asked for nothing at all').toBeGreaterThan(0);
      expect(scopeFailures(calls)).toEqual([]);
    });
  }
});

describe('every project-scoped write sends it too', () => {
  it('cleans up failed jobs in the project the rail names', async () => {
    const calls = captureRoutes(controlPlane);
    const page = await mount(pageFor('builds'), { projectID: SCOPED_PROJECT, ready: true });
    await press(control(page.container, 'Clean Up Failed'));
    const dialog = page.container.querySelector('dialog');
    expect(dialog, 'the bulk delete no longer asks').not.toBeNull();
    await press(control(dialog as ParentNode, 'Clean Up Failed'));
    expect(calls.filter((call) => call.path.startsWith('/api/builds/cleanup-failed'))).toHaveLength(
      1,
    );
    expect(scopeFailures(calls)).toEqual([]);
  });

  it('retries and deletes a job in the project the rail names', async () => {
    const calls = captureRoutes(controlPlane);
    const page = await mount(pageFor('build-detail', { job_id: 'job-1' }), {
      projectID: SCOPED_PROJECT,
      ready: true,
    });
    for (const action of ['Retry Job', 'Delete Job']) {
      await press(control(page.container, action));
      await press(control(page.container.querySelector('dialog') as ParentNode, action));
    }
    expect(calls.filter((call) => call.path.startsWith('/api/builds/retry'))).toHaveLength(1);
    expect(calls.filter((call) => call.path.startsWith('/api/builds/delete'))).toHaveLength(1);
    expect(scopeFailures(calls)).toEqual([]);
  });

  it('cancels a running job in the project the rail names', async () => {
    const calls = captureRoutes((path) =>
      path.startsWith('/api/builds/detail') ? { ...JOB, status: 'running' } : controlPlane(path),
    );
    const page = await mount(pageFor('build-detail', { job_id: 'job-1' }), {
      projectID: SCOPED_PROJECT,
      ready: true,
    });
    await press(control(page.container, 'Cancel Job'));
    await press(control(page.container.querySelector('dialog') as ParentNode, 'Cancel Job'));
    expect(calls.filter((call) => call.path.startsWith('/api/builds/cancel'))).toHaveLength(1);
    expect(scopeFailures(calls)).toEqual([]);
  });

  it('saves, tests and submits from /settings in the project the rail names', async () => {
    // Four writers on one page, three resources: the settings PUT, the
    // connection test, and the build the test-build control submits after the
    // PUT it makes first.
    const calls = captureRoutes(controlPlane);
    const page = await mount(pageFor('settings'), { projectID: SCOPED_PROJECT, ready: true });
    await press(control(page.container, 'Save'));
    await press(control(page.container, 'Test Connection'));
    await press(control(page.container, 'Start Test Build'));
    const wrote = (path: string, method: string): Call[] =>
      calls.filter((call) => call.path.startsWith(path) && call.method === method);
    expect(wrote('/api/settings/cloud/test', 'POST')).toHaveLength(1);
    expect(wrote('/api/settings/cloud', 'PUT').length).toBeGreaterThan(0);
    expect(wrote('/api/builds/submit', 'POST')).toHaveLength(1);
    expect(scopeFailures(calls)).toEqual([]);
  });

  it('revokes sessions from /settings in the project the rail names', async () => {
    vi.stubGlobal('confirm', () => true);
    const calls = captureRoutes(controlPlane);
    const page = await mount(pageFor('settings'), { projectID: SCOPED_PROJECT, ready: true });
    await press(control(page.container, 'Revoke'));
    await press(control(page.container, 'Revoke all sessions'));
    expect(
      calls.filter(
        (call) => call.path.startsWith('/api/iam/sessions?') && call.method === 'DELETE',
      ),
    ).toHaveLength(1);
    expect(
      calls.filter((call) => call.path.startsWith('/api/iam/sessions/revoke-all')),
    ).toHaveLength(1);
    expect(scopeFailures(calls)).toEqual([]);
  });
});

/* ---- the credential prompt ----------------------------------------------- */

describe('a step-up refusal is satisfied where the write was made', () => {
  const refusal = { kind: 'step-up' as const, message: 'fresh authentication required' };
  const ok = { kind: 'ok' as const, value: 'written' };

  it('asks for a local credential and repeats the write once', async () => {
    const answers = [{ ...refusal, method: 'local' as const }, ok];
    const run = vi.fn(() => Promise.resolve(answers.shift() ?? ok));
    const reauthenticate = vi.fn();
    const outcome = await withStepUp(run, {
      elevate: () => Promise.resolve(true),
      reauthenticate,
    });
    expect(run).toHaveBeenCalledTimes(2);
    expect(outcome).toEqual(ok);
    expect(reauthenticate).not.toHaveBeenCalled();
  });

  it('repeats it once and only once, so a second refusal is not a loop', async () => {
    const run = vi.fn(() => Promise.resolve({ ...refusal, method: 'local' as const }));
    const outcome = await withStepUp(run, {
      elevate: () => Promise.resolve(true),
      reauthenticate: () => undefined,
    });
    expect(run).toHaveBeenCalledTimes(2);
    expect(outcome.kind).toBe('step-up');
  });

  it('reports the refusal and writes nothing when the reader declines', async () => {
    const run = vi.fn(() => Promise.resolve({ ...refusal, method: 'local' as const }));
    const outcome = await withStepUp(run, {
      elevate: () => Promise.resolve(false),
      reauthenticate: () => undefined,
    });
    expect(run).toHaveBeenCalledTimes(1);
    expect(outcome).toEqual({ ...refusal, method: 'local' });
  });

  it('sends a federated session to the provider instead of asking for a password', async () => {
    const run = vi.fn(() => Promise.resolve({ ...refusal, method: 'federated' as const }));
    const elevate = vi.fn(() => Promise.resolve(true));
    const reauthenticate = vi.fn();
    await withStepUp(run, { elevate, reauthenticate });
    expect(elevate).not.toHaveBeenCalled();
    expect(reauthenticate).toHaveBeenCalledTimes(1);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('does neither when the deployment holds no step-up credential at all', async () => {
    // Telling the reader to authenticate again would be telling them to do
    // something that cannot work; the server's own sentence is all there is.
    const run = vi.fn(() => Promise.resolve({ ...refusal, method: 'unavailable' as const }));
    const elevate = vi.fn(() => Promise.resolve(true));
    const reauthenticate = vi.fn();
    const outcome = await withStepUp(run, { elevate, reauthenticate });
    expect(elevate).not.toHaveBeenCalled();
    expect(reauthenticate).not.toHaveBeenCalled();
    expect(outcome).toEqual({ ...refusal, method: 'unavailable' });
  });
});

/** The 428 the control plane answers a settings write with, once. */
function stepUpOnce(): (path: string) => unknown {
  let refused = false;
  return (path: string) => {
    if (path.startsWith('/api/settings/cloud/test') && !refused) {
      refused = true;
      return new Response(JSON.stringify({ code: 'step_up_required', method: 'local' }), {
        status: 428,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    return controlPlane(path);
  };
}

/** Answers the credential dialog the way a reader does. */
async function answerPrompt(): Promise<void> {
  const dialog = document.body.querySelector('dialog.stepup-dialog');
  expect(dialog, 'no credential prompt was raised').not.toBeNull();
  const fields = (dialog as ParentNode).querySelectorAll('input');
  (fields[0] as HTMLInputElement).value = 'root';
  (fields[1] as HTMLInputElement).value = 'hunter2';
  await press(control(dialog as ParentNode, 'Sign In'));
}

describe('answering the credential prompt writes only what was asked for', () => {
  it('does not PUT the settings form when Test Connection is the write being retried', async () => {
    // `createPortal` moves the dialog into the top layer and leaves it where it
    // was declared in the REACT tree, which is the tree a submit propagates
    // along. Declared inside the Proxmox panel, the prompt's own submit reached
    // `#settings-form`'s onSubmit — so answering a password for a connection
    // test PUT all forty-five controls, and a successful PUT empties the four
    // secret inputs, where an empty secret is the wire spelling of "keep the
    // stored one". Nothing on screen said which of the two had happened.
    const calls = captureRoutes(stepUpOnce());
    const page = await mount(pageFor('settings'), { projectID: SCOPED_PROJECT, ready: true });
    await press(control(page.container, 'Test Connection'));
    await answerPrompt();

    expect(calls.filter((call) => call.path.startsWith('/auth/step-up'))).toHaveLength(1);
    // The write that was actually asked for: refused, then repeated with the
    // credential in hand.
    expect(calls.filter((call) => call.path.startsWith('/api/settings/cloud/test'))).toHaveLength(
      2,
    );
    // And the one that was not.
    expect(
      calls.filter((call) => call.path === '/api/settings/cloud' && call.method === 'PUT'),
    ).toEqual([]);
  });
});

/* ---- modality ------------------------------------------------------------ */

/** A page with one control, which raises a credential prompt when activated. */
function StepUpHarness() {
  const stepUp = useStepUp();
  return (
    <>
      <button
        type="button"
        id="opener"
        onClick={() => {
          void stepUp.guard(() =>
            Promise.resolve({
              kind: 'step-up' as const,
              method: 'local' as const,
              message: 'fresh authentication required',
            }),
          );
        }}
      >
        Write
      </button>
      {stepUp.prompt}
    </>
  );
}

/** The same, for the confirmation the destructive bulk actions pass through. */
function ConfirmHarness() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button
        type="button"
        id="opener"
        onClick={() => {
          setOpen(true);
        }}
      >
        Delete
      </button>
      {open ? (
        <ConfirmDialog
          open
          titleKey="builds.cleanup.confirm"
          detail="12 records"
          confirmLabel="Confirm"
          busy={false}
          onConfirm={() => {
            setOpen(false);
          }}
          onCancel={() => {
            setOpen(false);
          }}
        />
      ) : null}
    </>
  );
}

describe('a modal hands the reader back to the control that opened it', () => {
  // The dialogs are opened with showModal and then taken down by unmounting,
  // never closed — so the browser had nothing left to restore focus from and
  // every dismissal dropped the reading position onto <body>. A keyboard reader
  // who cancelled a delete on /builds landed above the page head, a whole list
  // away from the button they had just used.
  //
  // jsdom implements neither showModal nor close, which is why both components
  // carry a fallback and why the restoration below has to be stated outright
  // rather than left to the platform.
  it('returns focus after the credential prompt is cancelled', async () => {
    const page = await mount(<StepUpHarness />, { projectID: SCOPED_PROJECT, ready: true });
    const opener = page.container.querySelector('#opener') as HTMLElement;
    await press(opener);
    const dialog = document.body.querySelector('dialog.stepup-dialog');
    expect(dialog, 'no credential prompt was raised').not.toBeNull();
    await press(control(dialog as ParentNode, 'Cancel'));
    expect(document.body.querySelector('dialog.stepup-dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it('returns focus after the confirmation is cancelled', async () => {
    const page = await mount(<ConfirmHarness />, { projectID: SCOPED_PROJECT, ready: true });
    const opener = page.container.querySelector('#opener') as HTMLElement;
    await press(opener);
    expect(page.container.querySelector('dialog')).not.toBeNull();
    await press(control(page.container.querySelector('dialog') as ParentNode, 'Cancel'));
    expect(page.container.querySelector('dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it('returns focus after the confirmation is confirmed', async () => {
    // Confirming is the path a reader takes on purpose, and it lost focus the
    // same way cancelling did.
    const page = await mount(<ConfirmHarness />, { projectID: SCOPED_PROJECT, ready: true });
    const opener = page.container.querySelector('#opener') as HTMLElement;
    await press(opener);
    await press(control(page.container.querySelector('dialog') as ParentNode, 'Confirm'));
    expect(page.container.querySelector('dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it('returns focus when the dialog is dismissed with Escape', async () => {
    // Escape is the browser's own dismissal and it arrives as `cancel` on the
    // element, not as a click on anything — the path most likely to be left out.
    const page = await mount(<ConfirmHarness />, { projectID: SCOPED_PROJECT, ready: true });
    const opener = page.container.querySelector('#opener') as HTMLElement;
    await press(opener);
    const dialog = page.container.querySelector('dialog') as HTMLElement;
    // Standing in for showModal, which jsdom does not implement: a modal that
    // has opened holds the reading position, and Escape is dismissing it from
    // inside. Without this the reader never left the opener and the case would
    // pass on a dialog that restores nothing.
    control(dialog, 'Cancel').focus();
    await act(async () => {
      dialog.dispatchEvent(new Event('cancel', { bubbles: true, cancelable: true }));
      await Promise.resolve();
    });
    await settle();
    expect(page.container.querySelector('dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });
});

describe('an expired session is announced, so the shell can act on it', () => {
  it('tells the listener on every 401, including a poll it did not start', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('', { status: 401 }))),
    );
    const expired = vi.fn();
    const stop = onSessionExpired(expired);
    expect(await request('/api/status')).toEqual({ kind: 'unauthorized' });
    expect(await request('/api/builds')).toEqual({ kind: 'unauthorized' });
    expect(expired).toHaveBeenCalledTimes(2);
    stop();
    await request('/api/status');
    expect(expired).toHaveBeenCalledTimes(2);
  });
});

function policy(overrides: Partial<ProjectPolicy> = {}): ProjectPolicy {
  return {
    project_id: 'p1',
    suspended: false,
    priority_weight: 100,
    starvation_threshold_seconds: 300,
    max_queued_jobs: 10,
    max_active_jobs: 4,
    max_daily_submissions: 50,
    max_active_vcpus: 64,
    max_active_memory_mib: 131072,
    max_active_disk_gib: 512,
    max_artifact_bytes_per_job: 0,
    max_daily_build_seconds: 36000,
    max_daily_cloud_cost_microunits: 1000000,
    max_failures_per_hour: 20,
    abuse_cooldown_seconds: 900,
    abuse_suspended: false,
    abuse_generation: 0,
    max_claimed_attempts: 3,
    max_provision_attempts: 3,
    max_build_attempts: 3,
    max_verify_attempts: 3,
    max_publish_attempts: 3,
    queued_jobs: 1,
    active_jobs: 1,
    submissions_today: 2,
    reserved_vcpus: 4,
    reserved_memory_mib: 8192,
    reserved_disk_gib: 40,
    quarantine_bytes: 1048576,
    active_artifact_budgets: 1,
    build_seconds_today: 600,
    cloud_cost_microunits_today: 1000,
    active_runtime_budgets: 0,
    failures_last_hour: 0,
    claimed_reservations: 1,
    provision_reservations: 0,
    build_reservations: 2,
    verify_reservations: 0,
    publish_reservations: 0,
    waiting_reservations: 0,
    phase_work_shadow: 0,
    phase_work_active: 3,
    phase_work_blocked: 0,
    phase_work_ready: 2,
    phase_work_unschedulable: 0,
    phase_work_claimed: 1,
    phase_work_failed: 0,
    submission_day_starts_at: '2026-08-01T00:00:00Z',
    submission_day_ends_at: '2026-08-02T00:00:00Z',
    version: 4,
    updated_by: 'operator',
    updated_at: '2026-08-01T12:00:00Z',
    ...overrides,
  };
}

describe('the quota rail says what a refused submit is about', () => {
  it('keeps the four limits that stop a build resident and folds the rest', async () => {
    captureFetch(policy());
    const rail = await mount(<ProjectQuota />, { projectID: 'p1', ready: true });
    expect(rail.found('.quota-meter')).toHaveLength(4);
    // The five folded ones are readings, not absences: they are behind the
    // disclosure, which is what a 240px rail can hold.
    expect(rail.found('.quota-dl dt').length).toBeGreaterThan(5);
    expect(rail.text).toContain('More');
  });

  it('promotes a folded meter that has crossed its threshold, so 95% cannot hide', async () => {
    captureFetch(policy({ reserved_vcpus: 64 }));
    const rail = await mount(<ProjectQuota />, { projectID: 'p1', ready: true });
    const meters = rail.found('.quota-meter');
    expect(meters).toHaveLength(5);
    // The attribute is one of the meter's two channels; the hue it drives and
    // the mark the stylesheet appends after the reading are asserted off the
    // emitted rules in src/styles/meters.test.ts, one case each.
    expect(meters.some((meter) => meter.getAttribute('data-level') === 'crit')).toBe(true);
  });

  it('says a project is suspended, which is the first thing looked for', async () => {
    captureFetch(policy({ suspended: true }));
    const rail = await mount(<ProjectQuota />, { projectID: 'p1', ready: true });
    expect(rail.found('.policy-summary')[0]?.getAttribute('data-state')).toBe('suspended');
    expect(rail.text).toContain('Suspended');
  });

  it('names the abuse cooldown separately, because it is a different fact', async () => {
    captureFetch(policy({ abuse_suspended: true }));
    const rail = await mount(<ProjectQuota />, { projectID: 'p1', ready: true });
    expect(rail.found('.policy-summary')[0]?.getAttribute('data-state')).toBe('suspended');
    expect(rail.text).toContain('Abuse cooldown');
  });

  it('asks for nothing and claims nothing when IAM named no project', async () => {
    const calls = captureFetch(policy());
    const rail = await mount(<ProjectQuota />, { projectID: '', ready: true });
    expect(calls).toEqual([]);
    // Not the loading sentence: the console this replaces left "Loading project
    // quota…" up forever here, which claims a request is still coming.
    expect(rail.text).toBe('Project quota unavailable');
  });
});
