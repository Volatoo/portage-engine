import { act } from 'react';
import { createRoot } from 'react-dom/client';
import type { Root } from 'react-dom/client';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { CloudSettingsResponse } from '../../api/types';
import { ProjectScopeContext } from '../../app/project';
import { MessagesProvider } from '../../i18n/context';
import { SettingsPage } from './SettingsPage';
import { attributeFailure } from './attribution';
import {
  EMPTY_DRAFT,
  SECRET_CONTROLS,
  TEXT_CONTROLS,
  clearSecrets,
  fromResponse,
  toPayload,
  withText,
} from './model';
import type { ControlID } from './model';
import { SECTIONS } from './sections';
import { decodeEntities } from './text';

/**
 * What the console this replaces did with this form, and what must never come
 * back.
 *
 * Measured with every non-GET delayed three seconds: two clicks eighty
 * milliseconds apart produced two complete `PUT /api/settings/cloud`, at +0ms
 * and +3017ms, and five activations produced three. The extra activations were
 * queued and replayed rather than dropped — and a successful write ends by
 * emptying the secret inputs, because an empty secret is the wire spelling of
 * "keep the stored one". So the replayed submission collected inputs the write
 * it was replaying had just cleared, and PUT blank Proxmox and AWS credentials
 * over good ones. The form looked saved either way.
 *
 * The delay is a deferred rather than a timer here, so "after the flight ends"
 * is a point the test controls exactly: every count below is taken once the
 * write has resolved and the tree has settled, not while it is still out.
 */

interface Deferred {
  promise: Promise<Response>;
  resolve: (response: Response) => void;
}

function deferred(): Deferred {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => {
    resolve = settle;
  });
  return { promise, resolve };
}

