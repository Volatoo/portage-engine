import { useEffect, useRef, useState } from 'react';

import type { ApiOutcome } from '../../api/client';
import { api } from '../../api/endpoints';
import { resolveState } from '../../api/state';
import type {
  FactoryEvidence,
  FactoryMilestone,
  FactoryStep,
  ImageFactoryStatus,
} from '../../api/types';
import { busyAttributes, singleWriter } from '../../api/write';
import { useProjectScope } from '../../app/project';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import { useMessages } from '../../i18n/context';
import type { Messages } from '../../i18n/messages';
import '../../styles/pages/image-factory.css';
import { Block, Moment, StatusBadge } from '../monitor/parts';
import { momentText } from '../monitor/verdicts';

/**
 * /image-factory — the catalogue that is always there, and the milestone
 * evidence that is only there when something produced it.
 *
 * Ported from imageFactoryContent/imageFactoryJS in internal/dashboard/ui.go.
 * The one thing on this route that must not change is the sentence it says when
 * the evidence file is not configured: it distinguishes "nothing has run" from
 * "the API failed", it names the environment variable that would fix it, and it
 * says in so many words that the UI does not guess. It is `factory.notconfigured`
 * in the catalogue, it is rendered verbatim, and it is the tone every other
 * empty state on this route is written against.
 */

/**
 * This surface does not poll.
 *
 * Milestone evidence is written by a signed CLI operation, not by a running
 * loop, so there is nothing here that changes between two ticks of a timer —
 * and a fifteen-second poll against a route whose payload changes weekly is
 * fifteen thousand unread round trips a day for nothing. Refresh is the control,
 * and it is the honest one.
 */
function useFactoryStatus(): {
  outcome: ApiOutcome<ImageFactoryStatus> | null;
  refresh: () => void;
  busy: boolean;
} {
  const [outcome, setOutcome] = useState<ApiOutcome<ImageFactoryStatus> | null>(null);
  const [busy, setBusy] = useState(false);
  const trigger = useRef<(() => void) | null>(null);
  // The catalogue is asked for in the project the rail says the reader is in,
  // and not before the rail knows which that is.
  const { projectID, ready } = useProjectScope();

  useEffect(() => {
    if (!ready) {
      return undefined;
    }
    const controller = new AbortController();
    // One reader, one flag: a held Enter on Refresh discards its repeats rather
    // than stacking requests whose answers can only be identical.
    const writer = singleWriter(async () => {
      const next = await api.imageFactoryStatus({ projectID, signal: controller.signal });
      if (!controller.signal.aborted) {
        setOutcome(next);
      }
    });
    trigger.current = () => {
      setBusy(true);
      void writer.run().finally(() => {
        if (!controller.signal.aborted) {
          setBusy(false);
        }
      });
    };
    void writer.run();
    return () => {
      controller.abort();
      trigger.current = null;
    };
  }, [projectID, ready]);

  return {
    outcome,
    refresh: () => {
      trigger.current?.();
    },
    busy,
  };
}

/**
 * One evidence line: whatever of label, digest, path, size and time exists.
 *
 * `recorded_at` goes through the same guard every other timestamp on both routes
 * does, so a piece of evidence recorded by a step that never ran reads "never"
 * rather than as a date in year 1.
 */
function evidenceText(messages: Messages, item: FactoryEvidence): string {
  const parts = [item.label === '' ? messages.t('factory.evidence') : item.label];
  if (item.digest !== undefined && item.digest !== '') {
    parts.push(item.digest);
  }
  if (item.path !== undefined && item.path !== '') {
    parts.push(item.path);
  }
  if (item.size_bytes !== undefined) {
    parts.push(messages.bytes(item.size_bytes));
  }
  if (item.recorded_at !== undefined) {
    parts.push(momentText(messages, item.recorded_at));
  }
  return parts.join(' · ');
}

/**
 * A step's start, finish and duration.
 *
 * The duration is computed from two instants and never from two formatted
 * strings, and it is only offered when BOTH ends are real: a zero `time.Time` at
 * one end measured a step that has not run yet as two thousand years long.
 */
function stepMeta(messages: Messages, step: FactoryStep): string {
  const parts: string[] = [];
  const started = messages.instant(step.started_at);
  const completed = messages.instant(step.completed_at);
  if (started !== null) {
    parts.push(`${messages.t('factory.started')} ${momentText(messages, step.started_at)}`);
  }
  if (completed !== null) {
    parts.push(`${messages.t('factory.finished')} ${momentText(messages, step.completed_at)}`);
  }
  if (started !== null && completed !== null) {
    const seconds = (completed.getTime() - started.getTime()) / 1000;
    parts.push(`${messages.t('factory.duration')} ${messages.duration(seconds)}`);
  }
  return parts.join(' · ');
}

