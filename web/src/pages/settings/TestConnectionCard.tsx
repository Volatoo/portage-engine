import { useRef, useState } from 'react';

import { StatusBadge } from '../../components/StatusBadge';
import { useMessages } from '../../i18n/context';
import { useProjectScope } from '../../app/project';
import type { StepUpGuard } from '../../app/stepup';
import { singleWriter } from '../../api/write';
import type { SingleWriter } from '../../api/write';
import type { ApiOutcome } from '../../api/client';
import type { CloudSettings } from '../../api/types';
import { failureMessage, testCloudSettings } from './api';
import type { CloudSettingsTestResult, PVENodeRow } from './api';
import { IDLE, noticeText } from './notice';
import type { Notice } from './notice';

/**
 * Test Connection: does the Proxmox endpoint in the form answer, and does it
 * host the template.
 *
 * It posts the form rather than the stored settings, because the whole point is
 * to check credentials the operator has typed and not yet saved —
 * handleCloudSettingsTest falls back to the stored value only for the fields
 * the body leaves empty, which is what lets a stored secret be reused while a
 * newly typed endpoint is tried.
 *
 * It writes nothing, so it does not share the settings resource's writer; it
 * gets one of its own, for the same reason every control does — a second click
 * during a round trip to a cluster that is not answering is a second connection
 * attempt nobody asked for, and the answer would arrive out of order.
 *
 * POST /api/v1/settings/cloud/test is one of the routes internal/server/iam.go
 * gates on a fresh credential, so the refusal is answered where the request is
 * made and the test repeated with it — inside the writer, so the retry is still
 * the one attempt that flag guards.
 *
 * The guard arrives as a prop and the prompt it raises does not appear here at
 * all. This card renders inside `#settings-form`, and a prompt rendered here is
 * a prompt whose submit reaches that form's onSubmit however it is portalled:
 * answering a password would PUT the whole settings resource, and a successful
 * PUT empties the four secret inputs. The page renders the prompt outside the
 * form instead.
 */
export function TestConnectionCard({
  collect,
  guard,
}: {
  collect: () => Partial<CloudSettings>;
  guard: StepUpGuard;
}) {
  const messages = useMessages();
  const { projectID } = useProjectScope();
  const [notice, setNotice] = useState<Notice>(IDLE);
  const [nodes, setNodes] = useState<readonly PVENodeRow[] | null>(null);
  const [busy, setBusy] = useState(false);

  /**
   * What the next test posts, filled at the activation and read by the writer.
   *
   * The writer is built once, so it cannot close over the form: `collect` is
   * rebuilt every time the draft changes, and a writer holding the first one
   * would post the empty form this card was mounted with rather than the
   * credentials the operator typed — which is the whole thing this card exists
   * to check.
   */
  const outbound = useRef<{ body: Partial<CloudSettings>; scope: { projectID?: string } }>({
    body: {},
    scope: {},
  });
  const writerRef = useRef<SingleWriter<ApiOutcome<CloudSettingsTestResult>> | null>(null);

  const run = async (): Promise<void> => {
    const writer = (writerRef.current ??= singleWriter(() =>
      guard(() => testCloudSettings(outbound.current.body, outbound.current.scope)),
    ));
    if (writer.pending()) {
      return;
    }
    outbound.current = { body: collect(), scope: { projectID } };
    setBusy(true);
    setNotice({ state: 'busy', key: 'set.testing' });
    try {
      const outcome = await writer.run();
      if (outcome === null) {
        return;
      }
      if (outcome.kind !== 'ok') {
        setNodes(null);
        setNotice({ state: 'failed', key: 'set.testfail', detail: failureMessage(outcome) });
        return;
      }
      if (!outcome.value.ok) {
        // The handler answers a refused connection with 200 and `ok: false`, so
        // a transport failure and a cluster that said no are two different
        // branches here rather than one.
        setNodes(null);
        setNotice({ state: 'failed', key: 'set.testfail', detail: outcome.value.error ?? '-' });
        return;
      }
      const rows = outcome.value.nodes ?? [];
      setNodes(rows);
      setNotice({ state: 'ok', plural: { key: 'set.testok', count: rows.length } });
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <div className="card-actions">
        <button
          className="btn"
          type="button"
          onClick={() => {
            void run();
          }}
          aria-disabled={busy ? 'true' : 'false'}
          {...(busy ? { 'aria-busy': 'true' as const } : {})}
        >
          {messages.t('set.test')}
        </button>
        {/* A node that exists to carry one sentence of state, and holds nothing
            else. The card below it is rendered content and carries no live
            region: announcing a five-column table every time somebody tests a
            connection is how /status came to re-read its whole component list
            every thirty seconds. */}
        <span className="save-msg settings-msg" data-state={notice.state} role="status">
          {noticeText(messages, notice)}
        </span>
      </div>
      {nodes === null ? null : (
        <div className="card settings-nodes">
          <h3 className="card-title">{messages.t('set.clusternodes')}</h3>
          <div
            className="table-scroll"
            tabIndex={0}
            role="region"
            aria-label={messages.t('set.clusternodes')}
          >
            <table className="list" data-cols="5" aria-label={messages.t('set.clusternodes')}>
              <thead>
                <tr>
                  <th>{messages.t('th.node')}</th>
                  <th>{messages.t('th.status')}</th>
                  <th>{messages.t('th.freemem')}</th>
                  <th>{messages.t('th.cpu')}</th>
                  <th>{messages.t('th.hastpl')}</th>
                </tr>
              </thead>
              <tbody>
                {nodes.map((node) => {
                  return (
                    <tr key={node.node}>
                      <td>{node.node}</td>
                      <td>
                        {/* Dot and word together, always. The dot's hue comes
                            from the one vocabulary; the word beside it is what
                            a reader in forced-colors mode — where every dot
                            goes one colour — actually reads. */}
                        <StatusBadge token={node.status} />
                      </td>
                      <td className="sec">
                        {messages.number(node.free_mem_gb, {
                          minimumFractionDigits: 1,
                          maximumFractionDigits: 1,
                        })}
                        {' GB'}
                      </td>
                      <td className="sec">
                        {messages.number(node.cpu_load, {
                          style: 'percent',
                          maximumFractionDigits: 0,
                        })}
                      </td>
                      <td className="sec">
                        {node.has_template ? messages.t('set.yes') : messages.t('set.no')}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </>
  );
}
