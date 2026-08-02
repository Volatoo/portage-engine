/**
 * /builds — every job the list endpoint will hand over, with a way to find one
 * and a way to read them a page at a time.
 *
 * Two things about the volume controls are worth stating, because both are
 * consequences of what the API can do rather than choices. /api/v1/builds/list
 * takes one parameter, `limit`, clamped to 200 server-side; it has no status
 * filter, no offset and no cursor. So this page asks for the whole 200, filters
 * and paginates what it holds, and says on screen when it is holding a full
 * page — a truncated set read as the whole set is the failure mode of pretending
 * otherwise. The alternative, filtering server-side, needs a query parameter
 * that does not exist yet.
 *
 * The other is the destructive control. "Clean Up Failed" sat beside "Refresh"
 * in the same blue pill, the same size and the same weight, and it deletes every
 * failed job record on the deployment. It is now separated from the safe
 * actions, wears the danger treatment, appears only when there is something for
 * it to delete and only for a system administrator, and states the count before
 * it runs.
 */

import { useCallback, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router';

import type { ApiOutcome } from '../../api/client';
import { api } from '../../api/endpoints';
import { resolveState } from '../../api/state';
import type { BuildStatus } from '../../api/types';
import { isCompositionEnter } from '../../api/write';
import { isGranted } from '../../app/capabilities';
import { useProjectScope } from '../../app/project';
import { CAPABILITY_SYSTEM_ADMIN } from '../../app/routes';
import type { PageProps } from '../../app/routes';
import { useStepUp } from '../../app/stepup';
import { useMessages } from '../../i18n/context';
import { resolveStatus } from '../../i18n/status';
import '../../styles/pages/builds.css';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import { useConsoleIdentity, usePolledResource, useSingleWriter } from './data';
import { matchesQuery, paginate } from './filter';
import { ConfirmDialog, SkeletonRows, StateBox, StatusBadge } from './parts';

/** The most /api/builds will answer with, whatever this asks for. */
const REQUEST_LIMIT = 200;
const POLL_MS = 15000;

function outcomeMessage(outcome: ApiOutcome<unknown> | null): string {
  if (outcome === null) {
    return '';
  }
  return 'message' in outcome ? outcome.message : outcome.kind;
}

export function BuildsPage({ route, boot }: PageProps) {
  useDocumentTitle(route.titleKey);
  const messages = useMessages();
  const identity = useConsoleIdentity(boot.auth_enabled);

  // /api/v1/builds/list resolves the project from the header, and refuses a
  // reader holding more than one project without it, so the list is not asked
  // for until the chrome has settled which project this is.
  const { projectID, ready } = useProjectScope();
  const load = useCallback(
    (signal: AbortSignal) => api.builds(REQUEST_LIMIT, { projectID, signal }),
    [projectID],
  );
  const builds = usePolledResource(load, POLL_MS, ready);
  const refresh = builds.refresh;
  const stepUp = useStepUp(boot?.principal?.authentication ?? '');

  const [draft, setDraft] = useState('');
  const [query, setQuery] = useState('');
  const [status, setStatus] = useState('');
  const [page, setPage] = useState(1);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [writeFailure, setWriteFailure] = useState('');
  // An Enter that only closed an IME candidate window is not a submission, and
  // both shipped languages are typed through an IME at least some of the time.
  const composing = useRef(false);

  const loaded = builds.lastOk;
  const rows: readonly BuildStatus[] = useMemo(() => loaded ?? [], [loaded]);
  const filtered = useMemo(
    () =>
      rows.filter(
        (build) => matchesQuery(build, query) && (status === '' || build.status === status),
      ),
    [rows, query, status],
  );
  const failedCount = rows.filter((build) => build.status === 'failed').length;

  const shown = paginate(filtered, page);
  const filterActive = query !== '' || status !== '';

  const state = resolveState({
    outcome: builds.lastOk === null ? builds.outcome : { kind: 'ok', value: builds.lastOk },
    count: filtered.length,
    query: filterActive ? query || status : '',
    ...(builds.lastOk !== null && builds.outcome?.kind === 'error' ? { missing: ['builds'] } : {}),
  });

  /**
   * One writer for the one destructive action, so a held Enter, a double click
   * and a slow control plane all produce exactly one delete: the writer discards
   * a second activation rather than queueing it.
   */
  const cleanup = useSingleWriter(async () => {
    setBusy(true);
    try {
      // internal/server/iam.go requires a fresh credential for
      // /api/v1/builds/cleanup-failed. Asked for here, where the write was
      // made, and the write repeated once with it — the alternative is a
      // control that answers a click with a sentence and no way to satisfy it.
      const outcome = await stepUp.guard(() => api.cleanupFailedBuilds({ projectID }));
      if (outcome.kind !== 'ok') {
        setWriteFailure(`${messages.t('detail.action.fail')}${outcomeMessage(outcome)}`);
        return;
      }
      setWriteFailure('');
      setConfirming(false);
      await refresh();
    } finally {
      setBusy(false);
    }
  });

  const clearFilter = (): void => {
    setDraft('');
    setQuery('');
    setStatus('');
    setPage(1);
  };

  // The statuses actually present, plus whichever one is selected, so a filter
  // does not silently drop off the control when the last matching job leaves the
  // page.
  const statusTokens = [...new Set([...rows.map((build) => build.status), status])]
    .filter((token) => token !== '')
    .sort((left, right) => left.localeCompare(right, messages.locale));

  const canCleanUp = isGranted(identity, CAPABILITY_SYSTEM_ADMIN) && failedCount > 0;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{messages.t(route.headingKey)}</h1>
          <p className="sub">{messages.plural('builds.count', rows.length)}</p>
        </div>
        <div className="actions">
          <button
            type="button"
            className="btn"
            onClick={() => {
              void refresh();
            }}
          >
            {messages.t('common.refresh')}
          </button>
          {/* Destructive actions live in their own group, after the safe ones and
              behind a rule, so the control that deletes records is not the one
              next to the control that reloads them. */}
          {canCleanUp ? (
            <span className="danger-actions">
              <button
                type="button"
                className="btn danger"
                onClick={() => {
                  setConfirming(true);
                }}
              >
                {messages.t('builds.cleanup')}
              </button>
            </span>
          ) : null}
        </div>
      </div>

      <form
        className="list-filter"
        role="search"
        onSubmit={(event) => {
          event.preventDefault();
          setQuery(draft);
          setPage(1);
        }}
        onKeyDown={(event) => {
          if (isCompositionEnter({ key: event.key, isComposing: event.nativeEvent.isComposing })) {
            event.preventDefault();
          }
        }}
      >
        <div className="field">
          <label htmlFor="builds-search">{messages.t('packages.search')}</label>
          <input
            id="builds-search"
            type="text"
            value={draft}
            onChange={(event) => {
              const value = event.target.value;
              setDraft(value);
              if (!composing.current) {
                setQuery(value);
                setPage(1);
              }
            }}
            onCompositionStart={() => {
              composing.current = true;
            }}
            onCompositionEnd={(event) => {
              composing.current = false;
              setQuery(event.currentTarget.value);
              setPage(1);
            }}
          />
        </div>
        <div className="field">
          <label htmlFor="builds-status">{messages.t('th.status')}</label>
          <select
            id="builds-status"
            value={status}
            onChange={(event) => {
              setStatus(event.target.value);
              setPage(1);
            }}
          >
            <option value="">{messages.t('filter.all')}</option>
            {statusTokens.map((token) => (
              <option key={token} value={token}>
                {resolveStatus(token, messages).label}
              </option>
            ))}
          </select>
        </div>
        {filterActive ? (
          <button type="button" className="btn" onClick={clearFilter}>
            {messages.t('builds.filter.clear')}
          </button>
        ) : null}
      </form>

      {/* Always rendered, so what it says arrives as a change to a region that
          was already there rather than as a region appearing. Empty most of the
          time, which is what a write that succeeded looks like. */}
      <p className="write-note" role="status" aria-live="polite">
        {writeFailure}
      </p>

      {rows.length >= REQUEST_LIMIT ? (
        <p className="list-note">{messages.t('builds.truncated', { limit: REQUEST_LIMIT })}</p>
      ) : null}

      <div className="card">
        <div className="table-block">
          <div
            className="table-scroll"
            tabIndex={0}
            role="region"
            aria-label={messages.t('builds.h1')}
          >
            <table className="list" data-cols="7">
              <thead>
                <tr>
                  <th>{messages.t('th.package')}</th>
                  <th>{messages.t('th.version')}</th>
                  <th>{messages.t('th.arch')}</th>
                  <th>{messages.t('th.status')}</th>
                  <th>{messages.t('th.jobid')}</th>
                  <th>{messages.t('th.created')}</th>
                  <th>{messages.t('th.updated')}</th>
                </tr>
              </thead>
              <tbody>
                {state.state === 'loading' ? (
                  <SkeletonRows rows={8} columns={7} />
                ) : (
                  shown.rows.map((build) => (
                    <tr key={build.job_id}>
                      <td>
                        <Link to={`/build/${encodeURIComponent(build.job_id)}`}>
                          {build.package_name === ''
                            ? messages.t('detail.unknown')
                            : build.package_name}
                        </Link>
                      </td>
                      <td className="sec">{build.version === '' ? '-' : build.version}</td>
                      <td className="sec">{build.arch === '' ? '-' : build.arch}</td>
                      <td>
                        <StatusBadge token={build.status} />
                      </td>
                      <td className="mono sec" title={build.job_id}>
                        {build.job_id.slice(0, 8)}
                      </td>
                      <td className="sec">{messages.dateTime(build.created_at) ?? '-'}</td>
                      <td className="sec">{messages.dateTime(build.updated_at) ?? '-'}</td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
          <StateBox
            state={state}
            emptyKey="builds.empty"
            filteredKey="builds.none"
            note={`${messages.t('common.loadfail')}${outcomeMessage(builds.outcome)}`}
            onRetry={() => {
              void refresh();
            }}
            actions={
              state.state === 'filtered-empty' ? (
                <button type="button" className="btn" onClick={clearFilter}>
                  {messages.t('builds.filter.clear')}
                </button>
              ) : undefined
            }
          />
        </div>
      </div>

      {/* The range is shown whenever the table is holding back rows — because a
          filter is on, or because there is more than one page of them. The head
          above says how many jobs there are in total, so a filtered table with
          three rows in it reads as three of the eight rather than as eight. */}
      {shown.rows.length > 0 && (filterActive || shown.pages > 1) ? (
        <div className="pager">
          {shown.pages > 1 ? (
            <button
              type="button"
              className="btn"
              aria-disabled={shown.page <= 1 ? 'true' : 'false'}
              onClick={() => {
                setPage(Math.max(1, shown.page - 1));
              }}
            >
              {messages.t('packages.prev')}
            </button>
          ) : null}
          {/* tabular figures and a reserved width, because this counter's digit
              COUNT changes as the reader pages through it. */}
          <span className="counter pager-range">
            {messages.t('packages.page', {
              start: messages.number(shown.first),
              end: messages.number(shown.first + shown.rows.length - 1),
            })}
          </span>
          {shown.pages > 1 ? (
            <button
              type="button"
              className="btn"
              aria-disabled={shown.page >= shown.pages ? 'true' : 'false'}
              onClick={() => {
                setPage(Math.min(shown.pages, shown.page + 1));
              }}
            >
              {messages.t('packages.next')}
            </button>
          ) : null}
        </div>
      ) : null}

      {/* Mounted only while it is being asked, so the markup of a destructive
          control — its label included — is not in the document of a reader who
          was never granted it. A closed dialog is display:none, which hides it
          from a screen and from a screen reader but not from a page scrape. */}
      {confirming ? (
        <ConfirmDialog
          open
          titleKey="builds.cleanup.confirm"
          detail={messages.t('builds.cleanup.count', { count: messages.number(failedCount) })}
          confirmLabel={messages.t('builds.cleanup')}
          busy={busy}
          onConfirm={() => {
            void cleanup.run();
          }}
          onCancel={() => {
            setConfirming(false);
          }}
        />
      ) : null}
      {/* Over the list, never instead of it: a credential request that replaced
          the rows would cost the reader the page they were reading. */}
      {stepUp.prompt}
    </>
  );
}
