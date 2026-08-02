import { Fragment, useEffect, useState } from 'react';

import type { ApiOutcome } from '../api/client';
import { api } from '../api/endpoints';
import type { ProjectPolicy } from '../api/types';
import { useMessages } from '../i18n/context';
import type { Messages } from '../i18n/messages';
import { meterLevel } from '../i18n/status';
import { useProjectScope } from './project';

/**
 * The project's own limits, in the rail, on every authenticated page.
 *
 * Ported from loadProjectPolicySummary in internal/dashboard/ui.go. It answers
 * the question an operator arrives with the moment a submit is refused — is this
 * project suspended, and which ceiling did we hit — and without it the answer
 * lives only in the rejection sentence of the request that was refused.
 *
 * The old rail's own defect is not ported: twenty facts joined by a separator
 * cannot be read in a 240px column. The four limits that actually stop a build
 * stay resident as meters; the rest sits behind a disclosure and promotes itself
 * back out the moment it crosses its warn threshold, so a metric at 95% cannot
 * hide behind a summary. The block's height is reserved in components.css for
 * exactly those four, so the rail does not move when the payload lands.
 *
 * Every reading goes through the message layer's formatters, which are bound to
 * the app locale — the old rail concatenated raw integers and a `toFixed(3)`,
 * so a Chinese reader got ungrouped ASCII in a rail whose every other number was
 * formatted.
 */

/**
 * A number the payload may not carry.
 *
 * Every reading below is declared as a plain integer, and the policy endpoint
 * has answered without some of them — the phase counters only arrive once the
 * phase executor is out of shadow. The console this replaces put
 * "cundefined/undefined" in the rail when that happened, which a reader takes
 * for a value rather than for an absent field, so a missing one is read as zero
 * here exactly as it was there.
 */
function reading(value: number): number {
  return Number.isFinite(value) ? value : 0;
}

interface Meter {
  key: string;
  label: string;
  used: number;
  max: number;
  /** How to spell both numbers. Bare counts go through the locale formatter. */
  format: (value: number) => string;
  /** Trails the reading: the unit that is not part of the number itself. */
  unit?: string;
  /** Always on screen, whatever its level. The rest are folded until they warn. */
  resident?: boolean;
}

/**
 * The meters, in the order the console this replaces put them.
 *
 * The four resident ones are the four that refuse a submission outright. The
 * five below them are reservations against a pool: they matter when they are
 * near their ceiling and are noise when they are not, which is exactly what the
 * disclosure is for.
 */