const SAVED: CloudSettingsResponse = {
  provider: 'pve',
  remote_builders: [],
  gcp_project: '',
  gcp_region: '',
  gcp_zone: '',
  gcp_key_file: '',
  aws_region: 'us-east-1',
  aws_zone: '',
  aws_access_key: 'AKIAEXAMPLE',
  pve_endpoint: 'https://pve.example.test:8006',
  pve_node: 'auto',
  pve_nodes: [],
  pve_token_id: 'root@pam!terraform',
  pve_username: '',
  pve_insecure: false,
  pve_storage: 'local-lvm',
  pve_network: 'vmbr0',
  pve_template: 'gentoo-native-cloudinit-template',
  pve_cicustom: '',
  pve_nameserver: '',
  ssh_key_path: '/var/lib/portage-engine/id_ed25519',
  ssh_user: 'root',
  ssh_known_hosts: '',
  ssh_insecure_host_key: false,
  gentoo_mirror: '',
  portage_sync_uri: '',
  portage_sync_method: 'webrsync',
  make_conf_extra: '',
  build_features: '',
  build_mode: 'native-gentoo',
  server_callback_url: 'http://10.0.0.10:8080',
  builder_binary_path: '',
  builder_binary_url: '',
  builder_binary_sha256: '',
  instance_ttl_minutes: 30,
  skip_verify_install: false,
  upload_url: '',
  upload_user: '',
  upload_dir: '',
  has_pve_token_secret: true,
  has_pve_password: true,
  has_aws_secret_key: true,
  has_upload_password: true,
  secret_values_managed_externally: false,
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** The rejections `http.Error` produces: text/plain, and prose. */
function refusal(sentence: string): Response {
  return new Response(`${sentence}\n`, {
    status: 400,
    headers: { 'Content-Type': 'text/plain' },
  });
}

interface Harness {
  container: HTMLElement;
  /** Every settings write the page issued, oldest first, with its parsed body. */
  writes: Record<string, unknown>[];
  /**
   * The deferreds those writes are waiting on, in the same order. Resolving one
   * with a Response is how a test says "the network answered, now".
   */
  pending: Deferred[];
  unmount: () => void;
}

let root: Root | null = null;

function harness(settings: CloudSettingsResponse = SAVED): Harness {
  const writes: Record<string, unknown>[] = [];
  const pending: Deferred[] = [];

  vi.stubGlobal(
    'fetch',
    vi.fn((input: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      if (input.startsWith('/api/settings/cloud') && method === 'PUT') {
        const body = typeof init?.body === 'string' ? init.body : '{}';
        writes.push(JSON.parse(body) as Record<string, unknown>);
        // Held open until the test says so. Nothing about this page may depend
        // on a write finishing quickly, which is the whole point.
        const gate = deferred();
        pending.push(gate);
        return gate.promise;
      }
      if (input.startsWith('/api/settings/cloud')) {
        return Promise.resolve(json(settings));
      }
      if (input.startsWith('/api/gpg/status')) {
        return Promise.resolve(
          json({ enabled: true, ready: false, key_id: '', mode: 'isolated-outbound-pull' }),
        );
      }
      if (input.startsWith('/api/iam/sessions')) {
        return Promise.resolve(json({ sessions: [], current_session_id: 'abc' }));
      }
      if (input.startsWith('/api/iam/me')) {
        return Promise.resolve(
          json({ principal: { subject: 'operator' }, projects: [], identity_providers: [] }),
        );
      }
      return Promise.resolve(json({}));
    }),
  );

  const container = document.createElement('div');
  document.body.appendChild(container);
  const mounted = createRoot(container);
  root = mounted;
  act(() => {
    mounted.render(
      <MessagesProvider lang="en">
        {/* The chrome settles which project every request is made in, and a
            page waits for it: mounted without one, nothing here would ask the
            control plane anything. */}
        <ProjectScopeContext value={{ projectID: 'p1', ready: true }}>
          <MemoryRouter>
            <SettingsPage />
          </MemoryRouter>
        </ProjectScopeContext>
      </MessagesProvider>,
    );
  });

  return {
    container,
    writes,
    pending,
    unmount: () => {
      act(() => {
        mounted.unmount();
      });
      container.remove();
      root = null;
    },
  };
}

/** Lets every settled promise and every effect land. */
async function settle(): Promise<void> {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

function control(page: Harness, id: string): HTMLInputElement {
  const node = page.container.querySelector<HTMLInputElement>(`#${id}`);
  if (node === null) {
    throw new Error(`the form has no control named ${id}`);
  }
  return node;
}

/** Types into a controlled input the way a browser does. */
function type(page: Harness, id: string, value: string): void {
  const node = control(page, id);
  // Through the prototype's own setter, because React tracks the value it last
  // wrote: assigning `node.value` leaves the tracker agreeing with the new
  // string, and the input event that follows is then treated as a no-op.
  const descriptor = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value');
  act(() => {
    if (descriptor?.set === undefined) {
      node.value = value;
    } else {
      descriptor.set.call(node, value);
    }
    node.dispatchEvent(new Event('input', { bubbles: true }));
  });
}

function saveButton(page: Harness): HTMLButtonElement {
  const node = page.container.querySelector<HTMLButtonElement>(
    '.settings-footer button[type=submit]',
  );
  if (node === null) {
    throw new Error('the form has no Save button');
  }
  return node;
}

/**
 * One activation of Save.
 *
 * The submit event is dispatched rather than the button clicked, because jsdom
 * does not implement implicit form submission — and because a submit event is
 * what every one of the four activation paths actually produces in a browser: a
 * pointer on the button, a held Enter in a text field, and an Enter that
 * confirmed an IME candidate all arrive here.
 */
function activate(page: Harness): void {
  const form = page.container.querySelector('form');
  act(() => {
    form?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  });
}

beforeEach(() => {
  (globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});

afterEach(() => {
  if (root !== null) {
    act(() => {
      root?.unmount();
    });
    root = null;
  }
  document.body.replaceChildren();
  vi.unstubAllGlobals();
});

async function loaded(settings: CloudSettingsResponse = SAVED): Promise<Harness> {
  const page = harness(settings);
  await settle();
  return page;
}

const SECRETS_TYPED = {
  pve_token_secret: 'pve-token-value',
  pve_password: 'pve-password-value',
  aws_secret_key: 'aws-secret-value',
  upload_password: 'upload-password-value',
};

function typeEverySecret(page: Harness): void {
  for (const [id, value] of Object.entries(SECRETS_TYPED)) {
    type(page, id, value);
  }
}

describe('the settings resource has exactly one writer', () => {
  it('answers a second activation eighty milliseconds into a write with nothing at all', async () => {
    const page = await loaded();
    typeEverySecret(page);

    activate(page);
    activate(page);
    expect(page.writes).toHaveLength(1);

    // The count that matters is taken AFTER the flight ends. The defect this
    // pins was invisible during the flight: the second write was queued, and it
    // went out when the first one came back.
    act(() => {
      page.pending[0]?.resolve(json(SAVED));
    });
    await settle();
    expect(page.writes).toHaveLength(1);
    expect(page.pending).toHaveLength(1);
  });

  it('drops four of five activations rather than queueing three of them', async () => {
    const page = await loaded();
    for (let attempt = 0; attempt < 5; attempt += 1) {
      activate(page);
    }
    expect(page.writes).toHaveLength(1);
    act(() => {
      page.pending[0]?.resolve(json(SAVED));
    });
    await settle();
    expect(page.writes).toHaveLength(1);
  });

  it('carries the typed credentials on the one write it does send', async () => {
    const page = await loaded();
    typeEverySecret(page);
    activate(page);
    activate(page);
    act(() => {
      page.pending[0]?.resolve(json(SAVED));
    });
    await settle();

    expect(page.writes).toHaveLength(1);
    expect(page.writes[0]).toMatchObject(SECRETS_TYPED);
    // Nothing blank reached the wire. A blank secret is the wire spelling of
    // "keep the stored one", so a replay of this write would have asked the
    // server to keep credentials the operator had just replaced — and the form
    // would have looked identical either way.
    for (const secret of SECRET_CONTROLS) {
      expect(page.writes[0]?.[secret]).not.toBe('');
    }
  });

  it('never sends a blank credential after the operator typed a real one', async () => {
    // The production incident, end to end: type all four secrets, double-click
    // Save, let the slow write land, then let everything settle. Under
    // queue-and-replay this is where the second PUT went out, carrying the
    // inputs the first one had just emptied — and `""` is the wire spelling of
    // "keep the stored one", so the server would have kept nothing and said
    // nothing, and the form would have read as saved.
    const page = await loaded();
    typeEverySecret(page);
    activate(page);
    activate(page);
    act(() => {
      page.pending[0]?.resolve(json(SAVED));
    });
    await settle();
    await settle();

    for (const write of page.writes) {
      for (const secret of SECRET_CONTROLS) {
        expect({ secret, value: write[secret] }).toEqual({
          secret,
          value: SECRETS_TYPED[secret],
        });
      }
    }
    expect(page.writes).toHaveLength(1);
  });

  it('leaves the secret inputs alone while the write is still out', async () => {
    const page = await loaded();
    typeEverySecret(page);
    activate(page);
    await settle();

    // Still in flight: clearing here is what made the replay destructive.
    expect(page.pending).toHaveLength(1);
    for (const [id, value] of Object.entries(SECRETS_TYPED)) {
      expect(control(page, id).value).toBe(value);
    }
  });

  it('empties all four secrets once the write has landed, including the mirror password', async () => {
    const page = await loaded();
    typeEverySecret(page);
    activate(page);
    act(() => {
      page.pending[0]?.resolve(json(SAVED));
    });
    await settle();

    // The console this replaces cleared three of these and omitted
    // `upload_password`, so the mirror credential behaved differently from the
    // other three for no reason anybody could state.
    for (const secret of SECRET_CONTROLS) {
      expect(control(page, secret).value).toBe('');
    }
  });

  it('reports the save in a region that carries the sentence and nothing else', async () => {
    const page = await loaded();
    activate(page);
    act(() => {
      page.pending[0]?.resolve(json(SAVED));
    });
    await settle();
    const region = page.container.querySelector('.settings-footer .settings-msg');
    expect(region?.getAttribute('role')).toBe('status');
    expect(region?.getAttribute('data-state')).toBe('ok');
    expect(region?.textContent).toBe('Saved — in effect immediately');
    // The region holds text, never the panels: a live region wrapped around
    // rendered content re-announces that content on every change.
    expect(region?.querySelector('input')).toBeNull();
  });

  it('keeps the button in the tab order while it is busy', async () => {
    const page = await loaded();
    activate(page);
    await settle();
    const button = saveButton(page);
    // aria-disabled and aria-busy, never `disabled`: `disabled` drops the
    // control out of the tab order in the middle of the operation and a
    // keyboard reader loses the element they just used. The guard that actually
    // stops the second write is the writer, not the attribute.
    expect(button.disabled).toBe(false);
    expect(button.getAttribute('aria-disabled')).toBe('true');
    expect(button.getAttribute('aria-busy')).toBe('true');
    act(() => {
      page.pending[0]?.resolve(json(SAVED));
    });
    await settle();
    expect(saveButton(page).getAttribute('aria-disabled')).toBe('false');
    expect(saveButton(page).hasAttribute('aria-busy')).toBe(false);
  });

  it('refuses an Enter that only confirmed an IME candidate', async () => {
    const page = await loaded();
    const form = page.container.querySelector('form');
    const composing = new KeyboardEvent('keydown', {
      key: 'Enter',
      bubbles: true,
      cancelable: true,
      isComposing: true,
    });
    act(() => {
      control(page, 'pve_endpoint').dispatchEvent(composing);
    });
    expect(composing.defaultPrevented).toBe(true);
    expect(page.writes).toHaveLength(0);

    // And an Enter that is a submission still is one.
    const plain = new KeyboardEvent('keydown', {
      key: 'Enter',
      bubbles: true,
      cancelable: true,
    });
    act(() => {
      control(page, 'pve_endpoint').dispatchEvent(plain);
      form?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    });
    expect(plain.defaultPrevented).toBe(false);
    expect(page.writes).toHaveLength(1);
  });
});

describe('a rejection is never silent', () => {
  it('opens the panel, focuses the control and points a describedby at the reason', async () => {
    const page = await loaded();
    activate(page);
    act(() => {
      page.pending[0]?.resolve(refusal('pve_endpoint must be an absolute URL'));
    });
    await settle();

    const pvePanel = page.container.querySelector('#pve');
    expect(pvePanel?.hasAttribute('hidden')).toBe(false);
    expect(page.container.querySelector('#general')?.hasAttribute('hidden')).toBe(true);

    const input = control(page, 'pve_endpoint');
    expect(input.getAttribute('aria-invalid')).toBe('true');
    expect(input.getAttribute('aria-describedby')).toContain('pve_endpoint-error');
    expect(document.activeElement).toBe(input);

    const note = page.container.querySelector('#pve_endpoint-error');
    expect(note?.getAttribute('role')).toBe('alert');
    expect(note?.textContent).toContain('pve_endpoint must be an absolute URL');

    // The sub-navigation says which panel is open with aria-current, not with a
    // class the stylesheet and a screen reader could read differently.
    const link = page.container.querySelector('#subnav a[data-sec=pve]');
    expect(link?.getAttribute('aria-current')).toBe('true');
    expect(
      page.container.querySelector('#subnav a[data-sec=general]')?.hasAttribute('aria-current'),
    ).toBe(false);
  });

  it('says so loudly when the sentence names no field it can reach', async () => {
    const page = await loaded();
    activate(page);
    // The real sentence handleCloudSettings answers with when the deployment
    // owns the credentials. It names four environment variables and no JSON
    // field, so nothing on the form can carry it — which is exactly the case
    // that used to attribute nothing and say nothing.
    const sentence =
      'credential values cannot be persisted through the shared settings API; inject ' +
      'CLOUD_PVE_TOKEN_SECRET, CLOUD_PVE_PASSWORD, CLOUD_AWS_SECRET_KEY, or ' +
      'PORTAGE_UPLOAD_PASSWORD via the deployment secret provider';
    act(() => {
      page.pending[0]?.resolve(refusal(sentence));
    });
    await settle();

    expect(page.container.querySelector('[aria-invalid="true"]')).toBeNull();
    const region = page.container.querySelector<HTMLElement>('.settings-footer .settings-msg');
    expect(region?.getAttribute('data-state')).toBe('failed');
    expect(region?.textContent).toBe(`Save failed: ${sentence}`);
    // A form that stays put reads as a form that did nothing at all, so the
    // reading position moves to the sentence that says it failed.
    expect(document.activeElement).toBe(region);
  });

  it('clears the previous attribution before the next write', async () => {
    const page = await loaded();
    activate(page);
    act(() => {
      page.pending[0]?.resolve(refusal('pve_endpoint must be an absolute URL'));
    });
    await settle();
    expect(control(page, 'pve_endpoint').getAttribute('aria-invalid')).toBe('true');

    activate(page);
    await settle();
    expect(control(page, 'pve_endpoint').hasAttribute('aria-invalid')).toBe(false);
    expect(page.container.querySelector('#pve_endpoint-error')).toBeNull();
  });
});

describe('attribution, without a document', () => {
  const reachable = (): boolean => false;

  it('prefers the longest wire name, so a prefix cannot steal a rejection', () => {
    // `pve_node` is a prefix of `pve_nodes`, and a first-match-wins scan in
    // declaration order focused the single-node input for a rejection about the
    // candidate list — a control the operator never touched.
    expect(attributeFailure('pve_nodes must be a list', reachable)?.control).toBe('pve_nodes');
    expect(attributeFailure('pve_node must name a cluster member', reachable)?.control).toBe(
      'pve_node_manual',
    );
  });

  it('refuses a control the reader cannot reach', () => {
    const message =
      'skip_verify_install is no longer supported: verification is mandatory before publication';
    // The checkbox is disabled because the server refuses any other value, so
    // attributing to it is indistinguishable from attributing to nothing.
    const unreachable = (control: ControlID): boolean => control === 'verify_install';
    expect(attributeFailure(message, reachable)?.control).toBe('verify_install');
    expect(attributeFailure(message, unreachable)).toBeNull();
  });

  it('names the panel a rejection has to open', () => {
    expect(attributeFailure('upload_dir is invalid', reachable)?.section).toBe('mirrors');
    expect(attributeFailure('builder_binary_sha256 is malformed', reachable)?.section).toBe('net');
  });

  it('answers null for a sentence naming nothing', () => {
    expect(attributeFailure('settings applied but not persisted: disk full', reachable)).toBeNull();
  });

  it('tells the field named `provider` from the English word', () => {
    // handleCloudSettings writes both: `unsupported provider %q` names the
    // field, and the credential refusal ends "…via the deployment secret
    // provider", where it is a noun. Matched bare, the second one opened the
    // Backend panel and focused the select — a rejection pointing at a control
    // the operator never touched.
    expect(attributeFailure('unsupported provider "azure"', reachable)?.control).toBe('provider');
    expect(
      attributeFailure(
        'credential values cannot be persisted through the shared settings API; inject ' +
          'CLOUD_PVE_TOKEN_SECRET via the deployment secret provider',
        reachable,
      ),
    ).toBeNull();
  });
});

describe('the form the sweep measured', () => {
  it('renders forty-five controls and gives every one of them a label', async () => {
    const page = await loaded();
    const controls = page.container.querySelectorAll<HTMLElement>(
      '#settings-form input, #settings-form select, #settings-form textarea',
    );
    expect(controls).toHaveLength(45);
    const unlabelled = [...controls].filter(
      (node) => page.container.querySelector(`label[for="${node.id}"]`) === null,
    );
    expect(unlabelled.map((node) => node.id)).toEqual([]);
  });

  it('keeps ten of the eleven panels hidden, and their controls out of the tab order', async () => {
    const page = await loaded();
    const panels = page.container.querySelectorAll('#settings-form section.panel');
    expect(panels).toHaveLength(11);
    const open = [...panels].filter((panel) => !panel.hasAttribute('hidden'));
    expect(open.map((panel) => panel.id)).toEqual(['general']);
  });

  it('offers all eleven sections as links a keyboard can reach', async () => {
    const page = await loaded();
    const links = page.container.querySelectorAll<HTMLAnchorElement>('#subnav a[data-sec]');
    expect(links).toHaveLength(11);
    for (const link of links) {
      // An href is what puts a link in the tab order and makes Enter operate it
      // with no script at all; the fragment is a real element, so a deep link
      // moves reading position to the panel rather than to nothing.
      expect(link.getAttribute('href')).toBe(`#${link.dataset['sec'] ?? ''}`);
      expect(
        page.container.querySelector(`section.panel#${link.dataset['sec'] ?? ''}`),
      ).not.toBeNull();
    }
  });

  it('states secret status in the placeholder as well as in the hint', async () => {
    const page = await loaded();
    const token = control(page, 'pve_token_secret');
    expect(token.placeholder).toBe('Saved — leave empty to keep');
    expect(token.getAttribute('aria-describedby')).toContain('pve_token_secret-hint');
    expect(page.container.querySelector('#pve_token_secret-hint')?.textContent).toBe(
      'Saved; leave empty to keep',
    );
  });

  it('disables the four secrets and says who owns them when the deployment does', async () => {
    // The PostgreSQL-authority branch of handleCloudSettings: the settings API
    // refuses to carry credential values at all, so the four inputs are not
    // editable and the placeholder is the only statement of that.
    const page = await loaded({ ...SAVED, secret_values_managed_externally: true });
    for (const secret of SECRET_CONTROLS) {
      const input = control(page, secret);
      expect(input.disabled).toBe(true);
      expect(input.placeholder).toBe('Managed by deployment environment');
      expect(page.container.querySelector(`#${secret}-hint`)?.textContent).toBe(
        'Managed by deployment environment (configured)',
      );
    }
  });

  it('sends a rejection naming a deployment-owned secret to the footer, not to a dead control', async () => {
    const page = await loaded({ ...SAVED, secret_values_managed_externally: true });
    activate(page);
    act(() => {
      page.pending[0]?.resolve(refusal('pve_token_secret cannot be set here'));
    });
    await settle();
    // The control the sentence names takes no focus and carries no aria-invalid
    // anybody will ever reach, so attributing to it would be the same as
    // attributing to nothing.
    expect(control(page, 'pve_token_secret').hasAttribute('aria-invalid')).toBe(false);
    const region = page.container.querySelector<HTMLElement>('.settings-footer .settings-msg');
    expect(region?.getAttribute('data-state')).toBe('failed');
    expect(document.activeElement).toBe(region);
  });
});

describe('the draft, as a value', () => {
  it('sends every field whether or not its panel was opened', () => {
    const payload = toPayload(withText(EMPTY_DRAFT, 'gcp_project', 'my-project'));
    expect(payload.gcp_project).toBe('my-project');
    // Two fields the server states rather than accepts, so the form states them
    // too: verification is mandatory, and the Docker builders are gone.
    expect(payload.skip_verify_install).toBe(false);
    expect(payload.build_mode).toBe('native-gentoo');
  });

  it('spells an unpinned cluster "auto" and a pinned one by name', () => {
    expect(toPayload(EMPTY_DRAFT).pve_node).toBe('auto');
    const pinned = {
      ...withText(EMPTY_DRAFT, 'pve_node_manual', 'pve2'),
      placement: 'manual' as const,
    };
    expect(toPayload(pinned).pve_node).toBe('pve2');
    // A pin with no name is still a pin, and "pve" is the node every default
    // single-host install has.
    expect(toPayload({ ...EMPTY_DRAFT, placement: 'manual' }).pve_node).toBe('pve');
  });

  it('trims every field except the one appended verbatim to make.conf', () => {
    const draft = withText(
      withText(EMPTY_DRAFT, 'ssh_user', '  root  '),
      'make_conf_extra',
      '  USE="-doc"\n',
    );
    expect(toPayload(draft).ssh_user).toBe('root');
    // Leading whitespace on a continuation line is part of what the operator
    // wrote, and this string lands in a generated make.conf unchanged.
    expect(toPayload(draft).make_conf_extra).toBe('  USE="-doc"\n');
  });

  it('empties all four secrets and nothing else', () => {
    let draft = EMPTY_DRAFT;
    for (const secret of SECRET_CONTROLS) {
      draft = withText(draft, secret, 'typed');
    }
    draft = withText(draft, 'pve_token_id', 'root@pam!tf');
    const cleared = clearSecrets(draft);
    expect(SECRET_CONTROLS.map((secret) => cleared.text[secret])).toEqual(['', '', '', '']);
    expect(cleared.text.pve_token_id).toBe('root@pam!tf');
  });

  it('never blanks a secret the operator is still typing when the payload lands', () => {
    const typing = withText(EMPTY_DRAFT, 'pve_password', 'half-typed');
    // The GET response redacts every secret, so reading one out of it would
    // empty the input mid-keystroke.
    expect(fromResponse(SAVED, typing).text.pve_password).toBe('half-typed');
    expect(fromResponse(SAVED, typing).text.pve_endpoint).toBe('https://pve.example.test:8006');
  });

  it('reads an empty node as automatic placement', () => {
    expect(fromResponse({ ...SAVED, pve_node: '' }).placement).toBe('auto');
    expect(fromResponse({ ...SAVED, pve_node: 'AUTO' }).placement).toBe('auto');
    expect(fromResponse({ ...SAVED, pve_node: 'pve3' }).placement).toBe('manual');
  });
});

describe('the section table is the form', () => {
  it('places every control in exactly one panel', () => {
    const placed = SECTIONS.flatMap((section) => section.controls);
    expect(new Set(placed).size).toBe(placed.length);
    const expected: ControlID[] = [
      ...TEXT_CONTROLS,
      'pve_insecure',
      'ssh_insecure',
      'verify_install',
      'place_auto',
      'place_manual',
    ];
    expect([...placed].sort()).toEqual([...expected].sort());
    expect(placed).toHaveLength(45);
  });
});

describe('the four transcribed catalogue entries', () => {
  it('decodes the XML entities the Go template had to write', () => {
    // React escapes a text node, so `&amp;` reaches the reader as characters.
    // The catalogue is where this belongs; until then the four keys this page
    // reads are decoded at the page.
    expect(decodeEntities('Backend &amp; Test')).toBe('Backend & Test');
    expect(decodeEntities('/local/&lt;dir&gt;/…')).toBe('/local/<dir>/…');
    expect(decodeEntities('nothing to decode')).toBe('nothing to decode');
  });
});
