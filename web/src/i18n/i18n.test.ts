import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import { messagesFor } from './messages';
import { EN } from './messages.en';
import type { MessageKey } from './messages.en';
import { ZH } from './messages.zh';
import { EN_PLURALS, PLURAL_CATEGORIES, ZH_PLURALS } from './plurals';
import type { PluralCategory } from './plurals';
import { meterLevel, resolveStatus } from './status';
import { STATUS_VOCABULARY } from './vocabulary';

/**
 * The checks the console this replaces could not run, because its English lived
 * in HTML attributes, its Chinese lived in a Go map and its placeholders were
 * spelled three different ways. Two of them find real defects the moment they
 * are written, which is the point: a gate that has never been red on the tree it
 * guards has not been shown to work.
 */

// Path arithmetic on the file path, not on a URL: the tests run under jsdom,
// whose `URL` resolves a relative reference against the document base rather
// than against the base argument, so `new URL('..', import.meta.url)` comes back
// as an http:// URL pointing at the dev server.
const SOURCE_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');

function sourceFiles(directory: string): string[] {
  const found: string[] = [];
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      found.push(...sourceFiles(path));
    } else if (/\.tsx?$/.test(entry.name) && !entry.name.endsWith('.test.ts')) {
      found.push(path);
    }
  }
  return found;
}

const PLACEHOLDERS = /\{([a-zA-Z][a-zA-Z0-9_]*)\}/g;

function placeholders(template: string): string[] {
  return [...template.matchAll(PLACEHOLDERS)].map((match) => match[1] ?? '').sort();
}

