/**
 * /build/:jobID — one job, and above all the answer to "where did it break and
 * why".
 *
 * The reading order is the argument. A failed build opens with the failure
 * report directly under the title: what failed, where, the control plane's own
 * words for why, the failing stage's own error line, and where the setting that
 * fixes it is written. Then the pipeline with the failing stage panned into
 * view, then the nine stage cards with that stage marked, then the job's
 * metadata, then the log. In the page this replaces the same sentence was the
 * last card, dressed exactly like an ordinary one, below the fold at 1280x900
 * and at 375px.
 *
 * Nothing here is painted from an absent record. A failed detail fetch used to
 * be swallowed and the pipeline drawn from an empty object, so a job whose
 * record 404s rendered nine chips with Queued marked running and a pulsing dot,
 * for a job that had failed hours earlier. No detail is a state and it renders
 * as one.
 */

import { useCallback, useEffect, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router';

import type { ApiOutcome } from '../../api/client';
import { api } from '../../api/endpoints';
import { resolveState } from '../../api/state';
import { subscribeToJobEvents } from '../../api/stream';
import type { BuildStatus } from '../../api/types';
import { isGranted } from '../../app/capabilities';
import { useProjectScope } from '../../app/project';
import { CAPABILITY_SYSTEM_ADMIN } from '../../app/routes';
import type { PageProps } from '../../app/routes';
import { useStepUp } from '../../app/stepup';
import { useMessages } from '../../i18n/context';
import type { MessageKey } from '../../i18n/messages';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import '../../styles/pages/builds.css';
import { canMaintainJob, useConsoleIdentity, usePolledResource, useSingleWriter } from './data';
import { FailureReport, PipelineRow, StageCards } from './jobstate';
import { readLogDocument } from './logdoc';
import { LogFilterChips, LogMeta, LogPane } from './logpane';
import { ConfirmDialog, StateBox, StatusBadge } from './parts';
import { isTerminal, stageStates } from './stages';

const DETAIL_POLL_MS = 10000;
const LOG_POLL_MS = 5000;

/** Which confirmation is open. One at a time, so one dialog exists at a time. */
type PendingAction = 'cancel' | 'retry' | 'delete' | null;

function outcomeMessage(outcome: ApiOutcome<unknown> | null): string {
  if (outcome === null) {
    return '';
  }
  return 'message' in outcome ? outcome.message : outcome.kind;
}

function basename(path: string): string {
  const cut = path.lastIndexOf('/');
  return cut >= 0 ? path.slice(cut + 1) : path;
}

function MetaTile({
  labelKey,
  children,
  wrap = false,
}: {
  labelKey: MessageKey;
  children: React.ReactNode;
  wrap?: boolean;
}) {
  const messages = useMessages();
  return (
    <div className="stat-tile">
      <h4>{messages.t(labelKey)}</h4>
      <div className={wrap ? 'num wrap' : 'num meta-value'}>{children}</div>
    </div>
  );
}

/** How long the job has been running, or how long it ran. */
function useJobDuration(job: BuildStatus | null): string {
  const messages = useMessages();
  const terminal = job !== null && isTerminal(job.status);
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (terminal) {
      return;
    }
    // A running build's duration is the one number on this page that changes
    // without a fetch, so it ticks on its own rather than waiting ten seconds
    // for the poll to make it true again.
    const timer = setInterval(() => {
      setNow(Date.now());
    }, 1000);
    return () => {
      clearInterval(timer);
    };
  }, [terminal]);

  if (job === null) {
    return '-';
  }
  // Both ends through the instant guard: a Go zero `created_at` read as a job
  // that had been running since the year one, and the tile said so to three
  // digits.
  const started = messages.instant(job.created_at);
  const ended = terminal ? messages.instant(job.updated_at) : new Date(now);
  if (started === null || ended === null) {
    return '-';
  }
  const seconds = (ended.getTime() - started.getTime()) / 1000;
  return seconds < 0 ? '-' : messages.duration(seconds);
}

