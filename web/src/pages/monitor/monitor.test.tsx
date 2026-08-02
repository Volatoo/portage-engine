import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { pollWhenVisible } from '../../api/stream';
import type { LedgerStatus, WorkerGatewayStatus, WorkloadIdentityInventory } from '../../api/types';
import { ProjectScopeContext } from '../../app/project';
import { MessagesProvider } from '../../i18n/context';
import { DARK_BLOCK } from '../../styles/modeblocks';
import { EN } from '../../i18n/messages.en';
import { messagesFor } from '../../i18n/messages';
import type { Language, MessageKey } from '../../i18n/messages';
import {
  GatewayCard,
  LedgerCard,
  MonitorPage,
  POLL_INTERVAL_MS,
  SchedulerCard,
  TargetCard,
} from './MonitorPage';
import { tolerateDegraded } from './health';
import type { MonitorSchedulerStatus, TargetReliabilityStatus } from './wire';
import { momentText, verdictAttributes, worstToken } from './verdicts';

/**
 * The four things /monitor was measured getting wrong, each pinned by the thing
 * a reader saw rather than by the code path that produced it.
 */

function paint(node: React.ReactNode, lang: Language = 'en'): string {
  return renderToStaticMarkup(<MessagesProvider lang={lang}>{node}</MessagesProvider>);
}

/** Go's zero time.Time, as it arrives on the wire. */
const GO_ZERO = '0001-01-01T00:00:00Z';

function ledgerWith(overrides: Partial<LedgerStatus> = {}): LedgerStatus {
  return {
    enabled: true,
    ok: true,
    authority: 'postgresql',
    writes: 4102,
    write_errors: 0,
    last_reconcile: {
      checked_at: GO_ZERO,
      legacy_count: 0,
      ledger_count: 12,
      missing: 0,
      mismatched: 0,
      request_mismatch: 0,
      extra: 0,
      repaired: 0,
      consistent: true,
    },
    ...overrides,
  };
}

describe('a write-error count comes with something to act on', () => {
  // The count is cumulative and `last_error` is not: internal/persistence/jobs.go
  // clears it on the next successful write. On the deployment this was found on,
  // nineteen errors sat under seventeen thousand writes with nothing said about
  // any of them, which is a number an operator can read and cannot use.
  it('prints the retained failure, which is the only one a busy ledger still has', () => {
    const markup = paint(
      <LedgerCard
        ledger={ledgerWith({
          write_errors: 19,
          last_error: '',
          last_write_error: 'insert build_jobs: deadlock detected',
          last_write_error_at: '2026-08-02T10:03:04Z',
        })}
      />,
    );
    expect(markup).toContain('insert build_jobs: deadlock detected');
  });

  it('says it once when both fields carry the same sentence', () => {
    const said = 'insert build_jobs: deadlock detected';
    const markup = paint(
      <LedgerCard ledger={ledgerWith({ write_errors: 19, last_error: said, last_write_error: said })} />,
    );
    expect(markup.split(said)).toHaveLength(2);
  });

  it('badges nineteen errors as degraded even where the server still says ok', () => {
    // internal/server/server.go computes `ok` from LastError, which the next
    // successful write clears, and treats a reconcile that never ran as fine —
    // so `ok` is true here. The card is what the reader sees; it must not be.
    const markup = paint(<LedgerCard ledger={ledgerWith({ write_errors: 19, ok: true })} />);
    expect(markup).toContain(messagesFor('en').t('mon.ledger.degraded'));
    expect(markup).toContain('data-verdict="red"');
  });
});

describe('a Go zero time is a state, not a date', () => {
  it('renders the translated "never" rather than year 1, in both languages', () => {
    for (const lang of ['en', 'zh'] as const) {
      const messages = messagesFor(lang);
      // The exact string the console this replaces printed in this slot, from
      // the exact value that produced it.
      expect(momentText(messages, GO_ZERO)).toBe(messages.t('common.never'));
      expect(momentText(messages, GO_ZERO)).not.toMatch(/1\/1\/1|0001/);
    }
  });

  it('answers the same for the other three ways a timestamp can be absent', () => {
    const messages = messagesFor('en');
    for (const absent of ['', null, undefined, 'not a date']) {
      expect(momentText(messages, absent)).toBe('never');
    }
  });

  it('does not print a plausible clock reading beside the write-error count', () => {
    const markup = paint(<LedgerCard ledger={ledgerWith({ write_errors: 19, ok: false })} />);
    expect(markup).toContain('never');
    // The literal a zero time.Time formats to on the console this replaces.
    expect(markup).not.toContain('1/1/1');
    expect(markup).not.toContain('0001');
  });

  it('still formats a real instant, so the guard is a guard and not a mute', () => {
    const messages = messagesFor('en');
    const printed = momentText(messages, '2026-08-01T12:34:56Z');
    expect(printed).not.toBe('never');
    // A date and a time to the second, which is what the ledger's "last checked"
    // is read to. Asserted by shape rather than by literal, because the value is
    // formatted in the runner's own zone.
    expect(printed).toMatch(/\d+\/\d+\/\d+/);
    expect(printed).toMatch(/\d+:\d+:\d+/);
  });
});

