import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import stylelint from 'stylelint';
import { describe, expect, it } from 'vitest';

/**
 * The stylesheet gates, put under a gate.
 *
 * Both of the rules exercised here certified a stylesheet they were not
 * reading. `mode-blocks-agree` compared the at-rule prelude against one literal
 * string, so respelling the query — `(prefers-color-scheme:dark)`, no space,
 * valid CSS, identical meaning — did not make it miss a defect, it made the rule
 * return before checking anything, and deleting two dark tokens under that
 * spelling was a clean exit 0. `no-color-literals` knew hex and the functional
 * notations and nothing else, so `color: red` — the exact frozen light-mode
 * value in the failure it was written for — went straight through, as did the
 * fallback in `var(--x, #ff0000)` and both operands of a `color-mix()` between
 * two literals, because the strip that exempts token expressions deleted them
 * whole before anything looked inside.
 *
 * A rule that has never been red on anything is a rule nobody has shown to
 * work, so each case below is a snippet that must be reported and, beside it,
 * the legitimate form it must not be confused with. They run through the real
 * config, so the plugin's wiring and its overrides are part of what is asserted.
 *
 * The file sits under src/ because that is where vitest is configured to look.
 */

const STYLES = dirname(fileURLToPath(import.meta.url));
const CONFIG = join(STYLES, '../..', 'stylelint.config.js');

/** A component stylesheet's path, so the token layer's exemptions do not apply. */
const COMPONENT = join(STYLES, 'fixture.css');
/** The token layer's own path, which is where the mode blocks live. */
const TOKENS = join(STYLES, 'tokens.css');

async function warningsFor(code: string, rule: string, codeFilename: string): Promise<string[]> {
  const linted = await stylelint.lint({ code, codeFilename, configFile: CONFIG });
  return (linted.results[0]?.warnings ?? [])
    .filter((warning) => warning.rule === rule)
    .map((warning) => warning.text);
}

const COLOUR_RULE = 'portage-engine/no-color-literals';
const MODE_RULE = 'portage-engine/mode-blocks-agree';

describe('a component names a token and never spells a value', () => {
  const picks: Record<string, string> = {
    'a named colour': '.a { color: red; }',
    'a named colour on a background': '.a { background: white; }',
    'a named colour nobody would grep for': '.a { border-color: rebeccapurple; }',
    // The fallback is the value that renders whenever the token is missing, so
    // it is a colour pick with a token standing in front of it.
    'the fallback behind a token reference': '.a { color: var(--dangerInk, #ff0000); }',
    // A mix that names no token at all is two frozen values in the syntax of an
    // operation on one.
    'both operands of a mix between literals':
      '.a { background: color-mix(in srgb, #123456, #fff 10%); }',
    // And a hue mixed into a token is still a hue this file chose.
    'a hue mixed into a token':
      '.a { background: color-mix(in srgb, var(--accentFill), rebeccapurple 40%); }',
  };
  for (const [what, code] of Object.entries(picks)) {
    it(`reports ${what}`, async () => {
      expect(await warningsFor(code, COLOUR_RULE, COMPONENT)).toHaveLength(1);
    });
  }

  const operations: Record<string, string> = {
    'a bare token reference': '.a { color: var(--dangerInk); }',
    'a token behind a token': '.a { background: var(--controlBG, var(--pageRaised)); }',
    'a non-colour fallback': '.a { min-height: calc(var(--radius-small, 4) * 2); }',
    // The darken the ported stylesheet actually does: the literal is the
    // direction of the operation and follows the token into dark mode.
    'a token darkened toward black':
      '.a { background: color-mix(in srgb, var(--accentFill), #000 3%); }',
    'an alpha applied to a channel triple': '.a { background: rgba(var(--systemRed-rgb), .12); }',
    // Neither of these picks a hue; the second follows whichever ink it lands in.
    'the two that are not hues': '.a { color: transparent; border-color: currentcolor; }',
    // A colour word inside a quoted string is text, and in a font stack it is
    // the name of a typeface.
    'a colour word in generated content': '.a::after { content: " red alert"; }',
    'a typeface whose name is a colour': '.a { font-family: "Red Hat Mono", monospace; }',
  };
  for (const [what, code] of Object.entries(operations)) {
    it(`leaves ${what} alone`, async () => {
      expect(await warningsFor(code, COLOUR_RULE, COMPONENT)).toEqual([]);
    });
  }
});

describe('the two mode blocks are compared however the query is spelled', () => {
  const light = ':root { --ink: #d70015; --pageRaised: #fff; }';
  const dark = ':root { --pageRaised: #1f1f1f; }';

  // Three spellings of one query. The second and third are what a formatter or
  // a hand edit produces, and under the literal comparison each of them turned
  // the rule off rather than turning it red.
  const spellings = [
    '@media (prefers-color-scheme: dark)',
    '@media (prefers-color-scheme:dark)',
    '@media screen and (prefers-color-scheme: dark)',
  ];
  for (const query of spellings) {
    it(`reports a light-only colour under \`${query}\``, async () => {
      const reported = await warningsFor(`${light}\n${query} { ${dark} }`, MODE_RULE, TOKENS);
      expect(reported).toHaveLength(1);
      expect(reported[0]).toContain('--ink is a colour (#d70015) declared only in light mode.');
    });

    it(`says nothing under \`${query}\` when both blocks agree`, async () => {
      const paired = `${light}\n${query} { :root { --ink: #ff453a; --pageRaised: #1f1f1f; } }`;
      expect(await warningsFor(paired, MODE_RULE, TOKENS)).toEqual([]);
    });
  }

  it('reports a stylesheet that declares the palette and has no dark block at all', async () => {
    const reported = await warningsFor(light, MODE_RULE, TOKENS);
    expect(reported).toHaveLength(1);
    expect(reported[0]).toContain('no @media (prefers-color-scheme: dark) block');
  });

  it('asks a component stylesheet, which declares no palette, for nothing', async () => {
    const component = '.card { background: var(--pageRaised); }';
    expect(await warningsFor(component, MODE_RULE, COMPONENT)).toEqual([]);
  });

  it('is not satisfied by a negated query', async () => {
    const negated = `${light}\n@media not all and (prefers-color-scheme: dark) { ${dark} }`;
    const reported = await warningsFor(negated, MODE_RULE, TOKENS);
    expect(reported).toHaveLength(1);
    expect(reported[0]).toContain('no @media (prefers-color-scheme: dark) block');
  });
});
