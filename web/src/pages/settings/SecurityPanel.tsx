import { useCallback, useEffect, useRef, useState } from 'react';

import { api } from '../../api/endpoints';
import { isSignedOut, resolveState, stateAttribute } from '../../api/state';
import { singleWriter } from '../../api/write';
import type { SingleWriter } from '../../api/write';
import type { ApiOutcome } from '../../api/client';
import type { IAMSessions, IdentityProvider } from '../../api/types';
import { useProjectScope } from '../../app/project';
import type { StepUpGuard } from '../../app/stepup';
import { useMessages } from '../../i18n/context';
import { StatusBadge } from '../../components/StatusBadge';
import { readSession, revokeSession } from './api';
import type { SessionRow } from './api';

/**
 * Sessions and security.
 *
 * Two lists, and the difference between "this failed to load" and "there is
 * nothing here" matters more on this panel than anywhere else in the console:
 * an empty revocation list means every credential for this identity is already
 * gone, and a table that stood headed and empty for a whole round trip said
 * exactly that while the request was still out. Both lists go through
 * `resolveState`, so loading, empty and failed are three screens and not one.
 *
 * Revoke-all is one of the two routes internal/server/iam.go names outright as
 * needing a fresh credential, and revoking one session is the same authority.
 * The guard for both arrives as a prop, and the prompt it raises is rendered by
 * the page rather than here: this panel is a child of `#settings-form`, and a
 * prompt rendered inside it is a prompt whose submit reaches that form's
 * onSubmit — a PUT of forty-five controls in answer to a password typed to
 * revoke a session.
 */

interface SessionsView {
  rows: readonly SessionRow[];
  current: string;
}

/**
 * The sentence a refusal carries.
 *
 * A 401 and a step-up refusal have no `message` of their own on the transport's
 * result type, and the reader still has to be told something other than
 * silence: the outcome's own name is the honest floor.
 */
function failureMessage(outcome: ApiOutcome<unknown>): string {
  return 'message' in outcome ? outcome.message : outcome.kind;
}