function meters(policy: ProjectPolicy, messages: Messages): readonly Meter[] {
  const count = (value: number): string => messages.number(value);
  // 131072 MiB is a number an operator has to divide before it means anything.
  // The reserves arrive in MiB and GiB rather than bytes, so both go through the
  // one byte formatter and the rail scales its unit the way the rest of the
  // console already does.
  const mib = (value: number): string => messages.bytes(value * 1024 * 1024);
  const gib = (value: number): string => messages.bytes(value * 1024 * 1024 * 1024);
  // Cloud spend is metered in millionths. "cloud 420000 / 1000000" names a unit
  // nobody operates in; the target cards already publish this same figure
  // divided down and call it a cost, and this is that figure.
  const cost = (value: number): string =>
    messages.number(value / 1000000, { minimumFractionDigits: 3, maximumFractionDigits: 3 });
  return [
    {
      key: 'queued',
      label: messages.t('quota.queued'),
      used: reading(policy.queued_jobs),
      max: reading(policy.max_queued_jobs),
      format: count,
      resident: true,
    },
    {
      key: 'active',
      label: messages.t('quota.active'),
      used: reading(policy.active_jobs),
      max: reading(policy.max_active_jobs),
      format: count,
      resident: true,
    },
    {
      key: 'build',
      label: messages.t('quota.build'),
      // Whole minutes, rounded up, because a build that has run for a second
      // has used a minute of the ceiling as far as the reader is concerned.
      used: Math.ceil(reading(policy.build_seconds_today) / 60),
      max: Math.ceil(reading(policy.max_daily_build_seconds) / 60),
      format: count,
      unit: messages.t('quota.minutes'),
      resident: true,
    },
    {
      key: 'today',
      label: messages.t('quota.today'),
      used: reading(policy.submissions_today),
      max: reading(policy.max_daily_submissions),
      format: count,
      resident: true,
    },
    {
      key: 'cpu',
      label: messages.t('quota.cpu'),
      used: reading(policy.reserved_vcpus),
      max: reading(policy.max_active_vcpus),
      format: count,
    },
    {
      key: 'ram',
      label: messages.t('quota.ram'),
      used: reading(policy.reserved_memory_mib),
      max: reading(policy.max_active_memory_mib),
      format: mib,
    },
    {
      key: 'disk',
      label: messages.t('quota.disk'),
      used: reading(policy.reserved_disk_gib),
      max: reading(policy.max_active_disk_gib),
      format: gib,
    },
    {
      key: 'cloud',
      label: messages.t('quota.cloud'),
      used: reading(policy.cloud_cost_microunits_today),
      max: reading(policy.max_daily_cloud_cost_microunits),
      format: cost,
    },
    {
      key: 'failures',
      label: messages.t('quota.failures'),
      used: reading(policy.failures_last_hour),
      max: reading(policy.max_failures_per_hour),
      format: count,
    },
  ];
}

/**
 * The rows behind the disclosure: everything that is a fact rather than a limit.
 *
 * The per-phase row reads the five reservation counters the policy endpoint
 * actually carries. The console this replaces read `phase_collect_active` and
 * `max_phase_collect` and four pairs like them — field names `ProjectPolicy` in
 * internal/persistence/iam.go has never had — so the row it drew was "c0/0 p0/0
 * b0/0 v0/0 pub0/0" on every deployment, which reads as five phases sitting at
 * an exhausted ceiling. These are the same phases, counted by the numbers that
 * exist; the maxima do not exist and are not invented.
 */
function details(
  policy: ProjectPolicy,
  messages: Messages,
): readonly (readonly [string, string])[] {
  const count = (value: number): string => messages.number(value);
  return [
    [messages.t('quota.weight'), count(reading(policy.priority_weight))],
    [messages.t('quota.starvation'), `${count(reading(policy.starvation_threshold_seconds))}s`],
    [
      messages.t('quota.quarantine'),
      `${messages.bytes(reading(policy.quarantine_bytes))} (${count(reading(policy.active_artifact_budgets))} ${messages.t('quota.jobs')})`,
    ],
    [
      messages.t('quota.phases'),
      `c${count(reading(policy.claimed_reservations))} p${count(reading(policy.provision_reservations))}` +
        ` b${count(reading(policy.build_reservations))} v${count(reading(policy.verify_reservations))}` +
        ` pub${count(reading(policy.publish_reservations))}`,
    ],
    [
      messages.t('quota.plans'),
      `${count(reading(policy.phase_work_active))}${messages.t('quota.shadow')}${count(reading(policy.phase_work_shadow))}`,
    ],
    [
      messages.t('quota.work'),
      count(reading(policy.phase_work_ready)) +
        messages.t('quota.unschedulable') +
        count(reading(policy.phase_work_unschedulable)) +
        messages.t('quota.claimed') +
        count(reading(policy.phase_work_claimed)) +
        messages.t('quota.blocked') +
        count(reading(policy.phase_work_blocked)),
    ],
  ];
}

/**
 * How much of the track is filled.
 *
 * A reservation that exists reads as a bar that exists: rounding alone sent
 * everything under half a percent to 0%, so one vCPU against a 512-vCPU ceiling
 * drew the same empty track as no reservation at all.
 */
function fillWidth(used: number, max: number): string {
  if (used <= 0) {
    return '0';
  }
  const ratio = max > 0 ? used / max : 0;
  return `${String(Math.max(2, Math.min(100, Math.round(ratio * 100))))}%`;
}

