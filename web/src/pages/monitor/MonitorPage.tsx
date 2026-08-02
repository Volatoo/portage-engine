import { useEffect, useRef, useState } from 'react';
import { Link } from 'react-router';

import type { ApiOutcome } from '../../api/client';
import { api } from '../../api/endpoints';
import type { Scope } from '../../api/endpoints';
import { resolveState } from '../../api/state';
import { pollWhenVisible } from '../../api/stream';
import type {
  BuildersStatus,
  CacheStatus,
  Instance,
  LedgerStatus,
  WorkerGatewayStatus,
  WorkloadIdentityInventory,
} from '../../api/types';
import { busyAttributes, singleWriter } from '../../api/write';
import { useProjectScope } from '../../app/project';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import { useMessages } from '../../i18n/context';
import type { MessageKey, Messages } from '../../i18n/messages';
import { meterLevel, resolveStatus } from '../../i18n/status';
import '../../styles/pages/monitor.css';
import { Block, MetaCount, MetaVerdict, Moment, StatusBadge } from './parts';
import { cardState, happened, momentText, verdictAttributes, worstToken } from './verdicts';
import { tolerateDegraded } from './health';
import type {
  MonitorSchedulerStatus,
  RuntimeMetadataEnvelope,
  SchedulerCapacityPoolStatus,
  TargetReliabilityStatus,
} from './wire';

/**
 * /monitor — the operator's answer to "can I trust what this control plane is
 * telling me".
 *
 * Ported from monitorContent/monitorJS in internal/dashboard/ui.go: the same six
 * rollups, in the same order, reading the same fields. What changed is that the
 * page no longer states a fact and a conclusion in the same ink, and that a
 * timestamp is guarded in one place instead of at thirty call sites.
 */

/**
 * Fifteen seconds, unchanged from the console this replaces, and gated on
 * visibility.
 *
 * The interval is not the interesting number — the gate is. A backgrounded tab
 * kept every one of these polls running: /monitor alone made 32 round trips a
 * minute, about 15000 of them unread over an eight-hour day. `pollWhenVisible`
 * stops the timer on `document.hidden` and ticks once immediately on the way
 * back, so returning to the tab never shows numbers a whole interval stale.
 */
export const POLL_INTERVAL_MS = 15000;

/** Every rollup the page reads, and `null` for one whose first request is out. */
interface Rollups {
  ledger: ApiOutcome<LedgerStatus> | null;
  scheduler: ApiOutcome<MonitorSchedulerStatus> | null;
  gateway: ApiOutcome<WorkerGatewayStatus> | null;
  identities: ApiOutcome<WorkloadIdentityInventory> | null;
  metadata: ApiOutcome<RuntimeMetadataEnvelope> | null;
  cache: ApiOutcome<CacheStatus> | null;
  builders: ApiOutcome<BuildersStatus> | null;
  instances: ApiOutcome<Instance[]> | null;
}

const NOTHING_ASKED_YET: Rollups = {
  ledger: null,
  scheduler: null,
  gateway: null,
  identities: null,
  metadata: null,
  cache: null,
  builders: null,
  instances: null,
};

/**
 * One poll, eight requests, one paint.
 *
 * Deliberately gathered rather than applied as each lands: eight independent
 * state updates are eight layout passes, and eight chances for a heading to walk
 * down the page under a reader who is mid-sentence. The measured property this
 * port has to keep is one scrollHeight across a poll, and a single commit is
 * what makes the poll a swap of text inside a settled layout.
 */
async function loadRollups(scope: Scope): Promise<Rollups> {
  const [ledger, scheduler, gateway, identities, metadata, cache, builders, instances] =
    await Promise.all([
      api.ledgerStatus(scope),
      api.schedulerStatus(scope),
      api.workerGatewayStatus(scope),
      api.workerGatewayIdentities(scope),
      api.runtimeMetadataStatus(scope),
      api.cacheStatus(scope),
      api.buildersStatus(scope),
      api.instances(scope),
    ]);
  return {
    // The three that answer 503 with the document the card is about.
    ledger: tolerateDegraded(ledger),
    cache: tolerateDegraded(cache),
    // The metadata document is the `status` inside the envelope; `enabled` and
    // `ok` are the envelope's own, and both of its failure branches carry no
    // document at all — which is the distinction between "the ledger could not
    // be asked" and "the ledger answered and six artifacts are missing".
    metadata: tolerateDegraded(metadata),
    // The scheduler's durable half is thirty fields; see src/api/types.ts for
    // the transcription and why every one of them is optional.
    scheduler: scheduler as unknown as ApiOutcome<MonitorSchedulerStatus>,
    gateway,
    identities,
    builders,
    instances,
  };
}

/* ---- the job ledger ------------------------------------------------------ */

/**
 * What this card calls its own three states.
 *
 * A lookup with a catch-all, not a chain: the colour still comes from
 * STATUS_VOCABULARY and only the word is the card's, so a token this table has
 * not heard of falls to "degraded" — which is the safe direction for a ledger,
 * and the one that cannot silently read as "consistent".
 */
const LEDGER_WORD: Record<string, MessageKey> = {
  passed: 'mon.ledger.ok',
  pending: 'mon.ledger.unreconciled',
  failed: 'mon.ledger.degraded',
};