export function SecurityPanel({ guard }: { guard: StepUpGuard }) {
  const messages = useMessages();
  const { projectID, ready } = useProjectScope();
  const [sessions, setSessions] = useState<ApiOutcome<SessionsView> | null>(null);
  const [providers, setProviders] = useState<readonly IdentityProvider[] | null>(null);
  /**
   * What a refused revocation says, and where it says it.
   *
   * Never in the list's own state: a write that failed is not a list that could
   * not be read, and putting it there replaced every session on screen with an
   * error box — so a reader who was refused one revocation lost the rows they
   * were about to try the next one on.
   */
  const [writeFailure, setWriteFailure] = useState('');

  /**
   * The answer, as rows.
   *
   * The wire shape is normalised on the way in — see readSession in ./api.ts —
   * so nothing below this line reads a field name off a payload, and a refused
   * list stays an `error` outcome rather than becoming an empty one.
   */
  const apply = useCallback((outcome: ApiOutcome<IAMSessions>): void => {
    if (outcome.kind !== 'ok') {
      setSessions(outcome);
      return;
    }
    setSessions({
      kind: 'ok',
      value: {
        rows: (outcome.value.sessions ?? []).map((row) => readSession(row)),
        current: outcome.value.current_session_id,
      },
    });
  }, []);

  /** The refresh path. The mount has its own, below, so it can be aborted. */
  const reload = useCallback(async (): Promise<void> => {
    apply(await api.iamSessions({ projectID }));
  }, [apply, projectID]);

  useEffect(() => {
    if (!ready) {
      return undefined;
    }
    const controller = new AbortController();
    void api.iamSessions({ projectID, signal: controller.signal }).then((outcome) => {
      if (!controller.signal.aborted) {
        apply(outcome);
      }
    });
    // The providers ride on /api/iam/me, which the chrome also asks for. Asking
    // again is one request, once, on a panel nobody opens by accident; the
    // alternative is a field on the shared identity context, which is in the
    // handoffs.
    //
    // Through the scoped entry point, not the chrome's: this panel has been
    // given a project and asks in it, like every other request the page makes.
    // The chrome's ask is the one that cannot carry a project, because its
    // answer is what names them.
    void api.iamMeInProject({ projectID, signal: controller.signal }).then((outcome) => {
      if (controller.signal.aborted) {
        return;
      }
      setProviders(outcome.kind === 'ok' ? outcome.value.identity_providers : []);
    });
    return () => {
      controller.abort();
    };
  }, [apply, projectID, ready]);

  // One writer for the whole revocation resource: revoking one session and
  // revoking all of them are the same authority, so they share one in-flight
  // flag and a second activation during either is discarded, never queued. The
  // work it runs is held in a ref because which revocation was activated is not
  // known when the writer is built — the writer belongs to the resource, and
  // the resource is "this identity's sessions".
  const workRef = useRef<() => Promise<void>>(() => Promise.resolve());
  const revokerRef = useRef<SingleWriter<void> | null>(null);
  const [revoking, setRevoking] = useState(false);

  const runRevoke = useCallback(async (work: () => Promise<void>): Promise<void> => {
    const revoker = (revokerRef.current ??= singleWriter<void>(() => workRef.current()));
    if (revoker.pending()) {
      return;
    }
    workRef.current = work;
    setRevoking(true);
    try {
      await revoker.run();
    } finally {
      setRevoking(false);
    }
  }, []);

  const revokeOne = (row: SessionRow, current: string) =>
    runRevoke(async () => {
      if (!confirm(messages.t('sec.revoke.confirm'))) {
        return;
      }
      const outcome = await guard(() => revokeSession(row.id, { projectID }));
      if (outcome.kind !== 'ok') {
        setWriteFailure(messages.t('sec.revoke.fail') + failureMessage(outcome));
        return;
      }
      setWriteFailure('');
      setSessions(null);
      if (row.id === current) {
        // Revoking your own session means the next request is unauthenticated;
        // going to /logout is what clears the cookie rather than leaving a
        // console holding a credential the server has already forgotten.
        location.href = '/logout';
        return;
      }
      await reload();
    });

  const revokeAll = () =>
    runRevoke(async () => {
      if (!confirm(messages.t('sec.revokeall.confirm'))) {
        return;
      }
      const outcome = await guard(() => api.revokeAllSessions({ projectID }));
      if (outcome.kind !== 'ok') {
        setWriteFailure(messages.t('sec.revoke.fail') + failureMessage(outcome));
        return;
      }
      location.href = '/logout';
    });

  const rows = sessions !== null && sessions.kind === 'ok' ? sessions.value.rows : [];
  const current = sessions !== null && sessions.kind === 'ok' ? sessions.value.current : '';
  const state = resolveState({ outcome: sessions, count: rows.length });
  const stamp = (value: string): string =>
    messages.dateTime(value, { dateStyle: 'medium', timeStyle: 'short' }) ??
    messages.t('common.never');

  return (
    <>
      <div className="card">
        <h3 className="card-title">{messages.t('sec.idp')}</h3>
        <div className="card-pad">
          <p className="hint">{messages.t('sec.idp.hint')}</p>
          <div className="provider-status-list settings-providers">
            {providers === null ? null : providers.length === 0 ? (
              <div className="empty" data-state="empty">
                {messages.t('sec.idp.none')}
              </div>
            ) : (
              providers.map((provider) => (
                <div className="provider-status-row" key={provider.id}>
                  {[
                    provider.display_name,
                    provider.type,
                    ...(provider.backchannel_logout_enabled
                      ? [messages.t('sec.idp.backchannel')]
                      : []),
                  ].join(' · ')}
                </div>
              ))
            )}
          </div>
        </div>
      </div>

      <div className="card">
        <h3 className="card-title">{messages.t('sec.sessions')}</h3>
        <div className="card-pad">
          <p className="hint">{messages.t('sec.sessions.hint')}</p>
          <div
            className="table-scroll"
            tabIndex={0}
            role="region"
            aria-label={messages.t('sec.sessions')}
          >
            <table className="list" data-cols="7" aria-label={messages.t('sec.sessions')}>
              <thead>
                <tr>
                  <th>{messages.t('th.session')}</th>
                  <th>{messages.t('th.issued')}</th>
                  <th>{messages.t('th.lastseen')}</th>
                  <th>{messages.t('th.expires')}</th>
                  <th>{messages.t('th.authctx')}</th>
                  <th>{messages.t('th.status')}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => {
                  return (
                    <tr key={row.id}>
                      <td className="mono sec">
                        {row.id.slice(0, 8)}
                        {row.id === current ? ` · ${messages.t('sec.current')}` : ''}
                      </td>
                      <td className="sec">{stamp(row.issued_at)}</td>
                      <td className="sec">{stamp(row.last_seen_at)}</td>
                      <td className="sec">{stamp(row.expires_at)}</td>
                      <td className="sec">{`${row.acr || '-'} · ${row.amr.join(', ') || '-'}`}</td>
                      <td className="sec">
                        <StatusBadge token={row.revoked ? 'revoked' : 'active'} />
                      </td>
                      <td>
                        {row.revoked ? null : (
                          <button
                            className="btn"
                            type="button"
                            onClick={() => {
                              void revokeOne(row, current);
                            }}
                            aria-disabled={revoking ? 'true' : 'false'}
                            {...(revoking ? { 'aria-busy': 'true' as const } : {})}
                          >
                            {messages.t('sec.revoke')}
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          {/* The block keeps its height while the request is out, so the two
              buttons below do not walk up the page when the list lands. The
              reserve is on the wrapper and never on tbody: a table row box
              cannot be given one. */}
          <div className="settings-sessions-state">
            {state.state === 'ok' ? null : (
              <div className="empty" data-state={stateAttribute(state)} role="status">
                {state.state === 'loading'
                  ? messages.t('common.loading')
                  : isSignedOut(state)
                    ? // A 401 carries no sentence of its own and the shell is
                      // already navigating to sign-in; "Sessions unavailable:"
                      // followed by nothing is not what happened.
                      messages.t('common.expired')
                    : state.state === 'error'
                      ? messages.t('sec.unavailable') + state.message
                      : messages.t('sec.none')}
              </div>
            )}
          </div>
          {/* Always rendered, so a refusal arrives as a change to a region that
              was already there. Empty most of the time, which is what a
              revocation that worked looks like. */}
          <p className="save-msg err" role="status">
            {writeFailure}
          </p>
          <div className="form-actions">
            <button
              className="btn"
              type="button"
              onClick={() => {
                // Back to loading first: a refreshed list that keeps the old
                // rows on screen while the request is out says the old rows are
                // current, and on this panel that is the one claim that must
                // never be made by accident.
                setSessions(null);
                void reload();
              }}
            >
              {messages.t('sec.refresh')}
            </button>
            <button
              className="btn danger"
              type="button"
              onClick={() => {
                void revokeAll();
              }}
              aria-disabled={revoking ? 'true' : 'false'}
              {...(revoking ? { 'aria-busy': 'true' as const } : {})}
            >
              {messages.t('sec.revokeall')}
            </button>
            {/* A server route, not a console one: it restarts the OIDC exchange
                and comes back with a fresh credential. */}
            <a className="btn" href="/login?step_up=1">
              {messages.t('sec.reauth')}
            </a>
          </div>{' '}
        </div>
      </div>
    </>
  );
}