export function ProjectQuota() {
  const messages = useMessages();
  const { projectID, ready } = useProjectScope();
  /**
   * The answer, and which project it is about.
   *
   * Kept together rather than reset when the selection changes: a reading held
   * over from the previous project while the new one's request is in flight
   * attributes one project's ceiling to another, and that is exactly the claim
   * an operator is here to check. Naming the project the answer belongs to makes
   * a stale one unrenderable instead of merely unlikely.
   */
  const [answer, setAnswer] = useState<{
    projectID: string;
    outcome: ApiOutcome<ProjectPolicy>;
  } | null>(null);

  useEffect(() => {
    if (!ready || projectID === '') {
      return undefined;
    }
    const controller = new AbortController();
    void api.projectPolicy({ projectID, signal: controller.signal }).then((next) => {
      if (!controller.signal.aborted) {
        setAnswer({ projectID, outcome: next });
      }
    });
    return () => {
      controller.abort();
    };
  }, [projectID, ready]);

  const outcome = answer !== null && answer.projectID === projectID ? answer.outcome : null;

  if (ready && projectID === '') {
    // No project to ask about, which is what an IAM refusal and an identity
    // holding no membership both produce. The console this replaces left the
    // loading sentence up forever here, which claims a request is still coming.
    return <div className="policy-summary">{messages.t('quota.unavailable')}</div>;
  }
  if (outcome === null) {
    return <div className="policy-summary">{messages.t('quota.loading')}</div>;
  }
  if (outcome.kind !== 'ok') {
    return <div className="policy-summary">{messages.t('quota.unavailable')}</div>;
  }

  const policy = outcome.value;
  const suspended = policy.suspended || policy.abuse_suspended;
  const rows = meters(policy, messages);
  // A folded metric that has climbed past its warn threshold promotes itself, so
  // nothing that is about to stop a build can hide behind the summary.
  const resident = (meter: Meter): boolean =>
    meter.resident === true || meterLevel(meter.used, meter.max) !== 'ok';
  const shown = rows.filter((meter) => resident(meter));
  const folded = rows.filter((meter) => !resident(meter));

  return (
    <div className="policy-summary" data-state={suspended ? 'suspended' : 'ok'}>
      {suspended ? (
        <p className="quota-state">
          {policy.suspended ? messages.t('quota.suspended') : messages.t('quota.abuse')}
        </p>
      ) : null}
      {shown.map((meter) => {
        // Level is a value and not a colour: the stylesheet spends two channels
        // on it, the bar's hue and a " !" / " !!" after the reading, so an
        // operator who cannot tell orange from red still sees which meters are
        // the problem.
        const level = meterLevel(meter.used, meter.max);
        return (
          <div className="quota-meter" data-level={level} key={meter.key}>
            <span className="quota-k">{meter.label.trim()}</span>
            <span className="quota-v">
              {`${meter.format(meter.used)} / ${meter.format(meter.max)}${meter.unit ?? ''}`}
            </span>
            <span className="quota-bar">
              <i style={{ width: fillWidth(meter.used, meter.max) }} />
            </span>
          </div>
        );
      })}
      <details className="quota-more">
        <summary>{messages.t('quota.more')}</summary>
        <dl className="quota-dl">
          {/* A fragment, so each pair is two children of the list and not one
              wrapper: the two-column grid is on the <dl>, and a box around each
              pair would make every pair a single cell in it. */}
          {folded
            .map((meter): readonly [string, string] => [
              meter.label,
              `${meter.format(meter.used)} / ${meter.format(meter.max)}`,
            ])
            .concat(details(policy, messages))
            .map(([term, value]) => (
              <Fragment key={term}>
                <dt>{term.trim()}</dt>
                <dd>{value.trim()}</dd>
              </Fragment>
            ))}
        </dl>
      </details>
    </div>
  );
}