export function LedgerCard({ ledger }: { ledger: LedgerStatus }) {
  const messages = useMessages();
  const reconcile = ledger.last_reconcile;
  const writeErrors = ledger.write_errors ?? 0;
  // checked_at is a bare time.Time on the wire, so a reconciler that has never
  // run still sends a full timestamp. Resolving it to an instant up front is
  // what turns "never" from a date into a state the verdict below can read.
  const reconciled = happened(messages, reconcile?.checked_at);

  // One judgement, read by the badge and by every number under it. The card
  // used to badge on `ledger.ok` alone, so "consistent" stood two lines above a
  // non-zero write-error count and beside a reconcile that had never happened —
  // and both of those are what a reader consults this card to decide.
  const writeToken = writeErrors > 0 ? 'failed' : 'passed';
  const reconcileToken = reconciled ? 'passed' : 'pending';
  const cardToken =
    ledger.ok === false ? 'failed' : worstToken(messages, [writeToken, reconcileToken]);

  return (
    <article className="builder-card">
      <h3>{messages.t('mon.ledger.shadow')}</h3>
      <StatusBadge
        token={cardToken}
        label={messages.t(LEDGER_WORD[cardToken] ?? 'mon.ledger.degraded')}
      />
      <div className="meta">
        <MetaCount labelKey="mon.ledger.legacy" value={reconcile?.legacy_count ?? 0} />
        <MetaCount labelKey="mon.ledger.rows" value={reconcile?.ledger_count ?? 0} />
        <MetaCount labelKey="mon.ledger.repaired" value={reconcile?.repaired ?? 0} />
        <MetaCount labelKey="mon.ledger.errors" value={writeErrors} token={writeToken} />
        {/* Always rendered, because a reconcile that has never run is the fact
            this line exists to report; it used to be suppressed by a truthiness
            test the zero timestamp passed, and then printed by a formatter that
            read the zero as the year 1. Both halves resolve through `Moment`. */}
        <span>
          {messages.t('mon.ledger.checked')}
          <span {...verdictAttributes(messages, reconcileToken)}>
            <Moment value={reconcile?.checked_at} />
          </span>
        </span>
        {ledger.last_error === undefined || ledger.last_error === '' ? null : (
          <span>{ledger.last_error}</span>
        )}
      </div>
    </article>
  );
}

/* ---- the durable scheduler ----------------------------------------------- */

/**
 * The three questions the scheduler card is read to answer.
 *
 * The card was forty statistics in one wrapped row — dispatch counters beside
 * lease-expiry recovery counters beside per-pool capacity readings — which reads
 * as developer output because nothing in it says which numbers answer which
 * question. Grouping is the whole change: an operator wants to know whether work
 * is flowing, whether anything is stuck, and whether anything is over budget,
 * and each group carries its own verdict from the same vocabulary the card's
 * headline is taken from. The headline is the worst of the three, so it cannot
 * be greener than something underneath it.
 */
interface SchedulerVerdicts {
  flow: string;
  stuck: string;
  budget: string;
  card: string;
}

function schedulerVerdicts(
  messages: Messages,
  scheduler: MonitorSchedulerStatus,
): SchedulerVerdicts {
  const running = scheduler.running_tasks ?? 0;
  const queued = scheduler.queued_tasks ?? 0;
  const attempts = scheduler.attempts_last_hour ?? 0;
  // Flowing when something is actually moving; blocked when there is work and
  // nothing has moved in an hour, which is the shape of a stalled dispatcher;
  // pending when there is simply nothing to do, which is not a fault.
  const flow = running > 0 || attempts > 0 ? 'healthy' : queued > 0 ? 'blocked' : 'pending';

  const projection = scheduler.target_history?.projection;
  const stuckCount =
    (scheduler.unschedulable_tasks ?? 0) +
    (scheduler.expired_leases ?? 0) +
    (scheduler.stale_workers ?? 0);
  const projectionBad = projection !== undefined && projection.valid === false;
  const starved = scheduler.fairness?.starved_projects ?? 0;
  const stuck =
    stuckCount > 0 || projectionBad || scheduler.healthy === false
      ? 'degraded'
      : starved > 0
        ? 'blocked'
        : 'healthy';

  const autoscaler = scheduler.autoscaler;
  const actuator = autoscaler?.actuator;
  const pools = autoscaler?.pools ?? [];
  const blockedPools = pools.filter((pool) => pool.unschedulable_backlog > 0).length;
  const budget =
    (actuator?.failed_actions ?? 0) > 0 ||
    blockedPools > 0 ||
    (autoscaler?.unschedulable_backlog ?? 0) > 0
      ? 'degraded'
      : (autoscaler?.desired_slots ?? 0) > (autoscaler?.active_slots ?? 0)
        ? 'blocked'
        : 'healthy';

  return { flow, stuck, budget, card: worstToken(messages, [flow, stuck, budget]) };
}

/**
 * A slot utilisation meter.
 *
 * `meterLevel` is the shared decision about where warn starts, so this cannot
 * disagree with the quota meters in the rail, and the level drives a suffix as
 * well as a hue: " !" and " !!" are what an operator who cannot tell orange from
 * red reads instead.
 */
