/**
 * The three surfaces that answer "where did it break, and why": the pipeline
 * row, the nine stage cards under it, and the failure report above both.
 *
 * They are one component tree fed by one pair of payloads, so they cannot
 * disagree about which stage failed. Each takes the reading — the array of stage
 * states — rather than computing its own, and none of them renders at all
 * without a job record: no detail is a state, and a pipeline painted from an
 * empty object is a live reading of a job that may have failed hours ago.
 */

import { useEffect, useRef } from 'react';
import { Link } from 'react-router';

import type { BuildLogStage, BuildStatus } from '../../api/types';
import { useMessages } from '../../i18n/context';
import type { Messages } from '../../i18n/messages';
import { STAGES, STAGE_STATE_WORDS, findRemedy, focusStage, lastFailureLine } from './stages';
import type { Remedy, StageState } from './stages';

function stageStateWord(messages: Messages, state: StageState): string {
  return messages.t(STAGE_STATE_WORDS[state]);
}

/**
 * The nine stages, as a row that wraps.
 *
 * Held in one nowrap row the row measured 1195px inside a 285px box at 375px,
 * with the failing stage 793px past the right edge and a scrollbar as the only
 * thing saying so. The stylesheet wraps it; this pans it, and only within its
 * own box — `scrollIntoView` would move the document too, and the thing an
 * operator must not lose sight of on a failed build is the report above this
 * row. Panning happens when the focused stage changes and not on every poll: a
 * row that re-pans itself every five seconds moves under the reader.
 */
export function PipelineRow({ states }: { states: readonly StageState[] }) {
  const messages = useMessages();
  const wrapRef = useRef<HTMLDivElement>(null);
  const focus = focusStage(states);

  useEffect(() => {
    const wrap = wrapRef.current;
    if (wrap === null || focus < 0) {
      return;
    }
    const node = wrap.children[focus];
    if (!(node instanceof HTMLElement) || wrap.scrollWidth <= wrap.clientWidth) {
      return;
    }
    const target = node.offsetLeft - (wrap.clientWidth - node.offsetWidth) / 2;
    wrap.scrollLeft = Math.max(0, Math.min(target, wrap.scrollWidth - wrap.clientWidth));
  }, [focus]);

  return (
    <div className="pipeline" ref={wrapRef} role="group" aria-label={messages.t('detail.h1')}>
      {STAGES.map((stage, index) => {
        const state = states[index] ?? 'pending';
        return (
          <span key={stage.key} className={`pipe-stage ${state}`}>
            <span className="pipe-chip">
              <span className="dot" />
              <span>{messages.t(stage.labelKey)}</span>
              {/* The chip's own word is the stage's name, never its state, so
                  which of the nine had run, was running, had failed or had never
                  started used to be carried by the dot's hue alone. */}
              <span className="pipe-state">{stageStateWord(messages, state)}</span>
            </span>
            {/* The connector belongs to the stage it leaves, so a wrapped row
                can never open with an arrow pointing at nothing. */}
            {index < STAGES.length - 1 ? <span className="pipe-arrow" /> : null}
          </span>
        );
      })}
    </div>
  );
}

/** How long a stage took, both ends through the instant guard. */
function stageDuration(messages: Messages, stage: BuildLogStage): string | null {
  const from = messages.instant(stage.started_at);
  const to = messages.instant(stage.updated_at);
  if (from === null || to === null) {
    return null;
  }
  const seconds = (to.getTime() - from.getTime()) / 1000;
  return seconds < 0 ? null : messages.duration(seconds);
}

/**
 * One card per stage the log carries.
 *
 * On the stage that failed the card shows the last line of that stage that says
 * something went wrong, not the last line it wrote: every event goes through one
 * appender, so "[publish] artifact promotion failed: …" and "[publish] artifact
 * byte gate passed: …" are the same shape, and the last line of the failing
 * stage was a passing message. The job's own error stands in when the stage
 * logged nothing that says so.
 */