describe('a card verdict cannot contradict the numbers under it', () => {
  it('refuses to say "consistent" with nineteen write errors', () => {
    const markup = paint(<LedgerCard ledger={ledgerWith({ write_errors: 19 })} />);
    expect(markup).not.toContain(EN['mon.ledger.ok']);
    expect(markup).toContain(EN['mon.ledger.degraded']);
  });

  it('says "never reconciled" rather than "consistent" when nothing has reconciled', () => {
    const markup = paint(<LedgerCard ledger={ledgerWith()} />);
    expect(markup).toContain(EN['mon.ledger.unreconciled']);
    expect(markup).not.toContain(EN['mon.ledger.ok']);
  });

  it('says "consistent" once a reconcile has actually happened and nothing failed', () => {
    const markup = paint(
      <LedgerCard
        ledger={ledgerWith({
          last_reconcile: {
            ...ledgerWith().last_reconcile!,
            checked_at: '2026-08-01T12:00:00Z',
          },
        })}
      />,
    );
    expect(markup).toContain(EN['mon.ledger.ok']);
  });

  it('takes the worst of several verdicts, so a headline is never greener than a group', () => {
    const messages = messagesFor('en');
    expect(worstToken(messages, ['passed', 'passed'])).toBe('passed');
    expect(worstToken(messages, ['passed', 'pending'])).toBe('pending');
    expect(worstToken(messages, ['pending', 'blocked'])).toBe('blocked');
    expect(worstToken(messages, ['blocked', 'failed'])).toBe('failed');
    // An unknown token resolves gray and sorts between fine and wrong, which is
    // what an unknown state is — and it never outranks a real alarm.
    expect(worstToken(messages, ['passed', 'a_state_shipped_after_this_console'])).toBe(
      'a_state_shipped_after_this_console',
    );
    expect(worstToken(messages, ['failed', 'a_state_shipped_after_this_console'])).toBe('failed');
  });
});

describe('a conclusion is inked as a conclusion and a statistic is not', () => {
  it('deepens the write-error count and leaves the neutral counts alone', () => {
    const markup = paint(<LedgerCard ledger={ledgerWith({ write_errors: 19 })} />);
    // The count that answers "did any write fail?" carries the verdict binding.
    expect(markup).toContain('class="counter verdict" data-verdict="red"');
    // The counts that answer nothing keep the meta row's own ink.
    expect(markup).toContain('class="counter">12<');
  });

  it('inks the SLO verdict and not the cost figure beside it', () => {
    const target: TargetReliabilityStatus = {
      target_id: 't1',
      project_id: 'p1',
      project_name: 'Default',
      provider: 'pve',
      execution_zone: 'default',
      architecture: 'amd64',
      build_mode: 'native',
      profile_id: 'desktop',
      image_id: 'img',
      image_generation: 'gen3',
      resource_class: 'default',
      windows: [
        {
          name: '24h',
          hours: 24,
          samples: 7,
          successes: 1,
          failures: 6,
          canceled: 0,
          success_rate_percent: 14.3,
          slo_met: false,
          insufficient_data: false,
          queue_p50_seconds: 3,
          queue_p95_seconds: 9,
          run_p50_seconds: 120,
          run_p95_seconds: 400,
          reserved_cost_microunits: 4200000,
          charged_cost_microunits: 3900000,
        },
      ],
    };
    const markup = paint(<TargetCard target={target} />);
    // The measured defect: "SLO breach" rendered byte-identical to the cost.
    expect(markup).toContain('<span class="verdict" data-verdict="red">SLO breach</span>');
    // The cost line carries no verdict binding at all.
    const costLine = markup.slice(markup.indexOf('reserved/settled cost'));
    expect(costLine.slice(0, costLine.indexOf('</span>'))).not.toContain('verdict');
  });

  it('binds the ink through the status vocabulary, never through a call-site comparison', () => {
    const messages = messagesFor('en');
    expect(verdictAttributes(messages, 'passed')).toEqual({
      className: 'verdict',
      'data-verdict': 'green',
    });
    expect(verdictAttributes(messages, 'failed')).toEqual({
      className: 'verdict',
      'data-verdict': 'red',
    });
    // The catch-all, so a token shipped upstream is legible rather than
    // confidently coloured.
    expect(verdictAttributes(messages, 'a_state_shipped_after_this_console')).toEqual({
      className: 'verdict',
      'data-verdict': 'gray',
    });
  });
});