function SlotMeter({ busy, active }: { busy: number; active: number }) {
  const messages = useMessages();
  const level = meterLevel(busy, active);
  const width = active > 0 ? Math.min(100, Math.round((busy / active) * 100)) : 0;
  return (
    <div className="slot-meter" data-level={level}>
      <span className="slot-k">{messages.t('mon.scheduler.utilisation')}</span>
      <span className="slot-v">
        {messages.number(busy)} / {messages.number(active)}
      </span>
      <span className="slot-bar">
        <i style={{ width: `${String(width)}%` }} />
      </span>
    </div>
  );
}

function SchedulerGroup({
  headingKey,
  token,
  children,
}: {
  headingKey: MessageKey;
  token: string;
  children: React.ReactNode;
}) {
  const messages = useMessages();
  return (
    <section className="ledger-group">
      <div className="group-head">
        <h4>{messages.t(headingKey)}</h4>
        <StatusBadge token={token} />
      </div>
      <div className="meta">{children}</div>
    </section>
  );
}

export function SchedulerCard({ scheduler }: { scheduler: MonitorSchedulerStatus }) {
  const messages = useMessages();
  const verdicts = schedulerVerdicts(messages, scheduler);
  const projection = scheduler.target_history?.projection;
  const expiries = scheduler.lease_expiries;
  const fairness = scheduler.fairness;
  const scoring = scheduler.worker_scoring;
  const autoscaler = scheduler.autoscaler;
  const actuator = autoscaler?.actuator;
  const pools = autoscaler?.pools ?? [];
  const blockedPools = pools.filter((pool) => pool.unschedulable_backlog > 0).length;
  const hasDetail =
    scoring !== undefined ||
    pools.length > 0 ||
    (actuator?.actions ?? []).length > 0 ||
    (actuator?.instances ?? []).length > 0;

  // Spelled by the server as free prose except for the one sentence the shadow
  // mode always sends, which is in the catalogue so a Chinese reader is not the
  // only one who gets it in English.
  const autoscaleReason =
    autoscaler?.reason === 'phase executor mode is shadow; capacity inventory only'
      ? messages.t('mon.scheduler.autoscale.shadow')
      : autoscaler?.reason;

  return (
    <article className="builder-card">
      <h3>{messages.t('mon.scheduler.pg')}</h3>
      <StatusBadge token={verdicts.card} />
      <div className="ledger-groups">
        <SchedulerGroup headingKey="mon.scheduler.group.flow" token={verdicts.flow}>
          <MetaCount labelKey="mon.scheduler.queue" value={scheduler.queued_tasks ?? 0} />
          <MetaCount labelKey="mon.scheduler.running" value={scheduler.running_tasks ?? 0} />
          <MetaCount labelKey="mon.scheduler.leases" value={scheduler.active_leases ?? 0} />
          <MetaCount labelKey="mon.scheduler.attempts" value={scheduler.attempts_last_hour ?? 0} />
          <MetaCount labelKey="mon.scheduler.workers" value={scheduler.active_workers ?? 0} />
          <MetaCount
            labelKey="mon.scheduler.capability"
            value={scheduler.capability_workers ?? 0}
          />
          {fairness === undefined ? null : (
            <>
              <MetaCount
                labelKey="mon.scheduler.fair.projects"
                value={fairness.eligible_projects}
              />
              <span>
                {messages.t('mon.scheduler.fair.dispatches')}
                {messages.number(fairness.admission_dispatches)}/
                {messages.number(fairness.phase_dispatches)}
              </span>
            </>
          )}
        </SchedulerGroup>

        <SchedulerGroup headingKey="mon.scheduler.group.stuck" token={verdicts.stuck}>
          <MetaCount
            labelKey="mon.scheduler.unschedulable"
            value={scheduler.unschedulable_tasks ?? 0}
            token={(scheduler.unschedulable_tasks ?? 0) > 0 ? 'failed' : 'passed'}
          />
          <MetaCount
            labelKey="mon.scheduler.expired"
            value={scheduler.expired_leases ?? 0}
            token={(scheduler.expired_leases ?? 0) > 0 ? 'failed' : 'passed'}
          />
          <MetaCount
            labelKey="mon.scheduler.stale"
            value={scheduler.stale_workers ?? 0}
            token={(scheduler.stale_workers ?? 0) > 0 ? 'failed' : 'passed'}
          />
          {fairness === undefined ? null : (
            <>
              <MetaCount
                labelKey="mon.scheduler.fair.starved"
                value={fairness.starved_projects}
                token={fairness.starved_projects > 0 ? 'blocked' : 'passed'}
              />
              <span>
                {messages.t('mon.scheduler.fair.maxwait')}
                {messages.number(fairness.max_queue_wait_seconds)}s
              </span>
            </>
          )}
          {expiries === undefined ? null : (
            <span>
              {messages.t('mon.scheduler.lease.expiry')}
              {messages.number(expiries.attempt_requeued)}/
              {messages.number(expiries.attempt_failed)}/
              {messages.number(expiries.attempt_canceled)} ·{' '}
              {messages.number(expiries.admission_requeued)}/
              {messages.number(expiries.admission_failed)}/
              {messages.number(expiries.admission_canceled)} ·{' '}
              {messages.number(expiries.phase_reclaimed)}
            </span>
          )}
          {projection === undefined ? null : (
            <>
              {/* The projection's own state word is a conclusion about whether
                  the snapshot on screen can be believed, so it takes the status
                  ink rather than the meta row's default. */}
              <MetaVerdict
                labelKey="mon.targets.projection"
                token={projection.valid === false ? 'failed' : 'passed'}
                reading={projectionStateWord(messages, projection.state)}
              />
              <span>
                {messages.t('mon.targets.projection.lag')}
                {messages.number(projection.lag_seconds)}/
                {messages.number(projection.alert_threshold_seconds)}s
              </span>
              <span>
                {messages.t('mon.targets.projection.source')}
                <Moment value={projection.source_watermark_at} />
              </span>
              <span>
                {messages.t('mon.targets.projection.snapshot')}
                <Moment value={projection.projected_watermark_at} />
              </span>
            </>
          )}
          {scheduler.error === undefined || scheduler.error === '' ? null : (
            <span>{scheduler.error}</span>
          )}
        </SchedulerGroup>

        <SchedulerGroup headingKey="mon.scheduler.group.budget" token={verdicts.budget}>
          <span>
            {messages.t('mon.scheduler.autoscale')}
            {autoscaler?.mode ?? 'off'}:{autoscaler?.recommendation ?? 'off'}
          </span>
          <span>
            {messages.t('mon.scheduler.autoscale.slots')}
            {messages.number(autoscaler?.active_slots ?? 0)}/
            {messages.number(autoscaler?.desired_slots ?? 0)}
          </span>
          <span>
            {messages.t('mon.scheduler.autoscale.demand')}
            {messages.number(autoscaler?.busy_slots ?? 0)}/
            {messages.number(autoscaler?.backlog ?? 0)}/
            {messages.number(autoscaler?.unschedulable_backlog ?? 0)}
          </span>
          <span>
            {messages.t('mon.scheduler.autoscale.pools')}
            {messages.number(pools.length)}/
            <span {...verdictAttributes(messages, blockedPools > 0 ? 'failed' : 'passed')}>
              {messages.number(blockedPools)}
            </span>
          </span>
          <span>
            {messages.t('mon.scheduler.actuator')}
            {messages.number(actuator?.open_actions ?? 0)}/
            <span
              {...verdictAttributes(
                messages,
                (actuator?.failed_actions ?? 0) > 0 ? 'failed' : 'passed',
              )}
            >
              {messages.number(actuator?.failed_actions ?? 0)}
            </span>
          </span>
          <span>
            {messages.t('mon.scheduler.instances')}
            {messages.number(actuator?.provisioning_instances ?? 0)}/
            {messages.number(actuator?.active_instances ?? 0)}/
            {messages.number(actuator?.draining_instances ?? 0)}/
            {messages.number(actuator?.deleting_instances ?? 0)}
          </span>
          {autoscaleReason === undefined || autoscaleReason === '' ? null : (
            <span>{autoscaleReason}</span>
          )}
        </SchedulerGroup>
      </div>

      <SlotMeter busy={autoscaler?.busy_slots ?? 0} active={autoscaler?.active_slots ?? 0} />

      {/* The per-pool, per-decision and per-instance rows are evidence for the
          three verdicts above rather than the verdicts themselves, and there can
          be dozens of them. They fold, so the card answers its three questions
          in one screen and still holds everything the console this replaces
          printed inline — and the control is absent rather than empty when a
          deployment has produced none of them, because a disclosure that opens
          onto nothing is a question a reader has to ask twice. */}
      {hasDetail ? (
        <details className="factory-step-details">
          <summary>{messages.t('mon.scheduler.group.detail')}</summary>
          <div className="meta">
            {scoring === undefined ? null : (
              <span>
                {messages.t('mon.scheduler.score.decisions')}
                {messages.number(scoring.decisions_last_hour)}/
                {messages.number(scoring.multi_candidate_last_hour)}
              </span>
            )}
            {(scoring?.recent ?? []).slice(0, 6).map((decision, index) => (
              <span key={`${decision.worker}-${String(index)}`}>
                {messages.t('mon.scheduler.score.worker')}
                {decision.work_kind}
                {decision.phase === undefined || decision.phase === ''
                  ? ''
                  : `/${decision.phase}`}{' '}
                · {decision.worker} · {messages.number(decision.candidate_count)} ·{' '}
                {messages.number(decision.pressure_score)}/
                {messages.number(decision.recent_failures)}
              </span>
            ))}
            {pools.map((pool: SchedulerCapacityPoolStatus) => (
              <span key={pool.id}>
                {messages.t('mon.scheduler.autoscale.pool')}
                {pool.provider}/{pool.execution_zone} · {pool.profile_id ?? pool.id} ·{' '}
                {pool.recommendation} · {messages.number(pool.active_slots)}/
                {messages.number(pool.desired_slots)} · {messages.number(pool.provider_max_slots)} ·{' '}
                {messages.number(pool.backlog)}/{messages.number(pool.unschedulable_backlog)}
              </span>
            ))}
            {(actuator?.actions ?? []).slice(0, 5).map((action) => (
              <span key={action.id}>
                {messages.t('mon.scheduler.action')}
                {action.kind} · {action.state} · {messages.number(action.attempts)} ·{' '}
                {action.pool_id}
                {action.failure_detail === undefined ? '' : ` · ${action.failure_detail}`}
              </span>
            ))}
            {(actuator?.instances ?? []).slice(0, 8).map((instance) => (
              <span key={instance.id}>
                {messages.t('mon.scheduler.instance')}
                {instance.provider_instance_id === ''
                  ? instance.id
                  : instance.provider_instance_id}{' '}
                · {instance.state} · {instance.pool_id}
              </span>
            ))}
          </div>
        </details>
      ) : null}
    </article>
  );
}

