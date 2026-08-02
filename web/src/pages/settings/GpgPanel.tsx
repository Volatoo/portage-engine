import { useEffect, useState } from 'react';

import { api } from '../../api/endpoints';
import { resolveState, stateAttribute } from '../../api/state';
import type { ApiOutcome } from '../../api/client';
import type { GPGStatus } from '../../api/types';
import { useProjectScope } from '../../app/project';
import { useMessages } from '../../i18n/context';
import { resolveStatus } from '../../i18n/status';
import { queueCount } from './api';

/**
 * The signing panel. Read-only, and read-only on purpose: the private key
 * exists only inside the isolated signer's key volume, the control plane
 * submits digest-bound tasks through PostgreSQL, and neither a builder nor this
 * bundle can read or generate one. There is nothing here to press.
 */
export function GpgPanel() {
  const messages = useMessages();
  const { projectID, ready } = useProjectScope();
  const [status, setStatus] = useState<ApiOutcome<GPGStatus> | null>(null);
  const [publicKey, setPublicKey] = useState<string | null>(null);

  useEffect(() => {
    if (!ready) {
      return undefined;
    }
    const controller = new AbortController();
    void api.gpgStatus({ projectID, signal: controller.signal }).then((outcome) => {
      if (controller.signal.aborted) {
        return;
      }
      setStatus(outcome);
      if (outcome.kind !== 'ok' || !outcome.value.ready) {
        return;
      }
      // Asked for only once the signer says a key exists: a 503 for a key that
      // was never generated is not news, and printing its message under a
      // heading that says "Public Key" reads as a broken deployment.
      void api.publicKey().then((key) => {
        if (!controller.signal.aborted && key.kind === 'ok') {
          setPublicKey(key.value.public_key);
        }
      });
    });
    return () => {
      controller.abort();
    };
  }, [projectID, ready]);

  const state = resolveState({ outcome: status });
  const value = status !== null && status.kind === 'ok' ? status.value : null;

  const signing = ((): string => {
    if (value === null) {
      return state.state === 'loading' ? messages.t('common.loading') : '-';
    }
    if (!value.enabled) {
      return messages.t('set.gpg.off');
    }
    return value.ready ? messages.t('set.gpg.on') : messages.t('set.gpg.wait');
  })();

  // The three queue words come from the one status vocabulary rather than from
  // three literals, so the badge on /builds and this counter cannot end up
  // spelling the same backend token two different ways.
  const queue = value?.queue;
  const counts: readonly [string, number][] = [
    [resolveStatus('queued', messages).label, queueCount(queue, 'queued')],
    [resolveStatus('claimed', messages).label, queueCount(queue, 'claimed')],
    [resolveStatus('failed', messages).label, queueCount(queue, 'failed')],
  ];

  return (
    <>
      <div className="card">
        <h3 className="card-title">{messages.t('set.sec.gpg')}</h3>
        <div className="card-pad">
          <div className="stat-grid settings-gpg-grid">
            <div className="stat-tile">
              <h4>{messages.t('set.gpg.state')}</h4>
              <div className="num settings-gpg-state">{signing}</div>
            </div>
            <div className="stat-tile">
              <h4>{messages.t('set.gpg.keyid')}</h4>
              <div className="num wrap">{value?.key_id || '-'}</div>
            </div>
            <div className="stat-tile">
              <h4>{messages.t('set.gpg.mode')}</h4>
              <div className="num wrap">{value?.mode || '-'}</div>
            </div>
            <div className="stat-tile">
              <h4>{messages.t('set.gpg.queue')}</h4>
              <div className="num wrap settings-gpg-queue">
                {counts.map(([label, count], index) => (
                  <span key={label}>
                    {index === 0 ? '' : ' · '}
                    {/* tabular figures and a floor of two characters: these
                        three counters change digit COUNT, and without a slot of
                        their own the two beside them stepped sideways on every
                        reload. */}
                    <span className="counter">{messages.number(count)}</span> {label}
                  </span>
                ))}
              </div>
            </div>
          </div>
          <p className="hint">{messages.t('set.gpg.hint')}</p>
          {state.state === 'error' ? (
            <div className="empty" data-state={stateAttribute(state)} role="status">
              {messages.t('common.loadfail') + state.message}
            </div>
          ) : null}
        </div>
      </div>
      <div className="card">
        <h3 className="card-title">{messages.t('set.gpg.pubkey')}</h3>
        <div className="card-pad">
          <pre className="log-view settings-pubkey">{publicKey ?? '-'}</pre>
          <div className="form-actions">
            {/* Not a router link: /api/keys/download is a server route outside
                the console base, and it answers with a file, not a page. */}
            <a className="btn" href="/api/keys/download">
              {messages.t('set.gpg.download')}
            </a>
          </div>
        </div>
      </div>
    </>
  );
}
