/**
 * The public service status page.
 *
 * The defect this page exists to not repeat is an accessibility one and it was
 * permanent. `role="status"` and `aria-live="polite"` were stamped on the
 * container that also held every component row, and that container was rebuilt
 * from scratch on a 30-second poll — so a screen-reader user had the entire
 * component list read to them every 30 seconds, forever, on the happy path,
 * with nothing having changed. A live region is a node that exists to carry
 * state text; here that is one sentence, and it is a sibling of the rows rather
 * than their parent. React only touches the DOM when the text differs, so a
 * poll that finds the same state announces nothing at all.
 *
 * The second defect was a vocabulary one. The page carried its own `statusLabel`
 * and `statusIndicator`, a conditional chain whose final branch was red plus the
 * word "Degraded" — so `operational` and `degraded` were handled and anything
 * else, including a state added upstream after this page was written, was
 * reported to the public as a fault. Both the badge and the headline are lookups
 * now, each with a catch-all that says what it does not know.
 *
 * And the meta line said "Version dev" with a clock reading in no stated zone.
 * `dev` is the linker's default, not a release anyone can pin an incident to,
 * and a public status page is read from timezones that are not the server's.
 */

import { useCallback, useEffect, useState } from 'react';

import type { ApiOutcome } from '../../api/client';
import { api } from '../../api/endpoints';
import { resolveState } from '../../api/state';
import { pollWhenVisible } from '../../api/stream';
import type { PublicServiceStatus } from '../../api/types';
import { useMessages } from '../../i18n/context';
import type { MessageKey, Messages } from '../../i18n/messages';
import { resolveStatus } from '../../i18n/status';
import { StateBox } from '../anonymous/StateBox';
import { StatusBadge } from '../anonymous/StatusBadge';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import '../../styles/pages/status.css';

/** The interval the page's own copy promises, spelled once. */
const POLL_INTERVAL_MS = 30_000;

/**
 * The overall state, as a sentence.
 *
 * A lookup and not a chain, for the same reason the status vocabulary is one:
 * the chain this replaces answered every token it had not been told about with
 * "Status service unavailable", which is a claim about this page's own
 * connection and not about the platform. A token with no sentence here falls
 * back to the vocabulary's own resolution of it — gray, with the raw token —
 * which is legibly unknown rather than confidently wrong.
 */
const OVERALL_SENTENCE: Record<string, MessageKey> = {
  operational: 'status.operational',
  degraded: 'status.degraded',
};

/**
 * The component names, which are display strings and not ids.
 *
 * handlers_public.go emits `Web and API`, `Package repository` and `Build
 * service` as literals with no stable key beside them, so the string is the
 * key. The catch-all renders a component added upstream under its own English
 * name, which is the only honest answer: a name this table has never seen is a
 * name nobody has translated.
 */
const COMPONENT_LABEL: Record<string, MessageKey> = {
  'Web and API': 'status.component.api',
  'Package repository': 'status.component.repository',
  'Build service': 'status.component.build',
};

function componentLabel(name: string, messages: Messages): string {
  const key = COMPONENT_LABEL[name];
  return key === undefined ? name : messages.t(key);
}

/**
 * The instant, with the zone named.
 *
 * `dateStyle`/`timeStyle` cannot be combined with `timeZoneName`, so the
 * components are spelled out. The locale is the app's, explicitly: a bare
 * `toLocaleString` formats to the operating system's locale, which is how a
 * reader who chose English on a zh-CN machine used to get one date format in
 * the console and another on this page of the same deployment.
 */
function updatedAt(value: string, messages: Messages): string | null {
  return messages.dateTime(value, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    timeZoneName: 'short',
  });
}

/**
 * The build identity.
 *
 * `dev` is what the linker leaves behind when no version was stamped, so
 * printing "Version dev" tells an anonymous reader that a release exists and
 * names it wrongly. Naming it as a development build says the same thing
 * truthfully; a real version is still printed verbatim, because guessing at one
 * is worse than printing it.
 */
function versionText(version: string, messages: Messages): string {
  if (version === '') {
    return '';
  }
  return version === 'dev'
    ? messages.t('status.version.dev')
    : messages.t('status.version') + version;
}

export function StatusPage() {
  const messages = useMessages();
  useDocumentTitle('title.status');

  const [outcome, setOutcome] = useState<ApiOutcome<PublicServiceStatus> | null>(null);

  const load = useCallback(() => {
    void api.publicStatus().then(setOutcome);
  }, []);

  useEffect(() => {
    load();
    // Never while the tab is hidden. The page this replaces polled a
    // backgrounded tab for as long as it was open, and the first tick after the
    // tab comes back is immediate here, so returning to it never shows a
    // reading a whole interval out of date.
    return pollWhenVisible(load, POLL_INTERVAL_MS);
  }, [load]);

  const payload = outcome?.kind === 'ok' ? outcome.value : null;
  const components = payload?.components ?? [];
  const resolved = resolveState<PublicServiceStatus>({ outcome, count: components.length });

  const overall = payload === null ? null : resolveStatus(payload.status, messages);
  const sentenceKey = payload === null ? undefined : OVERALL_SENTENCE[payload.status];
  // The headline is the state, and the whole of the state: no timestamp, no
  // version, no component names. Anything in here that changes on a poll is
  // something a screen reader is told about on every poll.
  //
  // Three sources, in order: the platform's own answer, then this page's
  // inability to get one, then the first load. "Status service unavailable" is
  // a statement about this page's connection, so it is said only when the
  // request actually failed — never as the catch-all for a token the platform
  // reported and this console has not been told about.
  let headline: string;
  if (overall !== null) {
    headline = sentenceKey === undefined ? overall.label : messages.t(sentenceKey);
  } else if (outcome === null) {
    headline = messages.t('status.loading');
  } else {
    headline = messages.t('status.unavailable');
  }

  const stamp = payload === null ? null : updatedAt(payload.updated_at, messages);
  const version = payload === null ? '' : versionText(payload.version, messages);

  return (
    <>
      <div className="public-head">
        <h1>{messages.t('status.h1')}</h1>
        <p>{messages.t('status.sub')}</p>
      </div>

      <div className="card status-page">
        <div className="status-overall">
          <div className="status-overall-state">
            <p className="status-headline" role="status">
              {/* The dot is decoration: the sentence beside it is the status
                  word, so a reader in forced-colors mode — where every dot goes
                  one colour — reads exactly the same fact. */}
              <span
                className={`status ${overall === null ? 'gray' : overall.color}`}
                aria-hidden="true"
              >
                <span className="dot" />
              </span>
              <span>{headline}</span>
            </p>
            {/* Outside the live region on purpose. This line changes on every
                single poll — that is what a timestamp is — and a live region
                containing it is a live region that speaks every 30 seconds. */}
            <p className="status-meta">
              {stamp === null ? null : (
                <>
                  {messages.t('status.updated')}
                  <time dateTime={payload?.updated_at}>{stamp}</time>
                </>
              )}
              {version === '' ? null : <span className="status-version"> · {version}</span>}
            </p>
          </div>
          <span className="status-cadence">{messages.t('status.refresh')}</span>
        </div>
        <div className="status-list">
          {components.map((component) => (
            <div className="status-row" key={component.name}>
              <span>{componentLabel(component.name, messages)}</span>
              <StatusBadge token={component.status} />
            </div>
          ))}
          <StateBox
            state={resolved}
            emptyKey="status.components.empty"
            filteredKey="status.components.empty"
            errorKey="common.loadfail"
            onRetry={load}
          />
        </div>
      </div>
    </>
  );
}