/**
 * The projection's state word, from the catalogue when the server's vocabulary
 * is one this console knows and raw when it is not — the same wrong-but-honest
 * degradation the status vocabulary takes, rather than a blank.
 */
const PROJECTION_WORD: Record<string, MessageKey> = {
  current: 'mon.targets.projection.current',
  empty: 'mon.targets.projection.empty',
  lagging: 'mon.targets.projection.lagging',
  invalid: 'mon.targets.projection.invalid',
};

function projectionStateWord(messages: Messages, state: string): string {
  const key = PROJECTION_WORD[state];
  return key === undefined ? state : messages.t(key);
}

/* ---- target reliability and cost ----------------------------------------- */

export function TargetCard({ target }: { target: TargetReliabilityStatus }) {
  const messages = useMessages();
  return (
    <article className="builder-card target-card">
      <h3>
        {target.profile_id === '' ? '-' : target.profile_id} ·{' '}
        {target.image_generation === '' ? '-' : target.image_generation}
      </h3>
      <div className="ep">
        {target.project_name === '' ? target.project_id : target.project_name} · {target.provider}/
        {target.execution_zone} · {target.architecture} · {target.resource_class}
      </div>
      <div className="meta">
        {(target.windows ?? []).map((window) => {
          const denominator = window.successes + window.failures;
          const rate =
            denominator === 0
              ? '—'
              : `${messages.number(window.success_rate_percent, {
                  minimumFractionDigits: 1,
                  maximumFractionDigits: 1,
                })}%`;
          // The SLO outcome is the one conclusion on this card, and it was the
          // same 400-weight secondary ink as the cost figure three lines below
          // it — and the two English words were never in the catalogue at all,
          // so a Chinese reader got "SLO breach" verbatim.
          const sloToken = window.insufficient_data
            ? 'pending'
            : window.slo_met
              ? 'passed'
              : 'failed';
          const slo = window.insufficient_data
            ? messages.t('mon.targets.insufficient')
            : window.slo_met
              ? messages.t('mon.targets.slo.met')
              : messages.t('mon.targets.slo.breach');
          return (
            <span key={window.name} className="target-window">
              <span>
                {window.name} · {messages.t('mon.targets.samples')}
                {messages.number(window.samples)} {messages.number(window.successes)}/
                {messages.number(window.failures)}/{messages.number(window.canceled)}
              </span>
              <span>
                {window.name} · {messages.t('mon.targets.slo')}
                {rate} · <span {...verdictAttributes(messages, sloToken)}>{slo}</span>
              </span>
              <span>
                {window.name} · {messages.t('mon.targets.latency')}
                {messages.number(window.queue_p50_seconds)}/
                {messages.number(window.queue_p95_seconds)}s ·{' '}
                {messages.number(window.run_p50_seconds)}/{messages.number(window.run_p95_seconds)}s
              </span>
              <span>
                {window.name} · {messages.t('mon.targets.cost')}
                {messages.number(window.reserved_cost_microunits / 1000000, {
                  minimumFractionDigits: 3,
                  maximumFractionDigits: 3,
                })}
                /
                {messages.number(window.charged_cost_microunits / 1000000, {
                  minimumFractionDigits: 3,
                  maximumFractionDigits: 3,
                })}
              </span>
              {window.dominant_failure_class === undefined ? null : (
                <span>
                  {window.name} · {messages.t('mon.targets.failure')}
                  {window.dominant_failure_class}
                </span>
              )}
            </span>
          );
        })}
      </div>
    </article>
  );
}