/* ---- contrast ------------------------------------------------------------ */

const STYLE_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'styles');

/** The two mode blocks of tokens.css, read as text so the values are the real ones. */
function tokenBlocks(): { light: string; dark: string } {
  const css = readFileSync(join(STYLE_ROOT, 'tokens.css'), 'utf8');
  // By the feature and not by one spelling of it: a contrast reading taken
  // against a block that was never found is a reading of the light palette
  // twice, and it passes.
  const split = css.search(DARK_BLOCK);
  expect(split, 'tokens.css has no dark block').toBeGreaterThan(0);
  return { light: css.slice(0, split), dark: css.slice(split) };
}

function token(block: string, name: string): string {
  const found = new RegExp(`${name}\\s*:\\s*([^;]+);`).exec(block);
  if (found === null) {
    throw new Error(`tokens.css declares no ${name}`);
  }
  return (found[1] ?? '').trim();
}

function channels(value: string): [number, number, number] {
  const short = /^#([0-9a-f])([0-9a-f])([0-9a-f])$/i.exec(value);
  if (short !== null) {
    return [1, 2, 3].map((index) => parseInt((short[index] ?? '0').repeat(2), 16)) as [
      number,
      number,
      number,
    ];
  }
  const long = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(value);
  if (long === null) {
    throw new Error(`not a hex colour: ${value}`);
  }
  return [1, 2, 3].map((index) => parseInt(long[index] ?? '0', 16)) as [number, number, number];
}

function luminance(rgb: readonly number[]): number {
  const linear = rgb.map((raw) => {
    const channel = raw / 255;
    return channel <= 0.03928 ? channel / 12.92 : Math.pow((channel + 0.055) / 1.055, 2.4);
  });
  return 0.2126 * (linear[0] ?? 0) + 0.7152 * (linear[1] ?? 0) + 0.0722 * (linear[2] ?? 0);
}

