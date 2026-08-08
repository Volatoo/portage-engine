import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { describe, expect, it, vi } from 'vitest';

import type { ApiOutcome } from '../../api/client';
import type { DeviceDecision } from '../../api/types';
import { CONSOLE_BASE, CONSOLE_ROUTES } from '../../app/routes';
import type { ConsoleRoute } from '../../app/routes';
import type { BootPayload } from '../../boot/payload';
import { MessagesProvider } from '../../i18n/context';
import type { Language } from '../../i18n/messages';
import { DevicePage } from './DevicePage';
import { createDecisionGate, normalizeUserCode } from './decision';
import type { Decision } from './decision';

const DEVICE_ROUTE = CONSOLE_ROUTES.find((route) => route.name === 'device') as ConsoleRoute;

function boot(userCode = 'ABCD-2345'): BootPayload {
  return {
    lang: 'en',
    html_lang: 'en',
    auth_enabled: true,
    oidc_enabled: true,
    local_login_enabled: false,
    identity_providers: [],
    principal: null,
    step_up_method: 'local',
    route: { name: 'device', path: '/ui/device', job_id: '', instance_id: '', user_code: userCode },
    asset_base: '/static/ui/',
  };
}

function paint(lang: Language, at = '/ui/device?user_code=ABCD-2345'): string {
  return renderToStaticMarkup(
    <MessagesProvider lang={lang}>
      <MemoryRouter initialEntries={[at]} basename={CONSOLE_BASE}>
        <DevicePage route={DEVICE_ROUTE} boot={boot()} onLanguageChange={() => undefined} />
      </MemoryRouter>
    </MessagesProvider>,
  );
}

/** A send that never settles, so a burst can be dispatched against it. */
function stalled(): {
  send: (userCode: string, decision: Decision) => Promise<ApiOutcome<DeviceDecision>>;
  calls: { userCode: string; decision: Decision }[];
  settle: () => void;
} {
  const calls: { userCode: string; decision: Decision }[] = [];
  let release: (() => void) | null = null;
  const gateway = new Promise<void>((resolve) => {
    release = resolve;
  });
  return {
    calls,
    settle: () => {
      release?.();
    },
    send: async (userCode, decision) => {
      calls.push({ userCode, decision });
      await gateway;
      return { kind: 'ok', value: { status: 'approved' } };
    },
  };
}

describe('the code is the code the terminal is showing', () => {
  it('accepts what a reader retypes off another screen', () => {
    expect(normalizeUserCode('abcd2345')).toBe('ABCD-2345');
    expect(normalizeUserCode('  abcd-2345 ')).toBe('ABCD-2345');
    expect(normalizeUserCode('ABCD 2345')).toBe('ABCD-2345');
  });

  it('refuses the four glyphs the issuer left out of the alphabet', () => {
    // I, O, 0 and 1 are the pairs a proportional face makes ambiguous, which is
    // why the issuer does not mint them — a code containing one is a misread.
    for (const misread of ['IBCD2345', 'OBCD2345', '0BCD2345', '1BCD2345']) {
      expect(normalizeUserCode(misread), misread).toBe('');
    }
  });

  it('refuses a code that is not eight characters', () => {
    expect(normalizeUserCode('ABCD234')).toBe('');
    expect(normalizeUserCode('ABCD23456')).toBe('');
    expect(normalizeUserCode('')).toBe('');
  });
});

