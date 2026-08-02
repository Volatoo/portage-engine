/**
 * /logs/:jobID — the whole log document, and nothing about the job.
 *
 * This route and the pane embedded in the detail page read the same endpoint
 * through the same components, which is the point: they disagreed once, and the
 * one that rendered nothing was the one an operator reaches for first. What
 * differs here is what is honestly available — there is no job record on this
 * route, so the stage cards carry line counts and timings and claim nothing
 * about which stage was running.
 */

import { useCallback, useState } from 'react';
import { Link, useParams } from 'react-router';

import type { ApiOutcome } from '../../api/client';
import { api } from '../../api/endpoints';
import { resolveState } from '../../api/state';
import { useProjectScope } from '../../app/project';
import type { PageProps } from '../../app/routes';
import { useMessages } from '../../i18n/context';
import '../../styles/pages/builds.css';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import { usePolledResource } from './data';
import { StageCards } from './jobstate';
import { readLogDocument } from './logdoc';
import { LogFilterChips, LogMeta, LogPane } from './logpane';
import { StateBox } from './parts';

const POLL_MS = 5000;

function outcomeMessage(outcome: ApiOutcome<unknown> | null): string {
  if (outcome === null) {
    return '';
  }
  return 'message' in outcome ? outcome.message : outcome.kind;
}

export function LogsPage({ route, boot }: PageProps) {
  useDocumentTitle(route.titleKey);
  const messages = useMessages();
  const params = useParams();
  const jobID = params['jobID'] ?? boot.route.job_id;

  // The log route resolves the job's project the same way the detail route
  // does, so it carries the same header and waits for the same answer.
  const { projectID, ready } = useProjectScope();
  const load = useCallback(
    (signal: AbortSignal) => api.buildLogs(jobID, { projectID, signal }),
    [jobID, projectID],
  );
  const logs = usePolledResource(load, POLL_MS, ready);
  const refresh = logs.refresh;
  const [activeFilter, setActiveFilter] = useState('all');

  const log = readLogDocument(logs.lastOk);
  // A document with no text is empty, not broken — a job can genuinely produce
  // no output. The pane's own floor is what separates that from a document that
  // reported lines and handed over none of them.
  const state = resolveState({
    outcome: logs.lastOk === null ? logs.outcome : { kind: 'ok', value: logs.lastOk },
    count: log.text === '' ? 0 : 1,
    ...(logs.lastOk !== null && logs.outcome?.kind === 'error' ? { missing: ['logs'] } : {}),
  });

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{messages.t(route.headingKey)}</h1>
          <p className="sub mono">{jobID}</p>
        </div>
        <div className="actions">
          <Link className="btn" to={`/build/${encodeURIComponent(jobID)}`}>
            {messages.t('logs.back')}
          </Link>
          <button
            type="button"
            className="btn"
            onClick={() => {
              void refresh();
            }}
          >
            {messages.t('common.refresh')}
          </button>
        </div>
      </div>

      <div className="card">
        <div className="card-pad">
          <LogFilterChips log={log} active={activeFilter} onSelect={setActiveFilter} />
          <LogMeta log={log} />
          <StageCards stages={log.stages} states={null} job={null} logText={log.text} />
          {/* Only the frame before the first answer belongs to the state box.
              A failed fetch is the pane's own fault to report, so the sentence
              here reads "Failed to load logs: …" and not a bare status line,
              and the reader gets the same words and the same retry on both
              routes over this document. */}
          {state.state === 'loading' ? (
            <StateBox state={state} emptyKey="logs.none" />
          ) : (
            <LogPane
              log={log}
              activeFilter={activeFilter}
              jobID={jobID}
              loadError={logs.lastOk === null ? outcomeMessage(logs.outcome) || null : null}
              onRetry={() => {
                void refresh();
              }}
              fullLogLink={false}
            />
          )}
        </div>
      </div>
    </>
  );
}
