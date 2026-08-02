import { useEffect } from 'react';
import { useParams } from 'react-router';

import { resolveState, stateAttribute } from '../api/state';
import { useMessages } from '../i18n/context';
import type { ConsoleRoute } from './routes';

/**
 * What every route renders until stage three ports its page.
 *
 * It is not a blank div, and that is deliberate: the whole point of this stage
 * is that the shell, the language, the chrome and the states are provable in a
 * browser before a single page exists. So this renders the page head the real
 * page will have, the parameter the URL carried, and the loading member of the
 * five states through the same `resolveState` a real page will call — which
 * means the two-width, two-theme, longest-locale sweep can be run now rather
 * than after fourteen pages have each invented their own layout.
 */
export function PlaceholderPage({ route }: { route: ConsoleRoute }) {
  const messages = useMessages();
  const params = useParams();

  // The document title is set after mount, which costs a beat of the product
  // name in the tab. The other option — a second title catalogue in Go, stamped
  // into the shell — would put the same fourteen strings in two places with
  // nothing checking they agree, which is the failure the one-catalogue rule
  // exists for. Worth revisiting if the beat proves visible.
  //
  // The title carries the product name and the heading does not, and they are
  // two different keys for that reason: the first render of this page put
  // "总览 — Portage Engine" in an h1 because it took one for the other.
  const title = messages.t(route.titleKey);
  useEffect(() => {
    document.title = title;
  }, [title]);
  const captured = Object.entries(params).filter(([, value]) => value !== undefined);
  // The state a page is in before its first fetch resolves. Named through the
  // shared resolver so this placeholder cannot drift from the real thing.
  const state = resolveState({ outcome: null });

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{messages.t(route.headingKey)}</h1>
          {captured.length > 0 ? (
            <p className="sub mono">{captured.map(([, value]) => value).join(' · ')}</p>
          ) : null}
        </div>
      </div>
      <div className="card">
        <div className="card-pad">
          <div
            className="empty"
            data-state={stateAttribute(state)}
            role="status"
            aria-live="polite"
          >
            {messages.t('common.loading')}
          </div>
        </div>
      </div>
    </>
  );
}
