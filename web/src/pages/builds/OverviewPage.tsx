/**
 * /overview — the five cluster counters and the ten newest jobs.
 *
 * Both surfaces reserve their geometry before their payloads land: the tiles are
 * rendered from the first frame with a placeholder bar where the number will be,
 * and the table holds ten rows' worth of height. The page this replaces moved
 * 103px when its data arrived, which is the whole heading below it walking down
 * the page while the reader was already looking at it.
 */

import { useCallback } from 'react';
import { Link } from 'react-router';

import { api } from '../../api/endpoints';
import { resolveState } from '../../api/state';
import type { BuildStatus } from '../../api/types';
import { useProjectScope } from '../../app/project';
import type { PageProps } from '../../app/routes';
import { useMessages } from '../../i18n/context';
import type { MessageKey } from '../../i18n/messages';
import '../../styles/pages/builds.css';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import { usePolledResource } from './data';
import { SkeletonRows, StateBox, StatusBadge } from './parts';

/** What /overview asks for. The server clamps anything above 200. */
const RECENT_LIMIT = 10;
const POLL_MS = 15000;

function rowsOf(value: BuildStatus[] | null): readonly BuildStatus[] {
  // A Go nil slice encodes as `null`, which is not an empty list to `.map`.
  return value ?? [];
}

/**
 * A counter tile.
 *
 * The slot for the number exists before the number does, and it is a bar rather
 * than a zero or a dash: a placeholder that reads as a value is a reading, and
 * "0 building" is a fact about a cluster this page has not heard from yet.
 */
function StatTile({
  labelKey,
  value,
  suffix,
}: {
  labelKey: MessageKey;
  value: string | null;
  suffix?: string;
}) {
  const messages = useMessages();
  return (
    <div className="stat-tile">
      <h4>{messages.t(labelKey)}</h4>
      <div className="num">
        {value === null ? (
          <span className="skeleton-bar" aria-hidden="true" />
        ) : (
          <span className="stat-num">{value}</span>
        )}
        {value !== null && suffix !== undefined ? <small>{suffix}</small> : null}
      </div>
    </div>
  );
}

export function OverviewPage({ route }: PageProps) {
  useDocumentTitle(route.titleKey);
  const messages = useMessages();

  // Both calls resolve the project from the header, so both carry it and
  // neither is made before the chrome knows which project this is.
  const { projectID, ready } = useProjectScope();
  const loadStatus = useCallback(
    (signal: AbortSignal) => api.clusterStatus({ projectID, signal }),
    [projectID],
  );
  const loadBuilds = useCallback(
    (signal: AbortSignal) => api.builds(RECENT_LIMIT, { projectID, signal }),
    [projectID],
  );
  const cluster = usePolledResource(loadStatus, POLL_MS, ready);
  const builds = usePolledResource(loadBuilds, POLL_MS, ready);

  const stats = cluster.lastOk;
  const number = (value: number | undefined): string | null =>
    value === undefined ? null : messages.number(value);

  // The cluster call and the list call are two surfaces on one page, so each
  // resolves its own state: an outage of one does not blank the other.
  const clusterState = resolveState({
    outcome: cluster.lastOk === null ? cluster.outcome : { kind: 'ok', value: cluster.lastOk },
    ...(cluster.lastOk !== null && cluster.outcome?.kind === 'error'
      ? { missing: ['status'] }
      : {}),
  });
  const rows = rowsOf(builds.lastOk);
  const listState = resolveState({
    outcome: builds.lastOk === null ? builds.outcome : { kind: 'ok', value: builds.lastOk },
    count: rows.length,
    ...(builds.lastOk !== null && builds.outcome?.kind === 'error' ? { missing: ['builds'] } : {}),
  });

  const updated = messages.dateTime(stats?.last_updated);
  const refresh = (): void => {
    void cluster.refresh();
    void builds.refresh();
  };

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{messages.t(route.headingKey)}</h1>
          {/* Always present, so the heading below does not move when the first
              timestamp arrives. A non-breaking space is what holds the line. */}
          <p className="sub">
            {updated === null ? ' ' : `${messages.t('common.updated')}${updated}`}
          </p>
        </div>
        <div className="actions">
          <button type="button" className="btn" onClick={refresh}>
            {messages.t('common.refresh')}
          </button>
        </div>
      </div>

      <div className="stat-grid">
        <StatTile labelKey="ov.building" value={number(stats?.active_builds)} />
        <StatTile labelKey="ov.queued" value={number(stats?.queued_builds)} />
        <StatTile labelKey="ov.instances" value={number(stats?.active_instances)} />
        <StatTile labelKey="ov.total" value={number(stats?.total_builds)} />
        <StatTile
          labelKey="ov.rate"
          value={
            stats === null
              ? null
              : messages.number(stats.success_rate, {
                  minimumFractionDigits: 1,
                  maximumFractionDigits: 1,
                })
          }
          suffix="%"
        />
      </div>
      {clusterState.state === 'error' || clusterState.state === 'partial' ? (
        <StateBox
          state={clusterState}
          emptyKey="ov.empty"
          note={`${messages.t('common.loadfail')}${
            cluster.outcome?.kind === 'error' ? cluster.outcome.message : ''
          }`}
          onRetry={() => {
            void cluster.refresh();
          }}
        />
      ) : null}

      <h2 className="section-title">{messages.t('ov.recent')}</h2>
      <div className="card">
        <div className="table-block">
          <div
            className="table-scroll"
            tabIndex={0}
            role="region"
            aria-label={messages.t('ov.recent')}
          >
            <table className="list" data-cols="5">
              <thead>
                <tr>
                  <th>{messages.t('th.package')}</th>
                  <th>{messages.t('th.version')}</th>
                  <th>{messages.t('th.arch')}</th>
                  <th>{messages.t('th.status')}</th>
                  <th>{messages.t('th.updated')}</th>
                </tr>
              </thead>
              <tbody>
                {listState.state === 'loading' ? (
                  <SkeletonRows rows={RECENT_LIMIT} columns={5} />
                ) : (
                  rows.map((build) => (
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
                      <td className="sec">{messages.dateTime(build.updated_at) ?? '-'}</td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
          <StateBox
            state={listState}
            emptyKey="ov.empty"
            note={`${messages.t('common.loadfail')}${
              builds.outcome?.kind === 'error' ? builds.outcome.message : ''
            }`}
            onRetry={() => {
              void builds.refresh();
            }}
          />
        </div>
      </div>
    </>
  );
}