describe('the source catalogue is the key set', () => {
  it('resolves every message key a component names', () => {
    // The type already refuses `t('typo')` at the call site. This is the second
    // half: a key assembled from a template literal is invisible to the
    // compiler, so the literal prefixes are collected here and checked as a set.
    const referenced = new Set<string>();
    for (const path of sourceFiles(SOURCE_ROOT)) {
      // Comments first: the doc comments explaining this mechanism spell
      // `t('typo')` on purpose, and a scan that reads prose reports it.
      const text = readFileSync(path, 'utf8')
        .replace(/\/\*[\s\S]*?\*\//g, '')
        .replace(/^\s*\/\/.*$/gm, '');
      for (const match of text.matchAll(/\bt\(\s*'([a-z][\w.]*)'/g)) {
        referenced.add(match[1] ?? '');
      }
    }
    expect(referenced.size).toBeGreaterThan(0);
    expect([...referenced].filter((key) => !(key in EN))).toEqual([]);
  });

  it('carries no Chinese value for a key the source does not have', () => {
    expect(Object.keys(ZH).filter((key) => !(key in EN))).toEqual([]);
  });

  it('holds every message table, so no second one can render strings unseen', () => {
    // What src/app/fallbackCopy.ts was: seven English strings in a file shaped
    // exactly like this catalogue, sitting outside it, with its own copy of the
    // `{name}` interpolator, feeding the three screens the shell renders in
    // place of a page. Nothing in this file could see it. The key scan reads
    // `t(` calls, placeholder parity reads `EN` against `ZH`, and the coverage
    // number counts what `ZH` is missing — which cannot count a string that was
    // never in `EN`. So the console's error screens were untranslated and also
    // unreportable, and the report is the only thing that would have said so.
    //
    // The shape is what is banned, not the strings: a quoted dotted key against
    // a string value is what a catalogue looks like, and the two files that may
    // hold one are in this directory.
    const catalogueLine = /^\s*'[a-z][A-Za-z0-9]*(?:\.[A-Za-z0-9]+)+':\s*(?:'|"|$)/m;
    const offenders = sourceFiles(SOURCE_ROOT)
      .filter((path) => !path.startsWith(join(SOURCE_ROOT, 'i18n')))
      .filter((path) =>
        catalogueLine.test(
          readFileSync(path, 'utf8')
            .replace(/\/\*[\s\S]*?\*\//g, '')
            .replace(/^\s*\/\/.*$/gm, ''),
        ),
      );
    expect(offenders).toEqual([]);
  });

  it('keeps Chinese coverage above the floor it shipped at', () => {
    const coverage = (Object.keys(ZH).length / Object.keys(EN).length) * 100;
    // Reported, and floored — never gated on equality. A translated catalogue is
    // allowed to be incomplete; what is not allowed is for it to get worse.
    //
    // The floor moves only when the source catalogue grows and the translation
    // has not caught up yet, and only by exactly that much: it was 98.4 for 593
    // source strings, and the six strings the ported build pages needed — a
    // filtered-empty sentence, a clear-filter control, a truncation notice, a
    // delete count, a dialog dismissal and the no-record state — take 599
    // source strings against the same 584 translated ones. Not one of the six
    // is translated here; every one is named below, which is what turns them
    // into a translator's task instead of a discovery. A Chinese value that
    // disappears still turns this red.
    //
    // The anonymous surface then took it to 587/605, /monitor's regrouped
    // scheduler card to 587/610, the restored shared runtime to 587/611, and
    // promoting the three fallback screens out of the private table they used
    // to live in to 587/618 — thirty-one source strings the port added or
    // uncovered and left for a translator, every one of them named below.
    //
    // The translator has now written all thirty-one, so the floor is 618/618,
    // and moving the builder-binary SHA-256 field's label, hint and placeholder
    // out of the page and into the catalogue makes it 621/621 — three source
    // strings that arrived translated, which is why the floor moved with them
    // rather than the coverage dropping.
    // It stays a floor and not an equality: the statement worth making here is
    // still "this cannot get worse", and a source catalogue that grows faster
    // than the translation is a translator's backlog rather than a broken
    // build. What forbids a silent gap is the list below, which is the check
    // that names the missing key rather than merely counting it.
    //
    // Written as the fraction rather than as a rounded literal so the floor and
    // the two counts it comes from stay one statement.
    expect(coverage).toBeGreaterThanOrEqual((621 / 621) * 100);
  });

  /**
   * The keys with no Chinese value. There are none, and that is the assertion.
   *
   * This list held thirty-one for the length of the port, each one named rather
   * than machine-translated so it read as a translator's task instead of a
   * discovery. The tasks were done: the console's three error screens, the
   * scheduler card's three questions, both navigation landmarks, the two keys
   * that had been split because one Chinese value was being rendered at two
   * call sites saying two different things, and the rest.
   *
   * An empty expectation is a stronger statement than a populated one and much
   * stronger than deleting the check. A new source string with no translation
   * lands here by name on the next run, which is the whole reason the list was
   * ever written out — and it now says the console has no English left on a
   * Chinese screen, rather than saying which English is left and why.
   *
   * The comparison stays a whole-array equality for the same reason it always
   * was one: a count, or a `toHaveLength(0)`, reports that something is missing
   * without reporting what.
   */
  it('holds no untranslated key, and names any that arrives', () => {
    const untranslated = Object.keys(EN)
      .filter((key) => !(key in ZH))
      .sort();
    expect(untranslated).toEqual([]);
  });

  /**
   * The keys whose Chinese value is the English one, character for character.
   *
   * Key parity cannot see this and the coverage floor counts it as translated:
   * the key is present and has a value, and the value is English. That is how
   * `quota.shadow` shipped as ` shadow ` next to a translated `活跃`, in the one
   * line of the project quota card that names two readings side by side.
   *
   * The nine below are the ones that are meant to be identical — product names,
   * a Gentoo term the rest of this catalogue also leaves in English, and three
   * abbreviations no reader expands differently in Chinese. A whole-array
   * equality rather than a count, for the same reason as the check above: a new
   * one lands here by name.
   */
  it('leaves English in place only where English is the term', () => {
    const identical = Object.keys(EN)
      .filter((key) => key in ZH && ZH[key as MessageKey] === EN[key as MessageKey])
      .filter((key) => /[A-Za-z]/.test(EN[key as MessageKey]))
      .sort();
    expect(identical).toEqual([
      'detail.profile',
      'mon.gateway',
      'mon.gateway.protocol.version',
      'packages.profile',
      'quota.cpu',
      'set.aws',
      'set.gcp',
      'set.pve',
      'th.ip',
    ]);
  });
});

describe('Chinese punctuation', () => {
  /**
   * Chinese sentences take Chinese punctuation.
   *
   * This is not a preference: a half-width comma against a Chinese character
   * renders without the space the glyph is designed to carry, so `实例,本控制台`
   * sets tighter than the `，` beside it and the catalogue reads as two authors.
   * Sixty-three strings were written this way against a hundred-odd that were
   * not, which is what makes it a defect rather than a house style.
   *
   * Bounded to marks that touch a Chinese character on either side, so an
   * English clause, a URL, `sha256:`, an `10.0.0.10:8080` and a `{count}`
   * placeholder are all left alone.
   */
  const ADJACENT = /[\u3400-\u9fff][,;:?!]|[,;:?!][\u3400-\u9fff]/;

  it('uses full-width marks inside Chinese sentences', () => {
    const offenders = Object.entries(ZH)
      .filter(([, value]) => ADJACENT.test(value))
      .map(([key]) => key)
      .sort();
    expect(offenders).toEqual([]);
  });

  it('uses them in the plural catalogue too', () => {
    const offenders = Object.entries(ZH_PLURALS)
      .filter(([, forms]) => Object.values(forms).some((form) => ADJACENT.test(form ?? '')))
      .map(([key]) => key)
      .sort();
    expect(offenders).toEqual([]);
  });
});

describe('placeholder parity', () => {
  /**
   * The highest-yield i18n check, and the one key parity cannot see: the key is
   * present and the value looks plausible while the number never arrives.
   *
   * The console this replaces had one — `Sign in with {{.DisplayName}}` against
   * `使用 %s 登录`. Two interpolation syntaxes for one message, so no comparison
   * was even expressible.
   */
  it('gives every translated string the source string placeholders', () => {
    const mismatched = Object.entries(ZH)
      .filter(([key, value]) => {
        const source = EN[key as MessageKey];
        return String(placeholders(source)) !== String(placeholders(value ?? ''));
      })
      .map(([key]) => key);
    expect(mismatched).toEqual([]);
  });

  it('gives every translated plural form the source form placeholders', () => {
    const mismatched: string[] = [];
    for (const [key, forms] of Object.entries(ZH_PLURALS)) {
      const source = EN_PLURALS[key as keyof typeof EN_PLURALS];
      for (const [category, value] of Object.entries(forms ?? {})) {
        if (String(placeholders(value)) !== String(placeholders(source.other))) {
          mismatched.push(`${key}.${category}`);
        }
      }
    }
    expect(mismatched).toEqual([]);
  });
});

describe('plurals', () => {
  it('declares exactly the CLDR categories each language selects', () => {
    for (const [lang, declared] of Object.entries(PLURAL_CATEGORIES)) {
      const messages = messagesFor(lang as 'en' | 'zh');
      const selected = new Set<PluralCategory>();
      // 0..40 plus a fraction covers `one`, `few`, `many` and `other` in every
      // language this table can grow to hold, so the assertion is about the
      // language and not about the numbers this console happens to print today.
      for (const count of [...Array.from({ length: 41 }, (_unused, index) => index), 1.5]) {
        selected.add(new Intl.PluralRules(messages.locale).select(count));
      }
      expect([...selected].sort(), `${lang} selects a category it does not declare`).toEqual(
        [...declared].sort(),
      );
    }
  });

  it('gives Chinese one form and English two', () => {
    for (const key of Object.keys(EN_PLURALS) as (keyof typeof EN_PLURALS)[]) {
      expect(Object.keys(EN_PLURALS[key]).sort()).toEqual(['one', 'other']);
      expect(Object.keys(ZH_PLURALS[key] ?? {})).toEqual(['other']);
    }
  });

  it('picks the form with Intl.PluralRules and not with a comparison to one', () => {
    const en = messagesFor('en');
    expect(en.plural('logs.lines', 1)).toBe('1 line');
    expect(en.plural('logs.lines', 0)).toBe('0 lines');
    expect(en.plural('logs.lines', 2)).toBe('2 lines');
    // Chinese has one form, and it is used for every count.
    const zh = messagesFor('zh');
    expect(zh.plural('logs.lines', 1)).toBe('1 行');
    expect(zh.plural('logs.lines', 12)).toBe('12 行');
  });
});

describe('lookup and degradation', () => {
  it('falls back to the source string for an untranslated key', () => {
    // `ZH[key] ?? EN[key]` is what keeps an incomplete translated catalogue safe
    // to ship, and every key has a Chinese value now, so nothing left in the
    // tree exercises that branch. It is exercised against a hole opened here and
    // closed in a `finally`: the lookup reads ZH at call time rather than at
    // build time, so removing one value is enough — and a hole left open would
    // hand a short catalogue to every test that runs after this one.
    const borrowed = ZH['builds.h1'];
    if (borrowed === undefined) {
      throw new Error('builds.h1 has no Chinese value; this check borrows one that has');
    }
    try {
      delete ZH['builds.h1'];
      expect(messagesFor('zh').t('builds.h1')).toBe(EN['builds.h1']);
    } finally {
      ZH['builds.h1'] = borrowed;
    }
    expect(messagesFor('zh').t('builds.h1')).toBe(borrowed);
  });

  it('renders the three fallback screens in Chinese, slot and all', () => {
    // The screens that state a render fault, a bad address and a missing grant
    // are read through the same lookup as every other surface, which is what
    // turned their absence from the translated catalogue into a reported gap
    // rather than a private decision — and then into copy, without either
    // screen being edited.
    //
    // The two headings are transcribed here, the way STATUS_VOCABULARY is
    // transcribed further down: a heading reworded in one file has to be
    // reworded in two, and an error screen is the one surface nobody opens
    // deliberately to notice it went back to English.
    const zh = messagesFor('zh');
    expect(zh.t('err.render.h1')).toBe('控制台无法渲染此页面');
    expect(zh.t('err.notfound.h1')).toBe('没有这个页面');
    expect(zh.t('err.notfound.hint')).not.toBe(EN['err.notfound.hint']);
    // The forbidden screen names the grant it wants. A translated sentence that
    // dropped or renamed the slot would read as a correct sentence with the one
    // fact worth reporting missing from it.
    const forbidden = zh.t('err.forbidden.hint', { capability: 'system-admin' });
    expect(forbidden).toContain('system-admin');
    expect(forbidden).not.toContain('{capability}');
    expect(forbidden).not.toBe(EN['err.forbidden.hint'].replace('{capability}', 'system-admin'));
  });

  it('fills the named slots and leaves an unfilled one standing', () => {
    const en = messagesFor('en');
    expect(en.t('login.oidc', { provider: 'Keycloak' })).toBe('Sign in with Keycloak');
    // Visible rather than silently deleted: a sentence missing its number reads
    // as a correct sentence, and `{provider}` on screen is a bug report.
    expect(en.t('login.oidc')).toBe('Sign in with {provider}');
  });
});

describe('formatting takes the app locale, never the operating system one', () => {
  it('formats the same instant differently per language', () => {
    const when = '2026-08-01T12:34:56Z';
    const options: Intl.DateTimeFormatOptions = { dateStyle: 'short', timeZone: 'UTC' };
    expect(messagesFor('en').dateTime(when, options)).not.toBe(
      messagesFor('zh').dateTime(when, options),
    );
  });

  it('reads Go zero time as "not an instant" rather than as year 1', () => {
    for (const zero of ['0001-01-01T00:00:00Z', '', null, undefined]) {
      expect(messagesFor('en').dateTime(zero)).toBeNull();
    }
    expect(messagesFor('en').instant('2026-08-01T00:00:00Z')).not.toBeNull();
  });
});

describe('the status vocabulary', () => {
  /**
   * The table, transcribed, and compared whole.
   *
   * Every other check in this file reads the vocabulary as a set of keys, which
   * gates the token list and nothing about what any of them means: bind
   * `offline` to green and a builder that is not answering renders the same hue
   * as one that is, with the label check, the coverage check and every page test
   * still green. Sampling three tokens does not fix that, because the twenty-six
   * that are not sampled are exactly the ones that go unnoticed.
   *
   * So the hues are written out once, from statusVocabulary in
   * internal/dashboard/ui.go for the thirty-one it carries and from the two
   * handlers the ported public surfaces read for the other three, and compared
   * as one object. A hue that moves now has to move in two files, and a token
   * added without a considered hue turns this red instead of arriving under a
   * check that only counts keys.
   */
  it('binds every token to the hue the backend vocabulary gives it', () => {
    expect(STATUS_VOCABULARY).toEqual({
      // Job lifecycle (internal/persistence job status).
      queued: 'gray',
      claimed: 'orange',
      provisioning: 'orange',
      forwarding: 'orange',
      deploying: 'orange',
      building: 'blue',
      collecting: 'blue',
      verifying: 'blue',
      signing: 'blue',
      publishing: 'blue',
      success: 'green',
      completed: 'green',
      failed: 'red',
      canceled: 'gray',
      // Builder and instance lifecycle (internal/builder local.go / registry.go).
      online: 'green',
      busy: 'blue',
      draining: 'orange',
      offline: 'red',
      running: 'green',
      destroy_failed: 'red',
      // Health rollups the monitor cards feed through the same badge.
      healthy: 'green',
      unhealthy: 'red',
      degraded: 'red',
      pending: 'gray',
      // Issuer generation and workload certificate lifecycle.
      active: 'green',
      revoked: 'red',
      // Image-factory milestone and step states.
      not_started: 'gray',
      planned: 'gray',
      in_progress: 'blue',
      blocked: 'orange',
      passed: 'green',
      // handlers_public.go's own two-word vocabulary for the anonymous status
      // page; `degraded` above is the other half of it.
      operational: 'green',
      // The artifact half, in the spelling the build record uses. `unsigned` is
      // orange and not red because a binhost may legitimately publish unsigned
      // while a deployment is still establishing its signing trust.
      signed: 'green',
      unsigned: 'orange',
    });
  });

  it('has a translation for every token it has a colour for', () => {
    for (const token of Object.keys(STATUS_VOCABULARY)) {
      expect(`status.token.${token}` in EN, `${token} has no label`).toBe(true);
      expect(`status.token.${token}` in ZH, `${token} has no Chinese label`).toBe(true);
    }
  });

  it('falls an unknown token to gray with its own raw name', () => {
    const resolved = resolveStatus('a_state_shipped_after_this_console', messagesFor('zh'));
    expect(resolved).toEqual({
      color: 'gray',
      label: 'a_state_shipped_after_this_console',
      known: false,
    });
  });

  it('spells a known token in the reader language, on the badge and in a sentence', () => {
    expect(resolveStatus('draining', messagesFor('en')).label).toBe('draining');
    expect(resolveStatus('draining', messagesFor('zh')).label).toBe('排空中');
    expect(resolveStatus('draining', messagesFor('en')).color).toBe('orange');
  });

  it('decides the meter level once, so two surfaces cannot disagree', () => {
    expect(meterLevel(0, 10)).toBe('ok');
    expect(meterLevel(7, 10)).toBe('ok');
    expect(meterLevel(8, 10)).toBe('warn');
    expect(meterLevel(10, 10)).toBe('crit');
    expect(meterLevel(3, 0)).toBe('ok');
  });
});

describe('catalogue hygiene', () => {
  it('carries no emoji in either language', () => {
    // Typographic glyphs — · — → • ✓ — are not emoji and are kept. The scan is
    // by codepoint property, so a new emoji cannot slip in under a name nobody
    // put on a list.
    const emoji = /\p{Extended_Pictographic}/u;
    const offenders = [...Object.entries(EN), ...Object.entries(ZH)]
      .filter(([, value]) => emoji.test(String(value)))
      .map(([key]) => key);
    expect(offenders).toEqual([]);
  });
});
