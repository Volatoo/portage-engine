/**
 * One status vocabulary, three readers, one catch-all.
 *
 * The badge, the state word inside a sentence and the coloured verdict on a
 * number all resolve here, so a token cannot be spelled one way on a badge and
 * another way two lines below it — which is what happened when the colour map
 * and the label catalogue were two hand-maintained mirrors of the same backend
 * vocabulary.
 *
 * `resolveStatus` is a lookup with a documented fallback and never a conditional
 * chain over status values. A token this table has never heard of renders gray
 * with its own raw name: wrong-but-honest, rather than confidently coloured as
 * whichever branch happened to be last. That is what makes a status added
 * upstream a legible unknown rather than a silent mis-report.
 */

import type { Messages } from './messages';
import type { MessageKey } from './messages.en';
import { STATUS_VOCABULARY } from './vocabulary';
import type { StatusColor, StatusToken } from './vocabulary';

export type { StatusColor, StatusToken } from './vocabulary';
export { STATUS_VOCABULARY } from './vocabulary';

export interface StatusPresentation {
  /** The dot hue. `gray` for anything the vocabulary does not carry. */
  color: StatusColor;
  /**
   * The word beside the dot. Status is never encoded by colour alone: this
   * string is always rendered, and it is what a reader in forced-colors mode —
   * where every dot goes one colour — reads instead.
   */
  label: string;
  /** False when the catch-all produced this, so a surface can say so. */
  known: boolean;
}

function isKnown(token: string): token is StatusToken {
  return Object.prototype.hasOwnProperty.call(STATUS_VOCABULARY, token);
}

export function resolveStatus(token: string, messages: Messages): StatusPresentation {
  if (!isKnown(token)) {
    return { color: 'gray', label: token === '' ? '-' : token, known: false };
  }
  return {
    color: STATUS_VOCABULARY[token],
    label: messages.t(`status.token.${token}` as MessageKey),
    known: true,
  };
}

/**
 * Where a meter sits against its own limit.
 *
 * Level is a value, not a colour, because the stylesheet spends two channels on
 * it: `[data-level]` picks the bar's hue and appends " !" or " !!" to the
 * reading, so an operator who cannot tell orange from red still sees which
 * meters are the problem. Deciding it here rather than at each meter is what
 * stops two pages disagreeing about where "warn" starts.
 */
export type MeterLevel = 'ok' | 'warn' | 'crit';

export function meterLevel(used: number, max: number): MeterLevel {
  if (!Number.isFinite(max) || max <= 0) {
    return 'ok';
  }
  const ratio = used / max;
  if (ratio >= 1) {
    return 'crit';
  }
  return ratio >= 0.8 ? 'warn' : 'ok';
}