function contrast(fore: readonly number[], back: readonly number[]): number {
  const a = luminance(fore);
  const b = luminance(back);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

/** An alpha ink composited over a ground, which is what a tinted fill is. */
function over(fore: readonly number[], alpha: number, back: readonly number[]): number[] {
  return fore.map((channel, index) => alpha * channel + (1 - alpha) * (back[index] ?? 0));
}

describe('the verdict inks clear the contrast floor on the surface they land on', () => {
  const blocks = tokenBlocks();

  it('measures both inks against --pageRaised in both modes', () => {
    for (const [mode, block] of [
      ['light', blocks.light],
      ['dark', blocks.dark],
    ] as const) {
      const raised = channels(token(block, '--pageRaised'));
      for (const ink of ['--successInk', '--dangerInk'] as const) {
        const ratio = contrast(channels(token(block, ink)), raised);
        expect(
          ratio,
          `${ink} on --pageRaised in ${mode} is ${ratio.toFixed(2)}:1`,
        ).toBeGreaterThanOrEqual(4.5);
      }
    }
  });

  /**
   * The two compositions a previous attempt shipped, kept as a red line.
   *
   * A status ink on a tinted fill is a different measurement from the same ink
   * on the card, and both of these fail: --successInk over the 10% black wash
   * (.factory-step, .stage-log-card) and --dangerInk over the 13.3% red wash the
   * dark multiplier produces. Neither route puts a verdict on either, and this
   * is what says so rather than a comment nobody re-measures.
   */
  it('records that the tinted fills do not carry a status ink', () => {
    const lightQuaternary = over([0, 0, 0], 0.1, channels(token(blocks.light, '--pageRaised')));
    expect(contrast(channels(token(blocks.light, '--successInk')), lightQuaternary)).toBeLessThan(
      4.5,
    );

    const darkWash = over(
      channels(token(blocks.dark, '--systemRed')),
      0.1 * 1.33,
      channels(token(blocks.dark, '--pageRaised')),
    );
    expect(contrast(channels(token(blocks.dark, '--dangerInk')), darkWash)).toBeLessThan(4.5);
  });

  it('binds .verdict to those two tokens and to no third colour', () => {
    const components = readFileSync(join(STYLE_ROOT, 'components.css'), 'utf8');
    expect(components).toContain('.verdict[data-verdict="green"] { color: var(--successInk); }');
    expect(components).toContain('.verdict[data-verdict="red"]   { color: var(--dangerInk); }');
  });
});

/* ---- the scheduler card's three questions -------------------------------- */

function schedulerWith(overrides: Partial<MonitorSchedulerStatus> = {}): MonitorSchedulerStatus {
  return {
    authority: 'postgresql',
    queued_tasks: 0,
    running_tasks: 0,
    healthy: true,
    unschedulable_tasks: 0,
    active_leases: 0,
    expired_leases: 0,
    active_workers: 2,
    capability_workers: 2,
    stale_workers: 0,
    attempts_last_hour: 0,
    ...overrides,
  };
}

/**
 * The one group a heading heads, from its opening tag to its closing one.
 *
 * A heading and the badge answering it are a single fact — "is anything stuck?"
 * answered green — and a card-wide assertion cannot see that fact at all: the
 * three headings are all still printed and the badge hues are all still on the
 * card whichever group each verdict is bound to, so swapping two of them leaves
 * every count and every substring exactly where it was. Slicing to the section
 * is what makes the pair one assertion instead of two independent ones.
 */
function group(markup: string, headingKey: MessageKey): string {
  const sections = markup
    .split('<section class="ledger-group">')
    .slice(1)
    .map((section) => section.split('</section>')[0] ?? '');
  const found = sections.filter((section) => section.includes(EN[headingKey]));
  expect(found.length, `${headingKey} heads ${String(found.length)} groups, not one`).toBe(1);
  return found[0] ?? '';
}

/** The card above its groups: the headline and the badge that summarises them. */
function headline(markup: string): string {
  return markup.split('<section class="ledger-group">')[0] ?? '';
}

describe('the scheduler card answers three questions instead of listing forty numbers', () => {
  it('badges each of the three questions beside the question, not somewhere on the card', () => {
    // One reading in each group, and three different answers, so no two groups
    // can trade verdicts without one of these three slices changing hue: a queue
    // nothing is draining, two leases that expired under it, and capacity that
    // is not short of anything.
    const markup = paint(
      <SchedulerCard scheduler={schedulerWith({ queued_tasks: 40, expired_leases: 2 })} />,
    );
    expect(group(markup, 'mon.scheduler.group.flow')).toContain('class="status orange"');
    expect(group(markup, 'mon.scheduler.group.stuck')).toContain('class="status red"');
    expect(group(markup, 'mon.scheduler.group.budget')).toContain('class="status green"');
    // Three groups and one headline badge, and nothing else wearing the badge.
    expect(markup.match(/class="status /g)?.length).toBe(4);
  });

  it('turns the headline red when one group is red, and only then', () => {
    const healthy = paint(<SchedulerCard scheduler={schedulerWith({ running_tasks: 3 })} />);
    expect(healthy).not.toContain('class="status red"');

    const stuck = paint(
      <SchedulerCard scheduler={schedulerWith({ running_tasks: 3, expired_leases: 2 })} />,
    );
    // The group that is stuck, and the headline that inherits from it — asserted
    // where each of them is, so a red badge on the wrong group is not a pass.
    expect(group(stuck, 'mon.scheduler.group.stuck')).toContain('class="status red"');
    expect(headline(stuck)).toContain('class="status red"');
    expect(group(stuck, 'mon.scheduler.group.flow')).not.toContain('class="status red"');
    expect(group(stuck, 'mon.scheduler.group.budget')).not.toContain('class="status red"');
  });

  it('reads a queue that nothing is draining as blocked rather than as healthy', () => {
    const markup = paint(<SchedulerCard scheduler={schedulerWith({ queued_tasks: 40 })} />);
    expect(group(markup, 'mon.scheduler.group.flow')).toContain('class="status orange"');
  });

  it('folds the per-pool and per-decision rows instead of printing forty inline', () => {
    const markup = paint(
      <SchedulerCard
        scheduler={schedulerWith({
          worker_scoring: { decisions_last_hour: 12, multi_candidate_last_hour: 4 },
        })}
      />,
    );
    expect(markup).toContain(EN['mon.scheduler.group.detail']);
    expect(markup).toContain('<details');
  });

  it('omits the disclosure entirely when there is nothing behind it', () => {
    // A disclosure that opens onto nothing is a question a reader has to ask
    // twice, and an in-memory authority sends none of the rows behind this one.
    const markup = paint(<SchedulerCard scheduler={schedulerWith()} />);
    expect(markup).not.toContain('<details');
    expect(markup).not.toContain(EN['mon.scheduler.group.detail']);
  });

  it('marks a saturated slot pool with a suffix as well as a hue', () => {
    const markup = paint(
      <SchedulerCard
        scheduler={schedulerWith({
          autoscaler: {
            mode: 'on',
            recommendation: 'hold',
            active_slots: 4,
            busy_slots: 4,
            backlog: 0,
            unschedulable_backlog: 0,
            desired_slots: 4,
          },
        })}
      />,
    );
    // The level is on the element; monitor.css turns it into " !!" as well as
    // into a colour, so an operator who cannot tell orange from red still reads
    // which meter is the problem.
    expect(markup).toContain('data-level="crit"');
  });
});

/* ---- the gateway's issuer inventory -------------------------------------- */

function gatewayWith(overrides: Partial<WorkerGatewayStatus> = {}): WorkerGatewayStatus {
  return {
    enabled: true,
    authority: 'postgresql',
    listener_port: 8443,
    transport: 'mtls',
    registered_sessions: 2,
    connected_sessions: 2,
    pending_tasks: 0,
    pending_uploads: 0,
    active_issuers: 1,
    draining_issuers: 0,
    revoked_issuers: 0,
    active_certificates: 3,
    revoked_certificates: 0,
    expiring_certificates: 0,
    issuer_id: 'file-1',
    issuer_provider: 'file',
    issuer_healthy: true,
    issuer_runtime: {},
    inbound_builder_api: false,
    executor_protocol: 2,
    certificate_ttl_min: 20,
    phase_executor_mode: 'shadow',
    ...overrides,
  };
}

/**
 * One issuer generation, spelled the way internal/workergateway/cert.go writes
 * it — which is what every deployment that has ever issued a workload
 * certificate sends, and what this card had never been given.
 */
const ISSUED: WorkloadIdentityInventory = {
  issuers: [
    {
      fingerprint: 'a1b2c3d4e5f60718293a4b5c6d7e8f901234567890abcdef1234567890abcdef',
      issuer_id: 'file-1',
      provider: 'file',
      subject: 'CN=portage-engine worker issuer',
      serial: '0a1b2c',
      not_before: '2026-07-01T00:00:00Z',
      not_after: '2026-09-01T00:00:00Z',
      state: 'active',
      last_issued_at: '2026-08-01T09:00:00Z',
      active_certificates: 3,
    },
  ],
  certificates: [],
  certificate_limit: 100,
};

describe('the issuer inventory renders the payload the control plane sends', () => {
  it('paints a card at all for a deployment that has issued a certificate', () => {
    // The measured defect: `issuer.id` is a field the inventory has never
    // carried, and `undefined.slice(0, 12)` throws inside the render — which
    // takes /monitor with it, every card on it, not just this disclosure.
    const markup = paint(<GatewayCard gateway={gatewayWith()} identities={ISSUED} />);
    expect(markup).toContain('a1b2c3d4e5f6…');
    expect(markup).not.toContain('undefined');
  });

  it('says which issuer, in which state, holding how many live leaves', () => {
    const markup = paint(<GatewayCard gateway={gatewayWith()} identities={ISSUED} />);
    // Provider and issuer id together, as the card's own provider line spells
    // them, so a generation can be matched to the issuer it came from.
    expect(markup).toContain('file:file-1');
    // The count the console this replaces printed here and this port dropped:
    // it is what says whether revoking this generation ends anything.
    expect(markup).toContain('3 active');
    expect(markup).not.toContain(EN['mon.gateway.inventory.empty']);
  });

  it('still says nothing has been issued when nothing has', () => {
    const markup = paint(
      <GatewayCard
        gateway={gatewayWith({ active_certificates: 0 })}
        identities={{ issuers: [], certificates: [], certificate_limit: 100 }}
      />,
    );
    expect(markup).toContain(EN['mon.gateway.inventory.empty']);
  });
});

/* ---- degraded is a payload, not a transport failure ---------------------- */

describe('a 503 that carries the status document renders as a card', () => {
  it('recovers the ledger the reader came to read', () => {
    const body = JSON.stringify({ enabled: true, ok: false, write_errors: 19 });
    const recovered = tolerateDegraded<LedgerStatus>({
      kind: 'error',
      status: 503,
      message: body,
    });
    expect(recovered.kind).toBe('ok');
    expect(recovered.kind === 'ok' ? recovered.value.write_errors : null).toBe(19);
  });

  it('leaves a real failure alone, including a 503 that is not the envelope', () => {
    expect(
      tolerateDegraded<LedgerStatus>({ kind: 'error', status: 503, message: 'upstream timeout' })
        .kind,
    ).toBe('error');
    expect(
      tolerateDegraded<LedgerStatus>({ kind: 'error', status: 500, message: '{"ok":false}' }).kind,
    ).toBe('error');
    expect(tolerateDegraded<LedgerStatus>({ kind: 'unauthorized' }).kind).toBe('unauthorized');
  });
});

/* ---- polling ------------------------------------------------------------- */

describe('polling is gated on visibility', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('polls at the interval the console this replaces used', () => {
    expect(POLL_INTERVAL_MS).toBe(15000);
  });

  it('never ticks in a backgrounded tab', () => {
    vi.useFakeTimers();
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
    const tick = vi.fn();
    const stop = pollWhenVisible(tick, POLL_INTERVAL_MS);
    vi.advanceTimersByTime(POLL_INTERVAL_MS * 20);
    // 15000ms x 20 is five minutes; the console this replaces made 32 round
    // trips a minute here whether or not anyone was reading them.
    expect(tick).not.toHaveBeenCalled();
    stop();
  });

  it('ticks while the tab is visible, and stops the moment it is not', () => {
    vi.useFakeTimers();
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const tick = vi.fn();
    const stop = pollWhenVisible(tick, POLL_INTERVAL_MS);
    vi.advanceTimersByTime(POLL_INTERVAL_MS * 3);
    expect(tick).toHaveBeenCalledTimes(3);

    hidden.mockReturnValue(true);
    document.dispatchEvent(new Event('visibilitychange'));
    vi.advanceTimersByTime(POLL_INTERVAL_MS * 10);
    expect(tick).toHaveBeenCalledTimes(3);

    // And the first tick on the way back is immediate, so returning to the tab
    // never shows numbers a whole interval out of date.
    hidden.mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));
    expect(tick).toHaveBeenCalledTimes(4);
    stop();
  });
});