/* ---- worker gateway ------------------------------------------------------ */

export function GatewayCard({
  gateway,
  identities,
}: {
  gateway: WorkerGatewayStatus;
  identities: WorkloadIdentityInventory | null;
}) {
  const messages = useMessages();
  // Nested and coalesced, unlike the scalars beside it. A rollup that answers a
  // partial object — a proxy that trimmed it, a handler mid-rollout — costs a
  // reading here; reaching into a member of it that is not there costs the whole
  // route, because a throw inside the chrome takes every other card with it.
  const runtime = gateway.issuer_runtime ?? {};
  const phase = gateway.phase_work;
  const token = gateway.enabled
    ? gateway.issuer_healthy === false
      ? 'failed'
      : 'passed'
    : 'pending';
  const issuers = identities?.issuers ?? [];
  return (
    <article className="builder-card">
      <h3>{messages.t('mon.gateway.mtls')}</h3>
      <StatusBadge
        token={token}
        label={
          gateway.enabled ? messages.t('mon.gateway.enabled') : messages.t('mon.gateway.disabled')
        }
      />
      <div className="meta">
        <span>
          {messages.t('mon.gateway.authority')}
          {gateway.authority}
        </span>
        <MetaCount labelKey="mon.gateway.connected" value={gateway.connected_sessions ?? 0} />
        <MetaCount labelKey="mon.gateway.registered" value={gateway.registered_sessions ?? 0} />
        <MetaCount labelKey="mon.gateway.tasks" value={gateway.pending_tasks ?? 0} />
        <MetaCount labelKey="mon.gateway.uploads" value={gateway.pending_uploads ?? 0} />
        <span>
          {messages.t('mon.gateway.issuers')}
          {messages.number(gateway.active_issuers)} {resolveStatus('active', messages).label} /{' '}
          {messages.number(gateway.draining_issuers)} {resolveStatus('draining', messages).label} /{' '}
          {messages.number(gateway.revoked_issuers)} {resolveStatus('revoked', messages).label}
        </span>
        <span>
          {messages.t('mon.gateway.certs')}
          {messages.number(gateway.active_certificates)} {resolveStatus('active', messages).label} /{' '}
          {messages.number(gateway.revoked_certificates)} {resolveStatus('revoked', messages).label}
        </span>
        {gateway.expiring_certificates > 0 ? (
          <MetaCount
            labelKey="mon.gateway.expiring"
            value={gateway.expiring_certificates}
            token="blocked"
          />
        ) : null}
        <span>
          {messages.t('mon.gateway.provider')}
          {gateway.issuer_provider}:{gateway.issuer_id}
        </span>
        {/* The issuer's own health is a verdict, and the failure count beside it
            is the evidence for it: both come off the one token. */}
        <MetaVerdict
          labelKey="mon.gateway.provider.health"
          token={runtime.healthy === false ? 'unhealthy' : 'healthy'}
        />
        <MetaCount
          labelKey="mon.gateway.provider.failures"
          value={runtime.consecutive_failures ?? 0}
          token={(runtime.consecutive_failures ?? 0) > 0 ? 'failed' : 'passed'}
        />
        <span>
          {messages.t('mon.gateway.provider.success')}
          <Moment value={runtime.last_success_at} />
        </span>
        {runtime.last_error === undefined || runtime.last_error === '' ? null : (
          <span>
            {messages.t('mon.gateway.provider.error')}
            {runtime.last_error}
          </span>
        )}
        <span>
          {messages.t('mon.gateway.inbound')}
          {gateway.inbound_builder_api ? 'on' : 'off'}
        </span>
        <span>
          {messages.t('mon.gateway.protocol')}
          {messages.t('mon.gateway.protocol.version')}
          {messages.number(gateway.executor_protocol, { useGrouping: false })}
        </span>
        <span>
          {messages.t('mon.gateway.phase')}
          {gateway.phase_executor_mode}
        </span>
        {phase === undefined ? null : (
          <>
            <MetaCount labelKey="mon.gateway.phase.active" value={phase.active} />
            <MetaCount labelKey="mon.gateway.phase.claimed" value={phase.claimed} />
            <MetaCount labelKey="mon.gateway.phase.ready" value={phase.ready} />
            <MetaCount
              labelKey="mon.scheduler.unschedulable"
              value={phase.unschedulable}
              token={phase.unschedulable > 0 ? 'failed' : 'passed'}
            />
            <MetaCount
              labelKey="mon.gateway.phase.blocked"
              value={phase.blocked}
              token={phase.blocked > 0 ? 'blocked' : 'passed'}
            />
            <MetaCount
              labelKey="mon.gateway.phase.failed"
              value={phase.failed}
              token={phase.failed > 0 ? 'failed' : 'passed'}
            />
          </>
        )}
        <span>
          {messages.t('mon.gateway.ttl')}
          {messages.number(gateway.certificate_ttl_min)}m
        </span>
      </div>
      {identities === null ? null : (
        <details className="factory-step-details">
          <summary>{messages.t('mon.gateway.inventory')}</summary>
          <div className="meta">
            {/* Keyed and abbreviated by fingerprint, which is what identifies a
                generation; `issuer_id` names the configured issuer several
                generations share, so a card keyed on it collapses the history
                this disclosure exists to show. The active-leaf count comes back
                with it — the console this replaces printed it here, and it is
                what says whether revoking a generation ends anything. */}
            {issuers.map((issuer) => (
              <span key={issuer.fingerprint} className="mono">
                {issuer.fingerprint.slice(0, 12)}… {issuer.state} · {issuer.provider}:
                {issuer.issuer_id} · {messages.number(issuer.active_certificates)}{' '}
                {resolveStatus('active', messages).label} · <Moment value={issuer.not_after} />
              </span>
            ))}
            {issuers.length === 0 ? <span>{messages.t('mon.gateway.inventory.empty')}</span> : null}
            <span>
              {messages.t('mon.gateway.inventory.recent')}
              {messages.number((identities.certificates ?? []).length)} /{' '}
              {messages.number(identities.certificate_limit)}
            </span>
          </div>
        </details>
      )}
    </article>
  );
}

