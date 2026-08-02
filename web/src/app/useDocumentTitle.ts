/**
 * The document title, from the route table's own key.
 *
 * Separate from the heading on purpose: a title carries the product name and a
 * heading does not, and taking one for the other is what put
 * "总览 — Portage Engine" in an h1 the first time this shell rendered.
 *
 * It lives beside the route table rather than under a page because the tab is
 * chrome, not content, and because the twelve routes were ported in parallel and
 * arrived with five spellings of these three lines: two byte-identical hooks in
 * two page directories, three pages inlining the same effect, and /packages
 * setting no title at all — so a reader who opened it from /status kept reading
 * "Status — Portage Engine" in the tab and in their history. One hook is what
 * makes the route's `titleKey` the thing that decides, rather than whether the
 * page that landed last remembered to.
 *
 * The effect depends on the translated string and not on the key, so a language
 * switch retitles the tab without the page re-mounting.
 */

import { useEffect } from 'react';

import { useMessages } from '../i18n/context';
import type { MessageKey } from '../i18n/messages';

export function useDocumentTitle(key: MessageKey): void {
  const messages = useMessages();
  const title = messages.t(key);
  useEffect(() => {
    document.title = title;
  }, [title]);
}
