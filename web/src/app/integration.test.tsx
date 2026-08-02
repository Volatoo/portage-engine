import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { MessagesProvider } from '../i18n/context';
import { CLOCK_READING } from '../i18n/format';
import { messagesFor } from '../i18n/messages';
import { StateBox } from '../pages/builds/parts';
import { CONSOLE_ROUTES } from './routes';

/**
 * What twelve routes ported in parallel could not check about themselves.
 *
 * Every case here is a disagreement between two pages that each looked right on
 * its own: a shared helper two groups read different defaults out of, one
 * three-line hook spelled five ways, a rule two surfaces kept and a third did
 * not, and a byte that made a whole file invisible to the checks above. None of
 * them is reachable from a single page's test file, which is why they survived
 * five green suites and are pinned from here instead.
 */

const SOURCE_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');

function sourceFiles(directory: string): string[] {
  const found: string[] = [];
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      found.push(...sourceFiles(path));
    } else if (/\.(tsx?|css)$/.test(entry.name)) {
      found.push(path);
    }
  }
  return found;
}

describe('the route table is fully ported', () => {
  it('names a real component for every route, so no path answers with a placeholder', () => {
    const unported = CONSOLE_ROUTES.filter((route) => route.component === undefined).map(
      (route) => route.name,
    );
    expect(unported).toEqual([]);
  });

  it('gives every route a title key, which is the only thing that names the tab', () => {
    // /packages reached the merge with a title key nothing consumed. The table
    // is what the router hands a page and the hook is what spends it, so the
    // check that matters is that both halves exist for all fourteen.
    for (const route of CONSOLE_ROUTES) {
      expect(route.titleKey, route.name).toBeTruthy();
      expect(route.headingKey, route.name).toBeTruthy();
      expect(route.titleKey, route.name).not.toBe(route.headingKey);
    }
  });

  it('sets the document title from exactly one hook', () => {
    // Five spellings of three lines arrived from five agents: two byte-identical
    // hooks in two page directories and three pages inlining the effect. A page
    // assigning document.title itself is how one of them ends up not doing it.
    const offenders = sourceFiles(SOURCE_ROOT)
      .filter((path) => path.includes(`${join('src', 'pages')}`) || path.includes('/pages/'))
      .filter((path) => !path.endsWith('.test.tsx') && !path.endsWith('.test.ts'))
      .filter((path) => /\bdocument\.title\s*=/.test(readFileSync(path, 'utf8')))
      .map((path) => path.slice(SOURCE_ROOT.length + 1));
    expect(offenders).toEqual([]);
  });
});

describe('a clock reading is a clock reading on every route', () => {
  /**
   * `new Intl.DateTimeFormat(locale)` with no options is a DATE. The
   * `toLocaleString()` in ui.go's fmtTime that this port replaces was date AND
   * time, so the four pages that called `messages.dateTime(value)` bare were not
   * formatting differently — they were dropping the time of day. /builds printed
   * the same `8/1/2026` in Created and Updated for jobs queued minutes apart.
   */
  it('carries a time of day when no options are given', () => {
    const when = new Date('2026-08-01T14:37:09Z');
    for (const lang of ['en', 'zh'] as const) {
      const bare = messagesFor(lang).dateTime(when);
      expect(bare, lang).not.toBeNull();
      // Two colon-separated groups is a clock and a date has none.
      expect(bare ?? '', lang).toMatch(/\d:\d\d/);
    }
  });

  it('formats a bare call and an explicit clock reading identically', () => {
    const when = new Date('2026-08-01T14:37:09Z');
    const messages = messagesFor('en');
    expect(messages.dateTime(when)).toBe(messages.dateTime(when, CLOCK_READING));
  });

  it('still refuses Go\u2019s zero time rather than formatting it', () => {
    // The default must not have reintroduced a reading for a thing that never
    // happened: a zero time.Time formats to a plausible clock face otherwise.
    expect(messagesFor('en').dateTime('0001-01-01T00:00:00Z')).toBeNull();
  });

  it('keeps both languages on their own locale, which is the defect underneath', () => {
    const when = new Date('2026-08-01T14:37:09Z');
    expect(messagesFor('en').dateTime(when)).not.toBe(messagesFor('zh').dateTime(when));
  });
});

describe('a live region carries state text and no controls', () => {
  it('leaves Retry outside the region that announces the state', () => {
    const markup = renderToStaticMarkup(
      <MessagesProvider lang="en">
        <StateBox
          state={{ state: 'error', message: 'build job not found', status: 404 }}
          emptyKey="detail.norecord"
          onRetry={() => undefined}
        />
      </MessagesProvider>,
    );
    // The control is still offered.
    expect(markup).toContain('Retry');
    // …and it is not inside the announced node: everything between the region's
    // opening tag and its close is sentences.
    const region = /<div class="empty"[^>]*role="status"[^>]*>([\s\S]*?)<\/div>/.exec(markup);
    expect(region).not.toBeNull();
    expect(region?.[1] ?? '').not.toContain('<button');
    expect(region?.[1] ?? '').toContain('build job not found');
  });

  it('stamps the live region on a node that renders no page content anywhere', () => {
    // The defect this replaces: /status carried role="status" on the container
    // holding its component list, so a 30-second poll re-read the whole list to
    // a screen reader, forever. A region whose subtree contains a table, a list
    // or a card is that defect again under a different class name.
    const carriers = /<(?:div|p|span|section)\b[^>]*(?:role="status"|aria-live)[^>]*>/;
    for (const path of sourceFiles(SOURCE_ROOT)) {
      if (!path.endsWith('.tsx') || path.includes('.test.')) {
        continue;
      }
      const text = readFileSync(path, 'utf8');
      if (!carriers.test(text)) {
        continue;
      }
      // A region is declared on a leaf in this codebase. The structural tags a
      // page's own content is built from must never appear as its children, and
      // the cheap version of that check is that no file declares a region on the
      // same element that opens one of them.
      expect(
        /(role="status"|aria-live)[^>]*>\s*<(table|tbody|ul|ol|article)/.test(text),
        path,
      ).toBe(false);
    }
  });
});

describe('the source tree is readable by the tools that check it', () => {
  /**
   * PackagesPage.tsx carried a literal NUL byte as a React key separator. `file`
   * called it data and `grep -rn` skipped it in silence, so every sweep over
   * src/ — the innerHTML sweep, the bare-toLocale sweep, the live-region sweep —
   * had a blind spot on a whole route and reported clean. Git missed it too, by
   * luck: its binary probe reads the first 8000 bytes and the NUL sat at 13037.
   */
  it('holds no control byte that makes a file non-text', () => {
    const offenders: string[] = [];
    for (const path of sourceFiles(SOURCE_ROOT)) {
      const raw = readFileSync(path);
      for (const byte of raw) {
        if ((byte < 0x09 || (byte > 0x0d && byte < 0x20) || byte === 0x7f) && byte !== 0x0a) {
          offenders.push(`${path.slice(SOURCE_ROOT.length + 1)}: 0x${byte.toString(16)}`);
          break;
        }
      }
    }
    expect(offenders).toEqual([]);
  });
});
