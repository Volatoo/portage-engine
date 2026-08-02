/**
 * The log pane and the chips above it.
 *
 * The pane and the chips above it are drawn from one payload, so they can only
 * disagree if a render did not land — and an empty box under a chip reading
 * "All (2628)" is a claim the reader has no way to check. The embedded pane did
 * exactly that on a completed job across five isolated loads, while the same
 * page reported a 264 KiB log and the separate /logs route rendered it fine.
 *
 * So the disagreement is checked after every render rather than assumed away: a
 * pane holding nothing while the fetch reported lines is an ERROR with a retry
 * and a way to the full log, and a job that genuinely produced no output is
 * EMPTY. Those are different sentences because they are different facts.
 */

import { useEffect, useRef } from 'react';
import { Link } from 'react-router';

import { useMessages } from '../../i18n/context';
import { logFault } from './logdoc';
import type { LogDocument, LogFault } from './logdoc';
import { LOG_FILTERS, countLogLines, filterLineCount, filterLines } from './stages';

/** The chips, each carrying the count the pane below is held to. */
export function LogFilterChips({
  log,
  active,
  onSelect,
}: {
  log: LogDocument;
  active: string;
  onSelect: (key: string) => void;
}) {
  const messages = useMessages();
  return (
    <div className="log-filters">
      {LOG_FILTERS.map((filter) => (
        <button
          key={filter.key}
          type="button"
          className="btn"
          aria-pressed={active === filter.key ? 'true' : 'false'}
          onClick={() => {
            onSelect(filter.key);
          }}
        >
          {`${messages.t(filter.labelKey)} (${messages.number(
            filterLineCount(filter, log.text, log.stages),
          )})`}
        </button>
      ))}
    </div>
  );
}

/** Size, freshness and whether the middle of the log was dropped. */
export function LogMeta({ log }: { log: LogDocument }) {
  const messages = useMessages();
  const generated = messages.dateTime(log.generatedAt);
  return (
    <div className="log-meta">
      <span>{`${messages.t('logs.bytes')}: ${messages.bytes(log.bytes)}`}</span>
      {generated === null ? null : <span>{`${messages.t('logs.generated')}: ${generated}`}</span>}
      {log.truncated ? <span>{messages.t('logs.truncated')}</span> : null}
    </div>
  );
}

/**
 * The fault box: a sentence, a retry, and the way to the full log.
 *
 * Its controls are the point. The pane it sits above is the one an operator
 * reaches for when a build has failed, and a box that renders nothing and
 * explains nothing leaves them with no next step at all.
 */
function LogFaultBox({
  fault,
  jobID,
  onRetry,
  fullLogLink,
}: {
  fault: NonNullable<LogFault>;
  jobID: string;
  onRetry: () => void;
  fullLogLink: boolean;
}) {
  const messages = useMessages();
  const sentence =
    fault.kind === 'load'
      ? `${messages.t('logs.fail')}${fault.message}`
      : messages.t('logs.blank', { lines: messages.plural('logs.lines', fault.counted) });
  return (
    <div className="log-fault" role="status" aria-live="polite">
      <p>{sentence}</p>
      <div className="log-fault-actions">
        <button type="button" className="btn" onClick={onRetry}>
          {messages.t('common.retry')}
        </button>
        {fullLogLink ? (
          <Link className="btn" to={`/logs/${encodeURIComponent(jobID)}`}>
            {messages.t('logs.open')}
          </Link>
        ) : null}
      </div>
    </div>
  );
}

/**
 * Whether the pane is showing its newest line.
 *
 * Within about two lines of the end rather than exactly at it: a scroll position
 * lands on a fractional pixel often enough that an exact comparison reads a
 * reader who is at the bottom as one who is not.
 */
function atNewestLine(pane: HTMLPreElement): boolean {
  return pane.scrollHeight - pane.scrollTop - pane.clientHeight < 40;
}

export interface LogPaneProps {
  log: LogDocument;
  activeFilter: string;
  jobID: string;
  /** The message from a failed fetch, or null when the fetch answered. */
  loadError: string | null;
  onRetry: () => void;
  /** Whether to keep the view pinned to the newest line, as a live log does. */
  follow?: boolean;
  /**
   * Whether the fault box offers the full-log route. The embedded pane does,
   * because that route is the one surface known to render this document; the
   * full-log page itself does not, because a link to the page you are on is not
   * a way out of anything.
   */
  fullLogLink?: boolean;
}

export function LogPane({
  log,
  activeFilter,
  jobID,
  loadError,
  onRetry,
  follow = false,
  fullLogLink = true,
}: LogPaneProps) {
  const messages = useMessages();
  const paneRef = useRef<HTMLPreElement>(null);
  const filter = LOG_FILTERS.find((entry) => entry.key === activeFilter) ?? LOG_FILTERS[0];
  const lines = filter === undefined ? [] : filterLines(log.text, filter);
  const body = lines.join('\n');
  const renderedLines = countLogLines(body);
  const fault = filter === undefined ? null : logFault(log, filter, renderedLines, loadError);

  // Pinned to the newest line only if the reader was already there, so a log
  // being appended to does not jump under someone who has scrolled up to read an
  // earlier stage. Whether they were there is recorded when they last moved,
  // because after the append there is no way left to ask: the container has
  // already grown by every appended line, so a reader sitting at the newest line
  // measures as one who scrolled up by exactly the length of the append, and the
  // live log answers "not following" on every tick that gives it something to
  // follow. It starts pinned, which is the line a live log is opened for.
  //
  // renderLogs in internal/dashboard/ui.go could ask it on either side of the
  // write because it owned the write. Here React does, and every effect runs
  // after it — so the answer is recorded when the reader gives it rather than
  // asked for once it is too late to get.
  const pinned = useRef(true);
  useEffect(() => {
    const pane = paneRef.current;
    if (pane === null || !follow) {
      return undefined;
    }
    const remember = (): void => {
      pinned.current = atNewestLine(pane);
    };
    pane.addEventListener('scroll', remember, { passive: true });
    return () => {
      pane.removeEventListener('scroll', remember);
    };
  }, [follow]);

  // The scroll itself is after paint rather than before it: it moves a scroll
  // position and lays nothing out, and a layout effect in a component that is
  // also rendered to static markup warns on every render that produces one.
  useEffect(() => {
    const pane = paneRef.current;
    if (pane === null || !follow || !pinned.current) {
      return;
    }
    pane.scrollTop = pane.scrollHeight;
  }, [body, follow]);

  // A filter that selects nothing out of a log that exists is a different
  // sentence from a job that has not logged anything yet — and neither of them
  // is what an empty pane means when the box above it is reporting a fault.
  // "(no logs yet)" under "Failed to load logs" is the page contradicting
  // itself, so on a fault the pane says nothing and the fault box speaks.
  const placeholder =
    fault !== null
      ? ''
      : log.text === ''
        ? messages.t('logs.none')
        : messages.t('logs.none.filter');

  return (
    <>
      {fault === null ? null : (
        <LogFaultBox fault={fault} jobID={jobID} onRetry={onRetry} fullLogLink={fullLogLink} />
      )}
      <pre className="log-view" ref={paneRef}>
        {body === '' ? placeholder : body}
      </pre>
    </>
  );
}
