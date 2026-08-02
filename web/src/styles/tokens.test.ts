import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import { DARK_BLOCK } from './modeblocks';

/**
 * These read the stylesheets as text on purpose. stylelint enforces that a
 * component names a token instead of a value; nothing in stylelint can say that
 * the dark block redeclares a token that exists, or that the focus ring is still
 * an outline. Both of those are load-bearing and both fail silently: a typo in a
 * dark token name is valid CSS that does nothing, and a ring rewritten as a
 * box-shadow disappears entirely under forced colors.
 */

function read(name: string): string {
  // Path arithmetic on the file path, not on a URL: under jsdom `URL` resolves a
  // relative reference against the document base rather than against the base
  // argument, so the result comes back pointing at the dev server.
  const path = join(dirname(fileURLToPath(import.meta.url)), name);
  // Comments are stripped before any brace counting: a brace inside an
  // annotation would otherwise close a block that is still open.
  return readFileSync(path, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
}

/** The first block opened by `opener`, brace-balanced. */
function block(css: string, opener: string | RegExp): string {
  const start = typeof opener === 'string' ? css.indexOf(opener) : css.search(opener);
  expect(start, `${String(opener)} is missing`).toBeGreaterThanOrEqual(0);
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
  throw new Error(`${opener} is never closed`);
}

function declaredTokens(css: string): string[] {
  return [...css.matchAll(/(--[\w-]+)\s*:/g)].map((match) => match[1] ?? '');
}

const tokens = read('./tokens.css');
const base = read('./base.css');
// The two reader-preference queries close the sheet rather than the base file:
// the forced-colors block restates a selector surfaces.css also declares, at the
// same specificity, so it wins only by arriving last. index.css imports it last.
const preferences = read('./preferences.css');

describe('token foundation', () => {
  it('declares every dark token in the light block, so a dark typo cannot be silent', () => {
    const light = new Set(declaredTokens(block(tokens, ':root {')));
    const dark = declaredTokens(block(tokens, DARK_BLOCK));
    expect(dark.length).toBeGreaterThan(0);
    expect(dark.filter((token) => !light.has(token))).toEqual([]);
  });

  it('remaps dark mode by redeclaring token values and nothing else', () => {
    const dark = block(tokens, DARK_BLOCK);
    // Every declaration inside the dark block is a custom property. A component
    // selector here would be a rule the token layer cannot express.
    const declarations = [...dark.matchAll(/([\w-]+)\s*:[^;{}]*;/g)].map((match) => match[1] ?? '');
    expect(declarations.filter((property) => !property.startsWith('--'))).toEqual([]);
  });

  it('names no theme in JavaScript: prefers-color-scheme is the only switch', () => {
    expect(tokens).toContain('color-scheme: light dark');
    expect(tokens).toMatch(DARK_BLOCK);
  });
});

describe('base rules', () => {
  it('draws the focus ring as an outline, which forced colors keeps', () => {
    expect(base).toMatch(/:focus-visible\s*\{\s*outline:\s*3px solid var\(--focusRing\);/);
    expect(base).toMatch(/outline-offset:\s*2px/);
    expect(base).not.toMatch(/:focus-visible[^{]*\{[^}]*box-shadow/);
  });

  it('restates the ring in forced colors against the system highlight', () => {
    expect(preferences).toMatch(/@media \(forced-colors: active\)/);
    expect(preferences).toMatch(/outline:\s*3px solid Highlight/);
  });

  it('imports the preference queries after every other SHARED sheet', () => {
    // All this file can see, and all index.css can decide. Whether the two
    // blocks actually win is a fact about the emitted bundle — the page sheets
    // are imported by their page components and land after every line of this
    // list — and src/styles/preferences.test.ts reads the bundle for it.
    const index = read('./index.css');
    const order = [...index.matchAll(/@import url\('\.\/([\w.-]+)'\)/g)].map((m) => m[1]);
    expect(order[0]).toBe('tokens.css');
    expect(order.at(-1)).toBe('preferences.css');
    expect(order.indexOf('surfaces.css')).toBeLessThan(order.indexOf('preferences.css'));
  });

  it('clamps every animation under prefers-reduced-motion: reduce', () => {
    const reduced = block(preferences, '@media (prefers-reduced-motion: reduce)');
    for (const property of [
      'animation-duration',
      'animation-iteration-count',
      'transition-duration',
      'scroll-behavior',
    ]) {
      expect(reduced, `${property} is not clamped`).toContain(property);
    }
    expect(reduced).toContain('*, *::before, *::after');
  });
});
