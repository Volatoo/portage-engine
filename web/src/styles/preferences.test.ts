import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { build } from 'vite';
import { describe, expect, it } from 'vitest';

/**
 * The two reader-preference blocks, read off the bytes that ship.
 *
 * They are the only rules in the project that must beat a component outright,
 * and the reason they did not is a thing no source file can see. index.css
 * imports preferences.css last, which is what the source-order check in
 * tokens.test.ts asserts and all it can assert — but a page sheet is imported by
 * its page component, so all nine of them are emitted AFTER the whole shared
 * layer. In the bundle this file was written against, thirty-three kilobytes of
 * page rules follow preferences.css, and one of them,
 * `.settings-msg:focus-visible`, was already beating the forced-colors ring: a
 * reader in forced-colors mode got the outline in the text colour instead of the
 * system highlight, which is the exact substitution the block exists to prevent.
 *
 * So the invariant is a priority and not a position, and it is checked here
 * against the emitted stylesheet rather than against an import list.
 */

const STYLES = dirname(fileURLToPath(import.meta.url));
const ROOT = join(STYLES, '../..');

/** This case runs a production build, so the default five seconds is not it. */
const BUILD_BUDGET = 60_000;

async function emittedCSS(): Promise<string> {
  const result = await build({
    root: ROOT,
    logLevel: 'silent',
    // In memory: this must not touch the tree `go:embed` compiles in.
    // Unminified, because the blocks below are located by their own preludes.
    build: { write: false, cssMinify: false, sourcemap: false },
  });
  const bundles = Array.isArray(result) ? result : [result];
  const assets = bundles.flatMap((bundle) =>
    'output' in bundle ? bundle.output.filter((chunk) => chunk.fileName.endsWith('.css')) : [],
  );
  expect(assets.length, 'the build emitted no stylesheet').toBe(1);
  const asset = assets[0];
  const source = asset !== undefined && asset.type === 'asset' ? String(asset.source) : '';
  // Comments go first: the annotations explaining these blocks name the same
  // at-rules the blocks are found by, and a brace inside one would close a
  // block that is still open.
  return source.replace(/\/\*[\s\S]*?\*\//g, '');
}

/** The first block opened by `opener`, brace-balanced, prelude included. */
function block(css: string, opener: RegExp): string {
  const start = css.search(opener);
  expect(start, `${String(opener)} is not in the emitted stylesheet`).toBeGreaterThanOrEqual(0);
  let depth = 0;
  for (let index = start; index < css.length; index += 1) {
    const character = css[index];
    if (character === '{') {
      depth += 1;
    } else if (character === '}') {
      depth -= 1;
      if (depth === 0) {
        return css.slice(start, index + 1);
      }
    }
  }
  throw new Error(`${String(opener)} is never closed`);
}

/**
 * Every declaration in a run of CSS, read out of rule bodies only.
 *
 * `[^{}]` is what keeps it to bodies: it can only match a brace pair with no
 * brace between, which is the innermost rule and never an at-rule wrapping one.
 * Reading preludes as well would count `.field input:focus` — a selector, in the
 * forced-colors prelude — as a declaration of `input`.
 */
function declarations(css: string): string[] {
  return [...css.matchAll(/\{([^{}]*)\}/g)].flatMap((rule) =>
    (rule[1] ?? '')
      .split(';')
      .map((declaration) => declaration.replace(/\s+/g, ' ').trim())
      .filter((declaration) => declaration !== ''),
  );
}

const REDUCED = /@media\s*\(prefers-reduced-motion:\s*reduce\)/;
const FORCED = /@media\s*\(forced-colors:\s*active\)/;

describe('the reader-preference blocks in the emitted stylesheet', () => {
  it(
    'state !important on every declaration, which is what outranks a page rule',
    async () => {
      const emitted = await emittedCSS();
      for (const [name, opener] of [
        ['prefers-reduced-motion: reduce', REDUCED],
        ['forced-colors: active', FORCED],
      ] as const) {
        const found = declarations(block(emitted, opener));
        expect(found.length, `${name} declares nothing`).toBeGreaterThan(0);
        expect(
          found.filter((declaration) => !declaration.includes('!important')),
          `${name} leaves these for a page sheet to overturn`,
        ).toEqual([]);
      }
    },
    BUILD_BUDGET,
  );

  it(
    'are the only rules that claim that priority, apart from the hidden attribute',
    async () => {
      // `!important` is not a tool this project reaches for: every other
      // precedence question in the bundle is settled by specificity or by the
      // order index.css declares. Two exceptions, both stated here so a third
      // has to be argued for rather than merely written.
      //
      // `[hidden]` is the second, and it is not a preference: the settings
      // panels and the monitor cards are shown and hidden by the attribute a
      // screen reader reads, so a component that sets `display` on the same
      // element must not be able to render a hidden panel visible.
      const emitted = await emittedCSS();
      const preference = new Set([
        ...declarations(block(emitted, REDUCED)),
        ...declarations(block(emitted, FORCED)),
      ]);
      const stray = declarations(emitted)
        .filter((declaration) => declaration.includes('!important'))
        .filter((declaration) => !preference.has(declaration))
        .filter((declaration) => !/^display:\s*none !important$/.test(declaration));
      expect(
        stray,
        'a component is claiming a priority only a reader preference may claim',
      ).toEqual([]);
    },
    BUILD_BUDGET,
  );

  it(
    'ship after the shared rule they restate, which the import order still decides',
    async () => {
      // The half index.css does control. `.field input:focus` in surfaces.css is
      // (0,1,1) and so is the forced-colors restatement of it, so within the
      // shared layer the restatement wins by arriving second — the !important
      // above is what carries it past the page layer, not past this.
      const emitted = await emittedCSS();
      const restated = emitted.search(FORCED);
      const declared = emitted.indexOf('.field input:focus');
      expect(declared).toBeGreaterThanOrEqual(0);
      expect(declared).toBeLessThan(restated);
    },
    BUILD_BUDGET,
  );
});
