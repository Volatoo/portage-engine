import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { build } from 'vite';
import { describe, expect, it } from 'vitest';

/**
 * What order the emitted stylesheet is in, read off the emitted stylesheet.
 *
 * Nothing in a source file can state this. A page sheet is imported by its page
 * component, so where its rules land in the bundle is decided by module
 * evaluation order in main.tsx — and with `./app/App` imported first, the route
 * table pulled all fourteen pages and every `styles/pages/*.css` with them, so
 * the page layer was emitted BEFORE the layer it is written on top of. Every
 * equal-specificity tie then went the wrong way, silently: `.meta-value` lost to
 * `.stat-tile .num` and the build-detail metadata tiles rendered at the headline
 * rung, which is where a formatted timestamp wraps inside its tile.
 *
 * So this builds the bundle and reads the bytes. Unminified, because the check
 * needs to find each source sheet in the output and minification is free to drop
 * the whitespace the anchors below are matched on; minification does not reorder
 * rules, so the order asserted here is the order that ships.
 */

const STYLES = dirname(fileURLToPath(import.meta.url));
const ROOT = join(STYLES, '../..');
const PAGES = join(STYLES, 'pages');

/** The base layer, in the sequence index.css imports it. */
const BASE_SHEETS = readFileSync(join(STYLES, 'index.css'), 'utf8')
  .replace(/\/\*[\s\S]*?\*\//g, '')
  .split('\n')
  .flatMap((line) => /@import url\('\.\/([\w.-]+)'\)/.exec(line)?.[1] ?? []);

const PAGE_SHEETS = readdirSync(PAGES).filter((name) => name.endsWith('.css'));

/** This case runs a production build, so the default five seconds is not it. */
const BUILD_BUDGET = 60_000;

function source(path: string): string {
  // Comments go first: a brace or a selector-shaped phrase inside an annotation
  // would otherwise be read as a rule.
  return readFileSync(path, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
}

/**
 * Every prelude in a sheet, one selector per entry, grouped ones split up.
 *
 * At-rule preludes come back too, and are worth having: tokens.css declares
 * nothing but `:root`, five times, so its `@supports` line is the only text in
 * it that can say where the file is.
 *
 * A newline counts as a boundary alongside the braces and the semicolon, because
 * the first rule inside an at-rule block is preceded by neither — and reading
 * only from a brace or a semicolon skipped exactly the rules a media query holds,
 * which is where a page sheet's narrow-viewport overrides live.
 */
function preludes(css: string): string[] {
  return [...css.matchAll(/(^|[{};\n])([^{};]+)\{/g)].flatMap((match) =>
    (match[2] ?? '')
      .split(',')
      .map((part) => part.trim())
      .filter((part) => part !== ''),
  );
}

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/**
 * Where a sheet sits in the emitted CSS, as [first byte, last byte].
 *
 * A selector is only usable as an anchor if it occurs once in the whole output:
 * `.card` and `:root` are declared by several sheets, and matching the wrong one
 * would place a file where it is not. Anchors are matched at the start of a line
 * because that is where a prelude sits in unminified output, which also keeps
 * `a` from matching the tail of `.media`.
 */
function span(emitted: string, path: string, name: string): [number, number] {
  const found: number[] = [];
  for (const selector of new Set(preludes(source(path)))) {
    const pattern = new RegExp(`(^|\n)[ \t]*${escapeRegExp(selector)}[ \t]*[,{]`, 'g');
    const hits = [...emitted.matchAll(pattern)];
    if (hits.length === 1) {
      found.push(hits[0]?.index ?? 0);
    }
  }
  expect(
    found.length,
    `${name} has no selector unique enough to locate it in the bundle`,
  ).toBeGreaterThan(0);
  return [Math.min(...found), Math.max(...found)];
}

async function emittedCSS(): Promise<string> {
  const result = await build({
    root: ROOT,
    logLevel: 'silent',
    // In memory: this must not touch the tree `go:embed` compiles in.
    build: { write: false, cssMinify: false, sourcemap: false },
  });
  const bundles = Array.isArray(result) ? result : [result];
  const assets = bundles.flatMap((bundle) =>
    'output' in bundle ? bundle.output.filter((chunk) => chunk.fileName.endsWith('.css')) : [],
  );
  expect(assets.length, 'the build emitted no stylesheet').toBe(1);
  const asset = assets[0];
  return asset !== undefined && asset.type === 'asset' ? String(asset.source) : '';
}

describe('the emitted stylesheet', () => {
  it(
    'puts every base sheet before every page sheet, which is the only reason a page rule can override one',
    async () => {
      const emitted = await emittedCSS();
      const base = BASE_SHEETS.map((name) => ({
        name,
        at: span(emitted, join(STYLES, name), name),
      }));
      const pages = PAGE_SHEETS.map((name) => ({
        name: `pages/${name}`,
        at: span(emitted, join(PAGES, name), name),
      }));
      expect(base.length).toBeGreaterThan(0);
      expect(pages.length).toBeGreaterThan(0);

      const last = base.reduce((worst, sheet) => (sheet.at[1] > worst.at[1] ? sheet : worst));
      const first = pages.reduce((best, sheet) => (sheet.at[0] < best.at[0] ? sheet : best));
      expect(
        last.at[1],
        `${last.name} is emitted after ${first.name}; main.tsx has to import ./styles/index.css before anything that reaches a page component`,
      ).toBeLessThan(first.at[0]);
    },
    BUILD_BUDGET,
  );
});

describe('a page rule that overrides a shared one', () => {
  it('wins by specificity, so it does not stop winning when an import moves', () => {
    // The two sites that restate `.stat-tile .num`, both of which the Go markup
    // set with an inline `style` attribute that beat it without naming it.
    expect(source(join(PAGES, 'builds.css'))).toContain('.stat-tile .num.meta-value');
    expect(source(join(PAGES, 'settings.css'))).toContain('.stat-tile .num.settings-gpg-state');
    // And neither is declared bare, which is the (0,1,0) form that loses.
    for (const name of ['builds.css', 'settings.css']) {
      expect(preludes(source(join(PAGES, name)))).not.toContain('.meta-value');
      expect(preludes(source(join(PAGES, name)))).not.toContain('.settings-gpg-state');
    }
  });

  it('reserves a block height only where a payload is coming, and says which by attribute', () => {
    const monitor = source(join(PAGES, 'monitor.css'));
    // /image-factory borrows .mon-block as a bare state carrier, so an
    // unqualified floor there is dead space in every state. The opt-out is on
    // the reserving rules and not on a later override: `.card > .mon-block` is
    // (0,2,0) and a plain `.mon-block[data-reserve='none']` would tie with it.
    const reserving = [...monitor.matchAll(/([^{}]+)\{([^{}]*)\}/g)].filter(
      (rule) => (rule[1] ?? '').includes('.mon-block') && (rule[2] ?? '').includes('min-height'),
    );
    expect(reserving.length, 'nothing reserves a block height any more').toBeGreaterThan(0);
    for (const rule of reserving) {
      expect(
        rule[1]?.trim(),
        'this floor is held for every caller, including the ones with no payload to hold it for',
      ).toContain("[data-reserve='payload']");
    }
  });
});

describe('every page stylesheet', () => {
  /**
   * routes.ts imports all fourteen page components statically, so there is one
   * always-loaded CSS bundle and "page-scoped" is a property of the selector or
   * of nothing. `table.list td .status` was written for one cell on
   * /image-factory and set a start margin on every status badge in every table
   * on the console — /builds, /overview, the /monitor instances table, the
   * /packages Signature column, the settings session and node tables.
   *
   * A page sheet names its page by carrying a class no other page sheet claims,
   * anywhere in the selector — as an ancestor (`.factory-profile-id .status`) or
   * on the key compound (`.num.meta-value`); an id does the same job. This
   * cannot see whether a class is page-exclusive in the MARKUP, only that no
   * other page's stylesheet reaches for it, which is the part that makes one
   * page's rule land on another page.
   */
  const claims = new Map<string, Set<string>>();
  for (const name of PAGE_SHEETS) {
    for (const prelude of preludes(source(join(PAGES, name)))) {
      for (const match of prelude.matchAll(/\.([\w-]+)/g)) {
        const owners = claims.get(match[1] ?? '') ?? new Set<string>();
        owners.add(name);
        claims.set(match[1] ?? '', owners);
      }
    }
  }

  for (const name of PAGE_SHEETS) {
    it(`${name} says which page it is in every selector`, () => {
      for (const prelude of preludes(source(join(PAGES, name)))) {
        if (prelude.startsWith('@') || prelude.includes('#')) {
          continue;
        }
        const own = [...prelude.matchAll(/\.([\w-]+)/g)].filter(
          (match) => claims.get(match[1] ?? '')?.size === 1,
        );
        expect(
          own.length,
          `${name}: \`${prelude}\` is written out of vocabulary every page shares, so it matches on every page`,
        ).toBeGreaterThan(0);
      }
    });
  }
});
