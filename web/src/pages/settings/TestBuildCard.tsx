import { useEffect, useRef, useState } from 'react';
import { Link } from 'react-router';

import { api } from '../../api/endpoints';
import type { ApiOutcome } from '../../api/client';
import type { CloudSettingsResponse } from '../../api/types';
import { useProjectScope } from '../../app/project';
import { useMessages } from '../../i18n/context';
import { StatusBadge } from '../../components/StatusBadge';
import { failureMessage } from './api';
import { TextField } from './fields';
import type { FormBinding } from './fields';
import { IDLE, noticeText } from './notice';
import type { Notice } from './notice';

/**
 * The full-pipeline test: save the settings, submit one build against them,
 * then watch it.
 *
 * This is the settings resource's second writer, and it stands down rather than
 * racing the first. A build submitted against half-written settings is a
 * machine provisioned from a configuration nobody chose, so when the shared
 * writer answers `null` — a save is already in flight — this says so and stops.
 * It never retries the save: every submission takes a fresh native Gentoo VM,
 * there is no warm pool, and a second attempt is a second machine.
 */

const TERMINAL = new Set(['failed', 'completed', 'success', 'canceled']);
const POLL_MS = 5000;

export interface TestBuildCardProps {
  form: FormBinding;
  /** The shared settings writer. `null` means the activation was discarded. */
  save: () => Promise<ApiOutcome<CloudSettingsResponse> | null>;
  /** Attributes a rejection to a control. True when it landed somewhere. */
  attribute: (message: string) => boolean;
}

export function TestBuildCard({ form, save, attribute }: TestBuildCardProps) {
  const messages = useMessages();
  // The submit and the poll that follows it are project-scoped routes, and the
  // project is the one the rail says the reader is in.
  const { projectID } = useProjectScope();
  const [notice, setNotice] = useState<Notice>(IDLE);
  const [job, setJob] = useState<{ id: string; status: string; error: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const noticeRef = useRef<HTMLSpanElement>(null);
  const runningRef = useRef(false);

  // The poll is a real interval and not a request loop: a build runs for hours,
  // and a page left open in a background tab must not keep asking. `hidden` is
  // checked inside the tick rather than by tearing the timer down, so returning
  // to the tab resumes without a round trip that has to re-establish anything.
  const jobID = job?.id ?? null;
  const settled = job !== null && TERMINAL.has(job.status);
  useEffect(() => {
    if (jobID === null || settled) {
      return;
    }
    const timer = setInterval(() => {
      if (document.hidden) {
        return;
      }
      void api.buildDetail(jobID, { projectID }).then((outcome) => {
        if (outcome.kind !== 'ok') {
          // A failed poll is not a failed build. The last known state stays on
          // screen; the next tick asks again.
          return;
        }
        setJob({
          id: jobID,
          status: outcome.value.status,
          error: outcome.value.error ?? '',
        });
      });
    }, POLL_MS);
    return () => {
      clearInterval(timer);
    };
  }, [jobID, projectID, settled]);

  const start = async (): Promise<void> => {
    if (runningRef.current) {
      return;
    }
    runningRef.current = true;
    setBusy(true);
    try {
      setNotice({ state: 'busy', key: 'set.testbuild.saving' });
      const saved = await save();
      if (saved === null) {
        setNotice({ state: 'failed', key: 'set.testbuild.busy' });
        return;
      }
      if (saved.kind !== 'ok') {
        const detail = failureMessage(saved);
        setNotice({ state: 'failed', key: 'set.savefail', detail });
        // Same contract as the footer: attributed to a control when the
        // server's prose names one, otherwise focus lands on the sentence that
        // says it failed, because a page that stays put reads as a page that
        // did nothing.
        if (!attribute(detail)) {
          noticeRef.current?.focus();
        }
        return;
      }
      setNotice({ state: 'busy', key: 'set.testbuild.submitting' });
      const submitted = await api.submitBuild(
        {
          package_name: form.draft.text.test_pkg.trim() || 'app-misc/jq',
          arch: 'amd64',
        },
        { projectID },
      );
      if (submitted.kind !== 'ok') {
        const detail = failureMessage(submitted);
        setNotice({ state: 'failed', key: 'set.testbuild.fail', detail });
        if (!attribute(detail)) {
          noticeRef.current?.focus();
        }
        return;
      }
      setJob({ id: submitted.value.job_id, status: submitted.value.status, error: '' });
      setNotice({
        state: 'ok',
        key: 'set.testbuild.submitted',
        detail: submitted.value.job_id.slice(0, 8),
      });
    } finally {
      runningRef.current = false;
      setBusy(false);
    }
  };

  return (
    <div className="card">
      <h3 className="card-title">{messages.t('set.testbuild')}</h3>
      <div className="card-pad">
        <p className="hint settings-lead">{messages.t('set.testbuild.hint')}</p>
        <div className="form-grid">
          <TextField form={form} control="test_pkg" labelKey="set.testbuild.pkg" />
        </div>
        <div className="form-actions">
          <button
            className="btn blue"
            type="button"
            onClick={() => {
              void start();
            }}
            aria-disabled={busy ? 'true' : 'false'}
            {...(busy ? { 'aria-busy': 'true' as const } : {})}
          >
            {messages.t('set.testbuild.go')}
          </button>
          <span
            className="save-msg settings-msg"
            data-state={notice.state}
            role="status"
            tabIndex={-1}
            ref={noticeRef}
          >
            {noticeText(messages, notice)}
          </span>
        </div>
        {/* The job line reserves its own height before a job exists: the button
            sits directly above it, and a result box that appears from nothing
            moves the pointer's target out from under it. */}
        <div className="settings-testbuild-result">
          {job === null ? null : (
            <>
              <StatusBadge token={job.status} />
              {job.error === '' ? null : <span className="sec"> {job.error.slice(0, 160)}</span>}
              <Link to={`/build/${encodeURIComponent(job.id)}`}>
                {' '}
                {messages.t('set.testbuild.view')}
              </Link>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