export function BuildDetailPage({ route, boot }: PageProps) {
  useDocumentTitle(route.titleKey);
  const messages = useMessages();
  const identity = useConsoleIdentity(boot.auth_enabled);
  const navigate = useNavigate();
  const params = useParams();
  // The server already resolved the id from the URL, so a deep link renders its
  // frame with the id in it before the router has parsed anything.
  const jobID = params['jobID'] ?? boot.route.job_id;

  // handlers_build.go resolves the job's project from the header before it will
  // answer for a job at all, so neither poll goes out before the chrome has
  // settled which project this reader is in.
  const { projectID, ready } = useProjectScope();
  const stepUp = useStepUp();
  const loadDetail = useCallback(
    (signal: AbortSignal) => api.buildDetail(jobID, { projectID, signal }),
    [jobID, projectID],
  );
  const loadLogs = useCallback(
    (signal: AbortSignal) => api.buildLogs(jobID, { projectID, signal }),
    [jobID, projectID],
  );
  const detail = usePolledResource(loadDetail, DETAIL_POLL_MS, ready);
  const logs = usePolledResource(loadLogs, LOG_POLL_MS, ready);
  const refreshDetail = detail.refresh;
  const refreshLogs = logs.refresh;

  const [activeFilter, setActiveFilter] = useState('all');
  const [pending, setPending] = useState<PendingAction>(null);
  const [busy, setBusy] = useState(false);
  const [writeFailure, setWriteFailure] = useState('');

  // The stream shortens the latency; the polls above are what make the page
  // correct. Losing the stream costs freshness, never truth — the server
  // refuses it outright when no cache is configured.
  //
  // Scoped like every other request this page makes, and for the same reason:
  // /api/events/jobs resolves the project it fans out from the selector, so a
  // subscription that names none is the unscoped one the control plane refuses
  // for a reader holding more than one project. Not opened before the chrome has
  // settled which project that is, because a stream opened in front of the IAM
  // answer is exactly that subscription.
  useEffect(() => {
    if (!ready) {
      return undefined;
    }
    return subscribeToJobEvents(
      (event) => {
        if (event.job_id === jobID) {
          void refreshDetail();
          void refreshLogs();
        }
      },
      { projectID },
    );
  }, [jobID, projectID, ready, refreshDetail, refreshLogs]);

  const job = detail.lastOk;
  const log = readLogDocument(logs.lastOk);
  const duration = useJobDuration(job);
  const states = stageStates(job, log.text);

  const detailState = resolveState({
    outcome: job === null ? detail.outcome : { kind: 'ok', value: job },
    ...(job !== null && detail.outcome?.kind === 'error' ? { missing: ['detail'] } : {}),
  });

  const terminal = job !== null && isTerminal(job.status);
  const canMaintain = canMaintainJob(identity, job?.project_id);
  const canReachAdmin = isGranted(identity, CAPABILITY_SYSTEM_ADMIN);
  const showCancel = job !== null && !terminal && canMaintain;
  const showRetry =
    job !== null && canMaintain && (job.status === 'failed' || job.status === 'canceled');
  const showDelete = job !== null && terminal && canMaintain;

  // One writer for all three, because they write one resource: the job. A
  // second activation while any of them is in flight is discarded, never
  // queued — a queued retry replayed after a delete would ask the control plane
  // to rebuild a record that no longer exists.
  const runAction = useSingleWriter(async (): Promise<void> => {
    if (pending === null) {
      return;
    }
    setBusy(true);
    try {
      // Delete is one of the three routes internal/server/iam.go requires a
      // fresh credential for. The guard is on all three because it costs
      // nothing when the control plane does not ask: it forwards the outcome
      // untouched unless the answer was a 428.
      const outcome = await stepUp.guard(() =>
        pending === 'cancel'
          ? api.cancelBuild(jobID, { projectID })
          : pending === 'retry'
            ? api.retryBuild(jobID, { projectID })
            : api.deleteBuild(jobID, { projectID }),
      );
      if (outcome.kind !== 'ok') {
        setWriteFailure(
          `${messages.t(pending === 'delete' ? 'detail.delete.fail' : 'detail.action.fail')}${outcomeMessage(outcome)}`,
        );
        return;
      }
      setWriteFailure('');
      setPending(null);
      if (pending === 'delete') {
        // The record this page is about no longer exists, so staying here would
        // mean polling a 404 forever.
        void navigate('/builds');
        return;
      }
      await refreshDetail();
      await refreshLogs();
    } finally {
      setBusy(false);
    }
  });

  // The package the job is for, once the record says which; the route's own
  // heading until then, so the h1 is never blank and never a job id twice.
  const heading =
    job === null || job.package_name === ''
      ? messages.t(route.headingKey)
      : `${job.package_name}${job.version === '' ? '' : ` ${job.version}`}`;

  const dialog: { titleKey: MessageKey; confirmKey: MessageKey; destructive: boolean } | null =
    pending === 'cancel'
      ? { titleKey: 'detail.cancel.confirm', confirmKey: 'detail.cancel', destructive: true }
      : pending === 'retry'
        ? { titleKey: 'detail.retry.confirm', confirmKey: 'detail.retry', destructive: false }
        : pending === 'delete'
          ? { titleKey: 'detail.delete.confirm', confirmKey: 'detail.delete', destructive: true }
          : null;

  const resolved = job?.resolved_context;
  const artifacts = job?.artifacts ?? [];
  const extraArtifacts = artifacts.filter((url) => url !== job?.artifact_url);

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{heading}</h1>
          <p className="sub mono">{jobID}</p>
        </div>
        <div className="actions">
          <Link className="btn" to={`/logs/${encodeURIComponent(jobID)}`}>
            {messages.t('detail.logs')}
          </Link>
          {showRetry ? (
            <button
              type="button"
              className="btn"
              onClick={() => {
                setPending('retry');
              }}
            >
              {messages.t('detail.retry')}
            </button>
          ) : null}
          <button
            type="button"
            className="btn"
            onClick={() => {
              void refreshDetail();
              void refreshLogs();
            }}
          >
            {messages.t('common.refresh')}
          </button>
          {/* Cancelling revokes a running executor's lease and deleting removes
              the record: both are separated from the controls that only read,
              and both wear the danger ink. They used to sit between Retry and
              Refresh in an identical blue pill. */}
          {showCancel || showDelete ? (
            <span className="danger-actions">
              {showCancel ? (
                <button
                  type="button"
                  className="btn danger"
                  onClick={() => {
                    setPending('cancel');
                  }}
                >
                  {messages.t('detail.cancel')}
                </button>
              ) : null}
              {showDelete ? (
                <button
                  type="button"
                  className="btn danger"
                  onClick={() => {
                    setPending('delete');
                  }}
                >
                  {messages.t('detail.delete')}
                </button>
              ) : null}
            </span>
          ) : null}
        </div>
      </div>

      <p className="write-note" role="status" aria-live="polite">
        {writeFailure}
      </p>

      {job === null ? null : (
        <FailureReport job={job} logText={log.text} canReachAdmin={canReachAdmin} />
      )}

      {job === null ? (
        <div className="card">
          {/* Reserved to roughly what the pipeline card occupies, so the log
              below it does not leap up the page the moment the record lands. */}
          <div className="card-pad detail-state">
            <StateBox
              state={detailState}
              emptyKey="detail.norecord"
              // Only on the failure. While the request is still out the box
              // says the page is loading, and a sentence declaring the record
              // unreadable in front of it would be a verdict on a request that
              // has not answered yet.
              {...(detailState.state === 'error' ? { lead: messages.t('detail.norecord') } : {})}
              onRetry={() => {
                void refreshDetail();
              }}
            />
          </div>
        </div>
      ) : (
        <>
          <div className="card">
            <div className="card-pad">
              <PipelineRow states={states} />
              <StageCards stages={log.stages} states={states} job={job} logText={log.text} />
            </div>
          </div>

          <div className="stat-grid">
            <MetaTile labelKey="detail.status">
              <StatusBadge token={job.status} />
            </MetaTile>
            <MetaTile labelKey="detail.arch">{job.arch === '' ? '-' : job.arch}</MetaTile>
            <MetaTile labelKey="detail.created">
              {messages.dateTime(job.created_at) ?? '-'}
            </MetaTile>
            <MetaTile labelKey="detail.updated">
              {messages.dateTime(job.updated_at) ?? '-'}
            </MetaTile>
            <MetaTile labelKey="detail.duration">
              <span className="counter duration-value">{duration}</span>
            </MetaTile>
            {job.instance_id === undefined || job.instance_id === '' ? null : (
              <MetaTile labelKey="detail.instance" wrap>
                {job.instance_id}
              </MetaTile>
            )}
            {resolved?.profile_id === undefined ? null : (
              <MetaTile labelKey="detail.profile" wrap>
                {resolved.profile_id}
              </MetaTile>
            )}
            {resolved?.image_generation === undefined ? null : (
              <MetaTile labelKey="detail.image" wrap>
                {`${resolved.image_id ?? '-'} · ${resolved.image_generation}`}
              </MetaTile>
            )}
            {resolved?.egress_policy === undefined ? null : (
              <MetaTile labelKey="detail.egress" wrap>
                {`${resolved.egress_policy.id} · ${resolved.egress_policy.mode}${
                  resolved.egress_policy_digest === undefined
                    ? ''
                    : ` · ${resolved.egress_policy_digest.slice(0, 19)}…`
                }`}
              </MetaTile>
            )}
            {job.artifact_url === undefined || job.artifact_url === '' ? (
              job.artifact_path === undefined || job.artifact_path === '' ? null : (
                <MetaTile labelKey="detail.artifact" wrap>
                  {basename(job.artifact_path)}
                </MetaTile>
              )
            ) : (
              <MetaTile labelKey="detail.artifact" wrap>
                <a href={job.artifact_url} download>
                  {basename(job.artifact_url)}
                </a>
                {extraArtifacts.map((url) => (
                  <span className="artifact-extra" key={url}>
                    <a href={url} download>
                      {basename(url)}
                    </a>
                  </span>
                ))}
                {extraArtifacts.length === 0 ? null : (
                  <span className="artifact-extra-note">
                    {`+${messages.number(extraArtifacts.length)} ${messages.t('detail.artifact.deps')}`}
                  </span>
                )}
              </MetaTile>
            )}
          </div>
          {detailState.state === 'partial' ? (
            <StateBox
              state={detailState}
              emptyKey="detail.norecord"
              note={`${messages.t('common.loadfail')}${outcomeMessage(detail.outcome)}`}
              onRetry={() => {
                void refreshDetail();
              }}
            />
          ) : null}
        </>
      )}

      <div className="card">
        <h3 className="card-title">{messages.t('detail.livelog')}</h3>
        <div className="card-pad">
          <LogFilterChips log={log} active={activeFilter} onSelect={setActiveFilter} />
          <LogMeta log={log} />
          <LogPane
            log={log}
            activeFilter={activeFilter}
            jobID={jobID}
            loadError={
              logs.lastOk === null && logs.outcome?.kind === 'error' ? logs.outcome.message : null
            }
            onRetry={() => {
              void refreshLogs();
            }}
            follow
          />
        </div>
      </div>

      {dialog === null ? null : (
        <ConfirmDialog
          open
          titleKey={dialog.titleKey}
          // Which job, in the two spellings the operator has: the package it
          // builds and the id they would paste into the CLI. A confirmation
          // that names nothing is one a reader cannot check.
          detail={`${heading} · ${jobID}`}
          confirmLabel={messages.t(dialog.confirmKey)}
          destructive={dialog.destructive}
          busy={busy}
          onConfirm={() => {
            void runAction.run();
          }}
          onCancel={() => {
            setPending(null);
          }}
        />
      )}
      {/* Mounted only while a credential is being asked for, and over the page
          rather than instead of it: the record, the pipeline and the log stay
          exactly where the reader left them. */}
      {stepUp.prompt}
    </>
  );
}
