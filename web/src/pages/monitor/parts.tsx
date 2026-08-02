/**
 * The nodes /monitor and /image-factory are assembled out of.
 *
 * Four of them exist because the same defect appeared on both routes and both
 * fixes had to be the same fix: a timestamp that is not an instant, a number
 * that carries a conclusion, a status word that must never be a colour alone,
 * and the box that holds a surface's state text and nothing else. The decisions
 * behind them live in ./verdicts.ts; this file only renders them.
 *
 * They sit under monitor/ because that is the surface with thirty of the forty
 * timestamp sites on it; /image-factory imports them across. Promoting them to
 * a shared components layer is a handoff, not a page's decision to take.
 */

import type { ReactNode } from 'react';

import type { ResourceState } from '../../api/state';
import { useMessages } from '../../i18n/context';
import type { MessageKey } from '../../i18n/messages';
import { resolveStatus } from '../../i18n/status';
import { momentText, stateSentence, verdictAttributes } from './verdicts';

/** A timestamp, or the translated word for a thing that has never happened. */
export function Moment({
  value,
  absent,
}: {
  value: string | number | Date | null | undefined;
  absent?: MessageKey;
}) {
  const messages = useMessages();
  return <>{momentText(messages, value, absent)}</>;
}

/**
 * The status badge is shared. `label` renames a known token's word and never
 * its hue, which is what lets the ledger card call its healthy state
 * "consistent" without being able to disagree with the token it was judged by.
 */
export { StatusBadge } from '../../components/StatusBadge';

/**
 * A labelled count inside a card's meta row.
 *
 * The counter is its own element with a minimum width because tabular figures
 * equalise the WIDTH of a digit and not the COUNT of them: 9 becoming 10 moved
 * everything after it on every poll. `token` is the second thing this carries —
 * a count that answers a question ("did any write fail?") is a conclusion and
 * takes the status ink, which is the ink the cost figure beside it does not get,
 * because a cost concludes nothing.
 */
export function MetaCount({
  labelKey,
  value,
  token,
}: {
  labelKey: MessageKey;
  value: number;
  token?: string;
}) {
  const messages = useMessages();
  if (token === undefined) {
    return (
      <span>
        {messages.t(labelKey)}
        <span className="counter">{messages.number(value)}</span>
      </span>
    );
  }
  const verdict = verdictAttributes(messages, token);
  return (
    <span>
      {messages.t(labelKey)}
      <span className={`counter ${verdict.className}`} data-verdict={verdict['data-verdict']}>
        {messages.number(value)}
      </span>
    </span>
  );
}

/** A meta-row line whose value is a conclusion rather than a statistic. */
export function MetaVerdict({
  labelKey,
  token,
  reading,
}: {
  labelKey: MessageKey;
  token: string;
  reading?: string;
}) {
  const messages = useMessages();
  const verdict = verdictAttributes(messages, token);
  return (
    <span>
      {messages.t(labelKey)}
      {reading === undefined ? null : `${reading} · `}
      <span className={verdict.className} data-verdict={verdict['data-verdict']}>
        {resolveStatus(token, messages).label}
      </span>
    </span>
  );
}

/**
 * A section of the page: its async content, then the node that carries its
 * state.
 *
 * The live region is a node of its own that exists to hold state text and holds
 * nothing else. The console this replaces stamped `role="status"` on a container
 * that also held the rendered component list, so every 30-second poll re-read
 * the whole list to a screen reader, forever. It is also why the node is
 * present in every state rather than mounted with the sentence: a live region
 * that arrives at the same moment as its content announces the arrival of the
 * region, not the change.
 *
 * The min-height is on this wrapper and not on the grid inside it, because both
 * children take space and they take it at different times — the sentence while
 * the payload is out, the cards afterwards. Reserving the pair is what makes the
 * swap free, and `reserve` is what says whether there is a swap to make.
 */
export function Block({
  state,
  emptyKey,
  reserve = 'payload',
  children,
}: {
  state: ResourceState<unknown>;
  /**
   * Absent for a section whose payload is one status document rather than a
   * list: `resolveState` cannot produce `empty` without a count, so there is no
   * empty sentence to write and inventing one would put a list's words under a
   * card.
   */
  emptyKey?: MessageKey;
  /**
   * Whether the block holds the height its payload will take.
   *
   * Declared rather than assumed, because the two routes use this node for two
   * different jobs. /monitor wraps a grid that lands in it, so the reserve is
   * what makes the swap from sentence to cards free. /image-factory renders its
   * lists itself and passes no children at all, using this only as the carrier
   * for the state sentence — and for that caller the reserve is not a floor
   * under something arriving, it is empty space in every state including ok.
   */
  reserve?: 'payload' | 'none';
  children: ReactNode;
}) {
  const messages = useMessages();
  const sentence = stateSentence(messages, state, emptyKey);
  return (
    <div className="mon-block" data-state={state.state} data-reserve={reserve}>
      {children}
      <div className="mon-live" role="status" aria-live="polite">
        {sentence === null ? null : (
          <div className="empty" data-state={state.state}>
            {sentence}
          </div>
        )}
      </div>
    </div>
  );
}
