import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

/**
 * The two meters that say a limit is close, and the two channels they say it in.
 *
 * A quota bar and a builder-slot bar both cross a threshold and both mark it by
 * changing the fill's hue. Hue alone is one channel: a deuteranopic operator
 * reads orange and red as the same bar, and a forced-colors reader gets neither
 * because the mode replaces the fill entirely. So each level also appends a
 * mark after the reading — " !" at warn, " !!" at crit — and that mark is text,
 * which survives both.
 *
 * Neither channel is asserted by the component tests: `data-level` is what
 * `runtime.test.tsx` and `monitor.test.tsx` check, and the attribute is only the
 * hook. Deleting either rule below leaves the attribute exactly where it was,
 * every component case green, and the meter down to one channel — so the rules
 * are read here, off the stylesheet, one channel per case.
 */

const STYLES = dirname(fileURLToPath(import.meta.url));

function source(name: string): string {
  // Comments first: a brace or a declaration-shaped phrase inside an annotation
  // would otherwise be read as a rule.
  return readFileSync(join(STYLES, name), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
}

interface Rule {
  prelude: string;
  body: string;
}

/**
 * Every rule in a sheet, whitespace collapsed and quotes normalised.
 *
 * The two sheets are written in different hands — components.css is the ported
 * single-line form with double quotes, monitor.css the expanded form with
 * single ones — and which quote a `content` value wears is not a fact this file
 * is about.
 */
function rules(css: string): Rule[] {
  return [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((match) => ({
    prelude: (match[1] ?? '').replace(/\s+/g, ' ').replace(/"/g, "'").trim(),
    body: (match[2] ?? '').replace(/\s+/g, ' ').replace(/"/g, "'").trim(),
  }));
}

/** The single rule matching every fragment, or a failure naming what was sought. */
function only(css: Rule[], fragments: string[], property: string): string {
  const found = css.filter(
    (rule) =>
      fragments.every((fragment) => rule.prelude.includes(fragment)) &&
      new RegExp(`(^|;)\\s*${property}\\s*:`).test(rule.body),
  );
  expect(found.length, `no single rule sets ${property} on ${fragments.join(' + ')}`).toBe(1);
  const declared = new RegExp(`(?:^|;)\\s*${property}\\s*:([^;]+)`).exec(found[0]?.body ?? '');
  return (declared?.[1] ?? '').trim();
}

interface Meter {
  name: string;
  sheet: string;
  /** The element the level is written on. */
  root: string;
  /** The fill whose hue is the first channel. */
  fill: string;
  /** The reading the second channel is appended to. */
  value: string;
}

const METERS: readonly Meter[] = [
  {
    name: 'the project quota meter',
    sheet: './components.css',
    root: '.quota-meter',
    fill: '.quota-bar i',
    value: '.quota-v',
  },
  {
    name: 'the builder slot meter',
    sheet: './pages/monitor.css',
    root: '.slot-meter',
    fill: '.slot-bar i',
    value: '.slot-v',
  },
];

describe.each(METERS)('$name', (meter) => {
  const css = rules(source(meter.sheet));
  const at = (level: string): string[] => [`${meter.root}[data-level='${level}']`];

  it('drives the fill hue off the level, and off two different tokens', () => {
    const warn = only(css, [...at('warn'), meter.fill], 'background');
    const crit = only(css, [...at('crit'), meter.fill], 'background');
    // A named token, never a literal: the same orange and red carry every other
    // warning in the console and they are remapped once, in the dark block.
    expect(warn).toMatch(/^var\(--[\w-]+\)$/);
    expect(crit).toMatch(/^var\(--[\w-]+\)$/);
    // Two levels the same colour is one level with two names.
    expect(warn).not.toBe(crit);
  });

  it('appends a mark after the reading, which is the channel a hue cannot carry', () => {
    // Deleting these is what this case exists to catch. It changes no
    // attribute, breaks no component test, and leaves an operator who cannot
    // separate orange from red with a meter that has stopped saying anything.
    const warn = only(css, [...at('warn'), `${meter.value}::after`], 'content');
    const crit = only(css, [...at('crit'), `${meter.value}::after`], 'content');
    expect(warn).toBe("' !'");
    expect(crit).toBe("' !!'");
    // Distinguishable from each other, and from the unmarked reading, without
    // reading a colour off either.
    expect(warn).not.toBe(crit);
  });

  it('marks no level it does not also colour, and colours none it does not mark', () => {
    // The two channels are declared per level, so a level added to the markup
    // with only half its treatment written is a level that reads as normal to
    // one reader and as critical to another.
    const levelled = (property: string): string[] =>
      css
        .filter(
          (rule) =>
            rule.prelude.includes(`${meter.root}[data-level=`) &&
            new RegExp(`(^|;)\\s*${property}\\s*:`).test(rule.body),
        )
        .flatMap((rule) => /\[data-level='([\w-]+)'\]/.exec(rule.prelude)?.[1] ?? []);
    expect(levelled('content').sort()).toEqual(levelled('background').sort());
    expect(levelled('content').sort()).toEqual(['crit', 'warn']);
  });
});