export function StageCards({
  stages,
  states,
  job,
  logText,
}: {
  stages: readonly BuildLogStage[];
  /**
   * The reading, or null where there is none to take: the full-log page has the
   * log document and no job record, and a card that said "pending" there would
   * be claiming something about a job nobody asked about.
   */
  states: readonly StageState[] | null;
  job: BuildStatus | null;
  logText: string;
}) {
  const messages = useMessages();
  return (
    <div className="stage-log-grid">
      {stages.map((stage) => {
        const index = STAGES.findIndex((known) => known.key === stage.id);
        const state = states === null ? null : (states[index] ?? 'pending');
        const known = index >= 0 ? STAGES[index] : undefined;
        const duration = stageDuration(messages, stage);
        const failureLine =
          state === 'failed' ? lastFailureLine(logText, stage.id) || (job?.error ?? '') : '';
        const message = state === 'failed' ? failureLine || stage.last_message : stage.last_message;
        const label = state === 'failed' ? messages.t('logs.fault') : messages.t('logs.last');
        return (
          <div
            key={stage.id}
            className={state === null ? 'stage-log-card' : `stage-log-card ${state}`}
          >
            <strong>{known === undefined ? stage.id : messages.t(known.labelKey)}</strong>
            {state === null ? null : (
              <span className="stage-log-state">{stageStateWord(messages, state)}</span>
            )}
            <span>
              {messages.plural('logs.lines', stage.line_count)}
              {duration === null ? '' : ` · ${duration}`}
            </span>
            {message === undefined || message === '' ? null : (
              <span>{`${label}: ${message.slice(0, 180)}`}</span>
            )}
          </div>
        );
      })}
    </div>
  );
}

/**
 * Where the value an error names is set.
 *
 * Two shapes, because the console owns some of these settings and not others.
 * The linked one is gated: /settings and /image-factory are system-admin
 * routes, so a reader without that capability gets the name of the field and no
 * link — the same fail-closed rule the navigation runs, and a link to a page
 * that would refuse them is not help.
 */
function RemedyLine({ remedy, canReachAdmin }: { remedy: Remedy; canReachAdmin: boolean }) {
  const messages = useMessages();
  if (remedy.kind === 'deployment') {
    return (
      <p className="failure-remedy">
        {messages.t('detail.remedy.env')}
        <span className="mono">{remedy.env}</span>
      </p>
    );
  }
  const label = `${messages.t(remedy.fieldKey)} · ${messages.t(remedy.sectionKey)}`;
  return (
    <p className="failure-remedy">
      {messages.t('detail.remedy.where')}
      {canReachAdmin ? <Link to={remedy.href}>{label}</Link> : <span>{label}</span>}
    </p>
  );
}

/**
 * The sentence a failed build was opened to read, directly under the title.
 *
 * It used to be the last card on the page, dressed exactly like the eight above
 * it, with its heading below the fold at 1280x900 and at 375px both. Three
 * things are on it now: what failed and where, the control plane's own words for
 * why, and — when the error names a value this console can point at — where that
 * value is set. The failing stage's own error line comes with it, so the answer
 * is here rather than nine cards down.
 */
export function FailureReport({
  job,
  logText,
  canReachAdmin,
}: {
  job: BuildStatus;
  logText: string;
  canReachAdmin: boolean;
}) {
  const messages = useMessages();
  const message = job.error ?? '';
  if (message === '') {
    return null;
  }
  const failedStage = STAGES.find((stage) => stage.key === job.failed_stage);
  const heading = ((): string => {
    if (job.status !== 'failed') {
      return messages.t('detail.error');
    }
    return failedStage === undefined
      ? messages.t('detail.failed')
      : messages.t('detail.failed.stage', { stage: messages.t(failedStage.labelKey) });
  })();
  // The log line the failing stage wrote, when it says something the job record
  // does not. A repeat of the error above it would be two sentences saying one
  // thing, so it is shown only when it differs.
  const stageLine = failedStage === undefined ? '' : lastFailureLine(logText, failedStage.key);
  const remedy = findRemedy(message);
  return (
    <section className="failure-report" aria-labelledby="failure-title">
      <h2 id="failure-title">{heading}</h2>
      <p>{message}</p>
      {stageLine === '' || message.includes(stageLine.trim()) ? null : (
        <p className="failure-line">{`${messages.t('logs.fault')}: ${stageLine.trim()}`}</p>
      )}
      {remedy === null ? null : <RemedyLine remedy={remedy} canReachAdmin={canReachAdmin} />}
    </section>
  );
}