/**
 * internal/catalog — ImageManifest, as the catalogue view copies it whole.
 *
 * The profile table's Display column reads `display_model` off it, and
 * src/api/types.ts stops at four of the manifest's keys — so the column resolved
 * `undefined` and printed a dash for every row that had a display model. Named
 * here so the column is right today; correcting the shared type is a handoff.
 */
interface CatalogImageView {
  id?: string;
  generation?: string;
  display_model?: string;
}

function StatTile({ labelKey, value }: { labelKey: Parameters<Messages['t']>[0]; value: string }) {
  const messages = useMessages();
  return (
    <div className="stat-tile">
      <h4>{messages.t(labelKey)}</h4>
      <div className="num">{value}</div>
    </div>
  );
}

function Milestone({ milestone }: { milestone: FactoryMilestone }) {
  const messages = useMessages();
  const steps = milestone.steps ?? [];
  const evidence = milestone.evidence ?? [];
  return (
    <li className="milestone">
      <div>
        <StatusBadge token={milestone.state} />
      </div>
      <div>
        <h3>
          {milestone.id} · {milestone.title}
        </h3>
        {milestone.summary === undefined ? null : <p>{milestone.summary}</p>}
        {/* Rendered whether or not the milestone has completed: "completed
            never" is the fact the line exists to report, and it used to be
            suppressed by a truthiness test that a zero time.Time passes and
            then printed as a date by a formatter that reads it as year 1. */}
        <div className="milestone-meta">
          {messages.t('factory.completed')} <Moment value={milestone.completed_at} />
        </div>
        {evidence.length === 0 ? null : (
          <div className="evidence-list">
            {evidence.map((item, index) => (
              <span key={`${item.label}-${String(index)}`}>{evidenceText(messages, item)}</span>
            ))}
          </div>
        )}
        {steps.length === 0 ? null : (
          <details className="factory-step-details">
            <summary>
              {messages.t('factory.stepLogs')} ({messages.number(steps.length)})
            </summary>
            <div className="factory-step-list">
              {steps.map((step) => {
                const meta = stepMeta(messages, step);
                return (
                  <div className="factory-step" key={step.id}>
                    <div className="factory-step-head">
                      <StatusBadge token={step.state} />
                      <strong>{step.title}</strong>
                      <span className="factory-step-id">{step.id}</span>
                    </div>
                    {meta === '' ? null : <div className="milestone-meta">{meta}</div>}
                    {step.summary === undefined ? null : <p>{step.summary}</p>}
                    {step.log === undefined ? null : (
                      <div className="factory-step-log">
                        {messages.t('factory.log')}: {evidenceText(messages, step.log)}
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          </details>
        )}
      </div>
    </li>
  );
}

export function ImageFactoryPage() {
  const messages = useMessages();
  const { outcome, refresh, busy } = useFactoryStatus();

  useDocumentTitle('title.factory');

  const data = outcome?.kind === 'ok' ? outcome.value : null;
  const catalog = data?.catalog;
  const profiles = catalog?.profiles ?? [];
  const images = (catalog?.images ?? []) as unknown as CatalogImageView[];
  const bundles = catalog?.mirror_bundles ?? [];
  const status = data?.status;
  const milestones = status?.milestones ?? [];
  const blockers = status?.blockers ?? [];
  const desktop = status?.desktop_e2e;

  const pageState = resolveState<ImageFactoryStatus>({ outcome });
  const profileState = resolveState<ImageFactoryStatus>({ outcome, count: profiles.length });
  const milestoneState = resolveState<ImageFactoryStatus>({ outcome, count: milestones.length });

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{messages.t('factory.h1')}</h1>
          {/* The subtitle names the surface until there is evidence, and names
              the evidence's own age once there is: an operator reading a
              milestone board first needs to know how old the board is. */}
          <p className="sub">
            {status === undefined ? (
              messages.t('factory.sub')
            ) : (
              <>
                {messages.t('factory.updated')}
                <Moment value={status.updated_at} />
              </>
            )}
          </p>
        </div>
        <div className="actions">
          <button className="btn" type="button" onClick={refresh} {...busyAttributes(busy)}>
            {messages.t('common.refresh')}
          </button>
        </div>
      </div>

      <p className="factory-note">{messages.t('factory.readonly')}</p>

      <div className="stat-grid">
        {data === null ? null : (
          <>
            <StatTile labelKey="factory.profiles" value={messages.number(profiles.length)} />
            <StatTile labelKey="factory.images" value={messages.number(images.length)} />
            <StatTile labelKey="factory.bundles" value={messages.number(bundles.length)} />
            <StatTile
              labelKey="factory.catalog.stat"
              value={`v${messages.number(catalog?.version ?? 0, { useGrouping: false })}`}
            />
          </>
        )}
      </div>

      {/* The page's own state — a failed load, or the first request still out.
          Separate from the not-configured note below it, which is not a state
          at all: the request succeeded and said so.

          This route renders its own lists and hands Block no children, so all
          three of its blocks reserve nothing: /monitor's floor holds room for a
          card grid that lands inside the same wrapper, and here the same floor
          would be a permanent gap between the tiles and the Catalog heading.
          The console this replaces used a bare <div> at each of these three
          places for exactly that reason. */}
      <Block state={pageState} emptyKey="factory.none" reserve="none">
        {null}
      </Block>

      {/* Verbatim, and deliberately so. It is the one sentence in the product
          that tells a reader which of three things is true — the catalogue
          loaded, the evidence path is unset, and the UI will not invent the
          difference — and rewording any third of it loses one of them. */}
      {data !== null && !data.configured ? (
        <div className="card card-pad factory-note">{messages.t('factory.notconfigured')}</div>
      ) : null}

      <h2 className="section-title">{messages.t('factory.catalog')}</h2>
      <div className="card">
        <div
          className="table-scroll"
          tabIndex={0}
          role="region"
          aria-label={messages.t('factory.catalog')}
        >
          <table className="list" data-cols="7" aria-label={messages.t('factory.catalog')}>
            <thead>
              <tr>
                <th>{messages.t('detail.profile')}</th>
                <th>{messages.t('th.arch')}</th>
                <th>{messages.t('factory.image')}</th>
                <th>{messages.t('detail.egress')}</th>
                <th>{messages.t('factory.displayModel')}</th>
                <th>{messages.t('factory.sets')}</th>
                <th>{messages.t('factory.channel')}</th>
              </tr>
            </thead>
            <tbody>
              {profiles.map((profile) => {
                const image = images.find((item) => item.id === profile.image_id);
                return (
                  <tr key={profile.id}>
                    {/* The class is what keeps the badge rule beside it on this
                        page: every page stylesheet is in the one bundle, so a
                        rule keyed on `table.list td` would set the gap on every
                        status badge in every table on the console. */}
                    <td className="mono factory-profile-id">
                      {profile.id}
                      {profile.default ? (
                        <span className="status green">
                          <span className="dot" />
                          <span>{messages.t('factory.default')}</span>
                        </span>
                      ) : null}
                    </td>
                    <td className="sec">{profile.arch}</td>
                    <td className="mono sec">{profile.image_id}</td>
                    <td className="mono sec">{profile.egress_policy_id}</td>
                    <td className="sec">{image?.display_model ?? '-'}</td>
                    <td className="sec">{(profile.package_sets ?? []).join(', ')}</td>
                    <td className="sec">{profile.channel}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
        <Block state={profileState} emptyKey="factory.none" reserve="none">
          {null}
        </Block>
      </div>

      <div className="factory-grid">
        <section>
          <h2 className="section-title">{messages.t('factory.milestones')}</h2>
          <div className="card">
            <ol className="milestone-list">
              {milestones.map((milestone) => (
                <Milestone key={milestone.id} milestone={milestone} />
              ))}
            </ol>
            <Block state={milestoneState} emptyKey="factory.none" reserve="none">
              {null}
            </Block>
          </div>
        </section>
        <aside>
          <h2 className="section-title">{messages.t('factory.desktop')}</h2>
          <div className="card">
            <div className="card-pad">
              <StatusBadge token={desktop?.state ?? 'not_started'} />
              <p className="factory-note">
                <strong>{messages.t('factory.desktop.strategy')}: </strong>
                {desktop?.strategy ?? '-'}
              </p>
              <p className="factory-note">
                <strong>{messages.t('factory.desktop.ai')}: </strong>
                {desktop?.ai_policy ?? '-'}
              </p>
              <p className="factory-note">
                <strong>{messages.t('factory.desktop.runner')}: </strong>
                {desktop?.runner ?? '-'}
              </p>
              <p className="factory-note">
                <strong>{messages.t('factory.desktop.display')}: </strong>
                {desktop?.display ?? '-'}
              </p>
            </div>
          </div>
          <h2 className="section-title">{messages.t('factory.blockers')}</h2>
          <div className="card">
            <div className="card-pad">
              {/* Not a state box: a status snapshot that reports no blockers has
                  succeeded at something, and the sentence says which snapshot it
                  is speaking for rather than implying nothing was asked. */}
              {blockers.length === 0 ? (
                <p className="factory-note">{messages.t('factory.noBlockers')}</p>
              ) : null}
              {blockers.map((blocker) => (
                <div className="blocker" key={blocker.code}>
                  <strong>{blocker.code}</strong>
                  <p>{blocker.summary}</p>
                  {blocker.action === undefined ? null : (
                    <p>
                      {messages.t('factory.action')}: {blocker.action}
                    </p>
                  )}
                </div>
              ))}
            </div>
          </div>
        </aside>
      </div>
    </>
  );
}