describe('one decision reaches the control plane, whatever the pointer did', () => {
  it('answers a double click with a single call', async () => {
    const backend = stalled();
    const gate = createDecisionGate(backend.send);
    const first = gate.decide('ABCD-2345', 'approve');
    const second = gate.decide('ABCD-2345', 'approve');
    backend.settle();
    await Promise.all([first, second]);
    expect(backend.calls).toHaveLength(1);
  });

  it('answers six activations 60ms apart with a single call', async () => {
    vi.useFakeTimers();
    try {
      const backend = stalled();
      const gate = createDecisionGate(backend.send);
      const pending: Promise<unknown>[] = [];
      for (let burst = 0; burst < 6; burst += 1) {
        pending.push(gate.decide('ABCD-2345', 'approve'));
        await vi.advanceTimersByTimeAsync(60);
      }
      backend.settle();
      await Promise.all(pending);
      expect(backend.calls).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('sends only the first of an approve and a deny dispatched in the same tick', async () => {
    // The dangerous one. These are two controls writing one resource, so a
    // per-button flag would let both reach the control plane — not a duplicate
    // request but a contradictory one, with the server breaking a tie for a
    // reader who pressed a single thing.
    const backend = stalled();
    const gate = createDecisionGate(backend.send);
    const approve = gate.decide('ABCD-2345', 'approve');
    const deny = gate.decide('ABCD-2345', 'deny');
    backend.settle();
    await Promise.all([approve, deny]);
    expect(backend.calls).toEqual([{ userCode: 'ABCD-2345', decision: 'approve' }]);
    expect(await deny).toBeNull();
  });

  it('sends nothing at all once the decision has landed', async () => {
    const backend = stalled();
    backend.settle();
    const gate = createDecisionGate(backend.send);
    await gate.decide('ABCD-2345', 'approve');
    gate.settle();
    expect(await gate.decide('ABCD-2345', 'deny')).toBeNull();
    expect(await gate.decide('ABCD-2345', 'approve')).toBeNull();
    expect(backend.calls).toHaveLength(1);
  });
});

describe('the card before IAM has answered', () => {
  it('offers neither action, because a capability is never assumed', () => {
    const markup = paint('en');
    expect(markup.split('aria-disabled="true"').length - 1).toBe(2);
    expect(markup).not.toContain('aria-disabled="false"');
  });

  it('renders the code the URL carried, in the face a terminal draws', () => {
    const markup = paint('en');
    expect(markup).toContain('value="ABCD-2345"');
    // The class is the whole mechanism: `.field input.device-code` is what puts
    // this string in the monospace face, and it wins only on source order.
    expect(markup).toContain('class="device-code"');
  });

  it('binds the result to the input and announces it from a node of its own', () => {
    const markup = paint('en');
    expect(markup).toContain('aria-describedby="device-hint device-result"');
    expect(markup).toContain('id="device-result"');
    expect(markup).toContain('role="alert"');
    // The state is an attribute, so the stylesheet and a screen reader read the
    // same value rather than a class and a sentence that can drift apart.
    expect(markup).toContain('data-state=""');
    expect(markup).toContain('aria-invalid="false"');
  });

  it('says what it is doing rather than leaving the identity line blank', () => {
    expect(paint('en')).toContain('Checking the current platform identity');
    expect(paint('zh')).toContain('正在确认当前平台身份');
  });
});

/**
 * Three of the four defects on this page were CSS, and all three are in the
 * shared stylesheet where a page cannot assert them by rendering. So they are
 * asserted against the stylesheet itself — which is also the only place a
 * break in them could come from.
 */
describe('the stylesheet keeps the three rules this card depends on', () => {
  const styles = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'styles');
  // Comments are stripped first: every one of these rules is annotated with the
  // failure it refuses, and a check that reads the annotation reads green
  // whether or not the declaration below it survived.
  const read = (name: string): string =>
    readFileSync(join(styles, name), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
  const surfaces = read('surfaces.css');
  const tokens = read('tokens.css');

  it('lets Approve and Deny share a row instead of stacking as two bars', () => {
    // `.auth-card .btn` sets width:100% and flex-basis auto reads it, so without
    // the width:auto here the destructive action becomes an identical bar 8px
    // under the confirming one — the adjacency a two-column layout exists to
    // prevent.
    expect(surfaces).toMatch(/\.device-actions \.btn \{[^}]*flex: 1 1 auto/);
    expect(surfaces).toMatch(/\.device-actions \.btn \{[^}]*width: auto/);
    expect(surfaces).toMatch(/\.device-actions \{[^}]*display: flex/);
  });

  it('gives the code the monospace face, declared after the rule it must beat', () => {
    const typed = surfaces.indexOf('.field input[type=text]');
    const code = surfaces.indexOf('.field input.device-code');
    expect(typed).toBeGreaterThanOrEqual(0);
    expect(code).toBeGreaterThan(typed);
    expect(surfaces).toMatch(/\.field input\.device-code \{[^}]*font-family: var\(--font-mono\)/);
  });

  it('writes the result in an ink that carries an outcome, not in a dot hue', () => {
    // The palette greens and oranges are dot hues and measure around 2:1 on
    // white; text stating what happened takes the deepened ink instead.
    expect(surfaces).toMatch(
      /\.device-result\[data-state="success"\] \{ color: var\(--successInk\); \}/,
    );
    expect(surfaces).toMatch(
      /\.device-result\[data-state="error"\] \{ color: var\(--dangerInk\); \}/,
    );
    expect(tokens).toContain('--successInk');
    expect(tokens).toContain('--dangerInk');
  });

  it('reserves both variable lines, so an answer does not move the buttons', () => {
    expect(surfaces).toMatch(/\.device-identity \{[^}]*min-height: 35px/);
    expect(surfaces).toMatch(/\.device-result \{[^}]*min-height: 35px/);
  });
});
