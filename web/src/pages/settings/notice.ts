/**
 * What a live region on this page holds.
 *
 * A message key and its detail, never a rendered sentence. The console this
 * replaces stored the finished string, so switching language left "Saved — in
 * effect immediately" standing in English on a Chinese page until the next
 * write — and it had to re-run `fill()` on every language change to paper over
 * it. Keeping the key means the sentence re-renders in the reader's language
 * with nothing subscribing to anything.
 *
 * `detail` is the server's own prose and stays as it arrived: it is evidence,
 * not copy, and this console does not translate what a Go handler said.
 */

import type { MessageKey, Messages, PluralKey } from '../../i18n/messages';

export interface Notice {
  /** Drives `data-state` on the region, which is what the stylesheet reads. */
  readonly state: 'idle' | 'busy' | 'ok' | 'failed';
  readonly key?: MessageKey;
  /** For the one counted sentence on the page — the node count after a test. */
  readonly plural?: { key: PluralKey; count: number };
  readonly detail?: string;
}

export const IDLE: Notice = { state: 'idle' };

export function noticeText(messages: Messages, notice: Notice): string {
  if (notice.plural !== undefined) {
    return messages.plural(notice.plural.key, notice.plural.count);
  }
  if (notice.key === undefined) {
    return '';
  }
  return messages.t(notice.key) + (notice.detail ?? '');
}