/* ---- the page ------------------------------------------------------------ */

export function MonitorPage() {
  const messages = useMessages();
  const [rollups, setRollups] = useState<Rollups>(NOTHING_ASKED_YET);
  const [busy, setBusy] = useState(false);
  const trigger = useRef<(() => void) | null>(null);
  // Every one of the eight is a system-admin route, and every request the
  // console makes carries the project it is being made in: the rollups a
  // reader is looking at belong to the project the rail says they are in.
  const { projectID, ready } = useProjectScope();

  useDocumentTitle('title.monitor');

  useEffect(() => {
    if (!ready) {
      return undefined;
    }
    const controller = new AbortController();
    /**
     * Refresh and the poll share one flag, and an activation while a load is in
     * flight is discarded rather than queued: a held Enter on Refresh used to
     * stack eight round trips per keypress, and the second copy of a poll can
     * only ever paint the same numbers the first one is about to.
     */
    const writer = singleWriter(async () => {
      const next = await loadRollups({ projectID, signal: controller.signal });
      if (!controller.signal.aborted) {
        setRollups(next);
      }
    });
    // Only an explicit Refresh reports busy. A poll does not, because a control
    // that flickers into aria-busy every fifteen seconds on its own is a control
    // a screen reader keeps interrupting itself about.
    trigger.current = () => {
      setBusy(true);
      void writer.run().finally(() => {
        if (!controller.signal.aborted) {
          setBusy(false);
        }
      });
    };
    void writer.run();
    const stop = pollWhenVisible(() => void writer.run(), POLL_INTERVAL_MS);
    return () => {
      stop();
      controller.abort();
      trigger.current = null;
    };
  }, [projectID, ready]);

  const ledgerState = cardState<LedgerStatus>(rollups.ledger);
  const schedulerState = cardState<MonitorSchedulerStatus>(rollups.scheduler);
  const scheduler = rollups.scheduler?.kind === 'ok' ? rollups.scheduler.value : null;
  const targets = scheduler?.target_history?.targets ?? [];
  const targetState = resolveState<MonitorSchedulerStatus>({
    outcome: rollups.scheduler,
    count: targets.length,
  });

  const gateway = rollups.gateway?.kind === 'ok' ? rollups.gateway.value : null;
  const identities = rollups.identities?.kind === 'ok' ? rollups.identities.value : null;
  // The card loaded and one section of it did not, which is neither a failed
  // card nor an empty one — and saying which half is still trustworthy is the
  // difference between a missing inventory and an inventory missing because the
  // gateway is down. The sentence is passed already translated because that is
  // what this surface has to say, not the name of a section.
  const gatewayMissing =
    gateway !== null && rollups.identities !== null && rollups.identities.kind !== 'ok'
      ? [messages.t('mon.gateway.inventory.error')]
      : undefined;
  const gatewayState = resolveState<WorkerGatewayStatus>({
    outcome: rollups.gateway,
    ...(gatewayMissing === undefined ? {} : { missing: gatewayMissing }),
  });

  const metadataState = cardState<RuntimeMetadataEnvelope>(rollups.metadata);
  const metadata = rollups.metadata?.kind === 'ok' ? rollups.metadata.value : null;
  const cacheState = cardState<CacheStatus>(rollups.cache);
  const cache = rollups.cache?.kind === 'ok' ? rollups.cache.value : null;

  const builders = rollups.builders?.kind === 'ok' ? (rollups.builders.value.builders ?? []) : [];
  const builderState = resolveState<BuildersStatus>({
    outcome: rollups.builders,
    count: builders.length,
  });

  const instances = rollups.instances?.kind === 'ok' ? rollups.instances.value : [];
  const instanceState = resolveState<Instance[]>({
    outcome: rollups.instances,
    count: instances.length,
  });

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{messages.t('mon.h1')}</h1>
          <p className="sub">{messages.t('mon.sub')}</p>
        </div>
        <div className="actions">
          <button
            className="btn"
            type="button"
            onClick={() => trigger.current?.()}
            {...busyAttributes(busy)}
          >
            {messages.t('common.refresh')}
          </button>
        </div>
      </div>

      <h2 className="section-title">{messages.t('mon.ledger')}</h2>
      <Block state={ledgerState}>
        <div className="builder-grid ledger-grid">
          {ledgerState.state === 'ok' || ledgerState.state === 'partial' ? (
            <LedgerCard ledger={ledgerState.data} />
          ) : null}
        </div>
      </Block>

      <h2 className="section-title">{messages.t('mon.scheduler')}</h2>
      <Block state={schedulerState}>
        <div className="builder-grid ledger-grid">
          {scheduler === null ? null : <SchedulerCard scheduler={scheduler} />}
        </div>
      </Block>

      <h2 className="section-title">{messages.t('mon.targets')}</h2>
      <p className="sub">{messages.t('mon.targets.sub')}</p>
      <Block state={targetState} emptyKey="mon.targets.empty">
        <div className="builder-grid">
          {targets.map((target) => (
            <TargetCard key={target.target_id} target={target} />
          ))}
        </div>
      </Block>

      <h2 className="section-title">{messages.t('mon.gateway')}</h2>
      <Block state={gatewayState}>
        <div className="builder-grid ledger-grid">
          {gateway === null ? null : <GatewayCard gateway={gateway} identities={identities} />}
        </div>
      </Block>

      <h2 className="section-title">{messages.t('mon.metadata')}</h2>
      <Block state={metadataState}>
        <div className="builder-grid ledger-grid">
          {metadata === null ? null : <MetadataCard envelope={metadata} />}
        </div>
      </Block>

      <h2 className="section-title">{messages.t('mon.cache')}</h2>
      <Block state={cacheState}>
        <div className="builder-grid ledger-grid">
          {cache === null ? null : <CacheCard cache={cache} />}
        </div>
      </Block>

      <h2 className="section-title">{messages.t('mon.builders')}</h2>
      <Block state={builderState} emptyKey="mon.noBuilders">
        <div className="builder-grid">
          {builders.map((builder) => (
            <article className="builder-card" key={builder.id}>
              <h3>{builder.id === '' ? '-' : builder.id}</h3>
              <p className="ep mono">{builder.endpoint}</p>
              <StatusBadge token={builder.status} />
              <div className="meta">
                <span>
                  {messages.t('mon.archLabel')}
                  {builder.architecture}
                </span>
                <span>
                  {messages.t('mon.loadLabel')}
                  {messages.number(builder.current_load)}/{messages.number(builder.capacity)}
                </span>
                {builder.native_job_policy === undefined ? null : (
                  <span>
                    {messages.t('mon.policyLabel')}
                    {builder.native_job_policy}
                  </span>
                )}
                <span>
                  {builder.accepting_builds
                    ? messages.t('mon.accepting')
                    : messages.t('mon.notAccepting')}
                </span>
              </div>
            </article>
          ))}
        </div>
      </Block>

      <h2 className="section-title">{messages.t('mon.instances')}</h2>
      <div className="card">
        {/* The table and its state node share one wrapper so the reserve covers
            both: the header row plus the sentence while the payload is out, the
            header row plus its rows afterwards. Never on tbody, which reserves
            nothing at all. */}
        <Block state={instanceState} emptyKey="mon.noInstances">
          <div
            className="table-scroll"
            tabIndex={0}
            role="region"
            aria-label={messages.t('mon.instances')}
          >
            <table className="list" data-cols="6" aria-label={messages.t('mon.instances')}>
              <thead>
                <tr>
                  <th>{messages.t('th.instance')}</th>
                  <th>{messages.t('th.provider')}</th>
                  <th>{messages.t('th.status')}</th>
                  <th>{messages.t('th.ip')}</th>
                  <th>{messages.t('th.created')}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {instances.map((instance) => (
                  <tr key={instance.id}>
                    <td className="mono">{instance.id}</td>
                    <td className="sec">{instance.provider}</td>
                    <td>
                      <StatusBadge token={instance.status} />
                    </td>
                    <td className="mono sec">{instance.ip_address || instance.public_ip || '-'}</td>
                    <td className="sec">{momentText(messages, instance.created_at)}</td>
                    <td>
                      <Link className="btn" to={`/shell/${encodeURIComponent(instance.id)}`}>
                        {messages.t('mon.shell')}
                      </Link>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Block>
      </div>
    </>
  );
}

/* ---- runtime metadata and realtime acceleration -------------------------- */

function MetadataCard({ envelope }: { envelope: RuntimeMetadataEnvelope }) {
  const messages = useMessages();
  const status = envelope.status;
  const integrityOK =
    (status?.missing_artifacts ?? 0) === 0 && (status?.corrupt_artifacts ?? 0) === 0;
  // Three answers, not two. `enabled: false` is a deployment with no PostgreSQL
  // ledger at all, which is a configuration and not a fault; the console this
  // replaces could only ever badge it red, because the shared helper turned the
  // 503 that carries it into a thrown error before the card was reached.
  const token = !envelope.enabled ? 'pending' : envelope.ok && integrityOK ? 'passed' : 'failed';
  return (
    <article className="builder-card">
      <h3>{messages.t('mon.metadata.db')}</h3>
      <StatusBadge token={token} />
      <div className="meta">
        {status === undefined ? (
          <span>{envelope.error ?? ''}</span>
        ) : (
          <>
            <MetaCount labelKey="mon.metadata.infra" value={status.live_infra} />
            <MetaCount
              labelKey="mon.metadata.cleanup"
              value={status.cleanup_failed_infra}
              token={status.cleanup_failed_infra > 0 ? 'failed' : 'passed'}
            />
            <MetaCount labelKey="mon.metadata.published" value={status.published_artifacts} />
            <MetaCount labelKey="mon.metadata.staged" value={status.staged_artifacts} />
            <MetaCount
              labelKey="mon.metadata.missing"
              value={status.missing_artifacts}
              token={status.missing_artifacts > 0 ? 'failed' : 'passed'}
            />
            <MetaCount
              labelKey="mon.metadata.corrupt"
              value={status.corrupt_artifacts}
              token={status.corrupt_artifacts > 0 ? 'failed' : 'passed'}
            />
            <MetaCount
              labelKey="mon.metadata.orphaned"
              value={status.orphaned_artifacts}
              token={status.orphaned_artifacts > 0 ? 'blocked' : 'passed'}
            />
            <MetaCount labelKey="mon.metadata.factory" value={status.factory_runs} />
            <span>
              {messages.t('common.updated')}
              <Moment value={status.last_metadata_update_at} />
            </span>
          </>
        )}
      </div>
    </article>
  );
}

function CacheCard({ cache }: { cache: CacheStatus }) {
  const messages = useMessages();
  return (
    <article className="builder-card">
      <h3>{messages.t('mon.cache.redis')}</h3>
      <StatusBadge token={cache.ok ? 'passed' : 'failed'} />
      <div className="meta">
        <MetaCount labelKey="mon.cache.presence" value={cache.control_plane_presence ?? 0} />
        <span>{messages.t('mon.cache.fallback')}</span>
        <span>
          {messages.t('common.updated')}
          <Moment value={cache.last_success_at} />
        </span>
        {cache.last_error === undefined || cache.last_error === '' ? null : (
          <span>{cache.last_error}</span>
        )}
      </div>
    </article>
  );
}