/* ---- end to end, from the wire to the attribute --------------------------- */

describe('the whole route, from a degraded 503 to what the card actually says', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('renders the ledger a reader came for instead of an error box full of JSON', async () => {
    (globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT =
      true;
    // The three health endpoints answer 503 with the document the card is
    // about; everything else is empty and 200, which is a real deployment with
    // no builders and no cloud instances.
    const degraded = ledgerWith({ ok: false, write_errors: 19 });
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string) =>
        Promise.resolve(
          input === '/api/ledger/status'
            ? new Response(JSON.stringify(degraded), {
                status: 503,
                headers: { 'Content-Type': 'application/json' },
              })
            : new Response(JSON.stringify(input === '/api/instances' ? [] : {}), {
                status: 200,
                headers: { 'Content-Type': 'application/json' },
              }),
        ),
      ),
    );

    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
      root.render(
        <MessagesProvider lang="en">
          {/* The chrome settles which project every request is made in, and a
              page waits for it: mounted without one, nothing here would ask the
              control plane anything. */}
          <ProjectScopeContext value={{ projectID: 'p1', ready: true }}>
            <MemoryRouter>
              <MonitorPage />
            </MemoryRouter>
          </ProjectScopeContext>
        </MessagesProvider>,
      );
      await Promise.resolve();
    });

    const text = container.textContent ?? '';
    // The two numbers a reader consults this card for, both present and both
    // saying what they mean.
    expect(text).toContain('19');
    expect(text).toContain('never');
    expect(text).toContain(EN['mon.ledger.degraded']);
    expect(text).not.toContain(EN['mon.ledger.ok']);
    // Not the error box, and not the raw document that box would have shown.
    expect(container.querySelector('[data-state="error"]')).toBeNull();
    expect(text).not.toContain('write_errors');
    // The count carries the verdict binding; the ledger-row count beside it
    // does not, because it concludes nothing.
    const verdicts = [...container.querySelectorAll('.counter.verdict')];
    expect(verdicts.some((node) => node.getAttribute('data-verdict') === 'red')).toBe(true);

    await act(async () => {
      root.unmount();
      await Promise.resolve();
    });
    container.remove();
  });
});
