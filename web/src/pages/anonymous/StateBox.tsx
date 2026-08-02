/**
 * The five states, in one box, on every anonymous page.
 *
 * `resolveState` already decided which of them a surface is in; this only
 * renders the decision, so the three public pages cannot come to three
 * different conclusions about what an empty answer means — which is what the
 * pages this replaces did, where a search matching nothing and a binhost that
 * had never published anything were the same sentence.
 *
 * The node is mounted in every state, including `ok`, and hidden rather than
 * removed. A live region that is unmounted and remounted has no history for a
 * screen reader to compare against, so the announcement that matters — the one
 * where a loaded surface becomes a failed one — is the announcement most likely
 * to be dropped. It carries state text and nothing else: no rows, no counts, no
 * timestamps, so a poll that changes nothing changes no text and says nothing.
 */

import type { ResourceState } from '../../api/state';
import { stateAttribute } from '../../api/state';
import { useMessages } from '../../i18n/context';
import type { MessageKey } from '../../i18n/messages';

export interface StateBoxProps {
  state: ResourceState<unknown>;
  /** Nothing exists yet: the sentence that says so without inviting a retry. */
  emptyKey: MessageKey;
  /** Things exist and this query matches none of them. */
  filteredKey: MessageKey;
  /** The prefix the page puts in front of the server's own sentence. */
  errorKey: MessageKey;
  /** Offered only in the error state, and outside the live region. */
  onRetry?: () => void;
}

export function StateBox({ state, emptyKey, filteredKey, errorKey, onRetry }: StateBoxProps) {
  const messages = useMessages();
  let text = '';
  switch (state.state) {
    case 'loading':
      text = messages.t('common.loading');
      break;
    case 'empty':
      text = messages.t(emptyKey);
      break;
    case 'filtered-empty':
      text = messages.t(filteredKey);
      break;
    case 'error':
      text = messages.t(errorKey) + state.message;
      break;
    case 'partial':
      text = messages.t(errorKey) + state.missing.join(', ');
      break;
    case 'ok':
      break;
  }
  const quiet = state.state === 'ok';
  return (
    <div className="state-block">
      <p className="empty" data-state={stateAttribute(state)} role="status" hidden={quiet}>
        {text}
      </p>
      {state.state === 'error' && onRetry !== undefined ? (
        <div className="state-retry">
          <button
            className="btn"
            type="button"
            onClick={() => {
              onRetry();
            }}
          >
            {messages.t('common.retry')}
          </button>
        </div>
      ) : null}
    </div>
  );
}
