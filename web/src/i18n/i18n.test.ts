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
    // The anonymous surface then took it to 587/605. Three of its six new
    // source strings have no Chinese value and are named below; the other three
    // do, and are not translations written here — `status.token.operational` is
    // the value `status.state.operational` has carried since the Go catalogue,
    // for the same word from the same handler.
    //
    // /monitor's scheduler card then took it to 587/610. Its five new source
    // strings are the three questions the card is grouped by, the disclosure
    // holding the per-pool evidence, and the one reading on it that is a
    // proportion rather than a count. All five are named below and none is
    // translated here: web-ui rule #21 — a value from a file this port does not
    // write is reported, not edited into compliance — and the translated
    // catalogue is a translator's file.
    //
    // Restoring the shared runtime took it to 587/611. It added exactly one
    // source string — what a surface says for the moment between a 401 and the
    // sign-in page it is being sent to — and it moved four: the step-up
    // credential strings are no longer the shell's, because six controls on
    // three other routes now raise the same prompt. The move carried the
    // Chinese values with it, so only the new key is untranslated, and it is
    // named below.
    //
    // Promoting the three fallback screens then took it to 587/618. Their seven
    // strings are not new copy: they were already on screen, out of a private
    // table shaped like this catalogue and sitting outside it, where no key
    // scan reached them and — an absent key cannot be reported missing —
    // neither did this number. Counting them is what turns the console's error
    // screens from a surface nobody could see into seven named lines below.
    //
    // Written as the fraction rather than as a rounded literal so the floor and
    // the two counts it comes from stay one statement.
    expect(coverage).toBeGreaterThanOrEqual((587 / 618) * 100);
  });

  /**
   * The keys with no Chinese value, named rather than machine-translated.
   *
   * Five are strings the old console never translated at all.
   *
   * Two are keys that each carried two different English strings at two call
   * sites — `Managed by deployment environment` against `… (configured)`, and
   * `Catalog` against `Active Catalog` — so the single Chinese value was
   * rendered in both places and said the wrong thing in one of them. Splitting
   * the key is what makes that visible.
   *
   * Two are the navigation landmarks, whose `aria-label` was an English literal
   * in the markup: a Chinese reader on a Chinese page heard "Main navigation".
   * An aria-label is the copy-bearing attribute nobody sees on screen, which is
   * why it is also the one that goes untranslated.
   *
   * Six arrived with the ported build pages, which say things the console this
   * replaces never said: that a filter matched nothing (as opposed to nothing
   * existing), that the filter can be cleared, that the list endpoint answers
   * with a bounded page and there are older jobs behind it, how many records a
   * bulk delete is about to remove, that a dialog can be dismissed without
   * running it, and that a job's record could not be read at all.
   *
   * Three arrived with the ported anonymous surface, which now states two facts
   * the old public pages left in a `title` attribute or left out entirely: the
   * sync path an operator copies into binrepos.conf, and whether the public
   * catalogue reports a signature for an artifact at all. The third is the word
   * it uses when the catalogue reports nothing — `publicPackage` in
   * handlers_binhost.go carries no signature field today, and "not reported" is
   * what the page says rather than inferring "signed" from the deployment
   * holding a signing key.
   *
   * Five arrived with /monitor's regrouped scheduler card. Four are structural
   * — the three questions an operator arrives with, written as headings, and
   * the disclosure that holds the per-pool and per-decision evidence for them —
   * and the fifth names the slot utilisation reading, which is the one figure on
   * that card that can be over its own limit rather than merely large. The
   * answers beside the three headings need no strings: they are status badges,
   * and STATUS_VOCABULARY already carries those words in both languages.
   *
   * One arrived with the shared runtime: the sentence a surface carries for the
   * moment between a 401 and the sign-in page the shell is sending the reader
   * to. It exists because the transport's own word for that outcome —
   * `unauthorized` — was reaching the screen as an English literal from outside
   * the catalogue, which no key check could see.
   *
   * Seven arrived with the three screens the shell renders in place of a page:
   * a render that threw, an address the router matches nothing for, and a route
   * whose capability this session was not granted. The console this replaces
   * has none of them — it rendered on the server, so a page that failed was a
   * Go error page and a bad address was a 404 from net/http — which is why
   * there is no Chinese to carry over for any of the seven.
   *
   * Supplying a value for any of the thirty-one is a translator's job, not
   * this port's. They are named here so that is a task rather than a discovery.
   */
  it('names every untranslated key, so a new one cannot arrive unnoticed', () => {
    const untranslated = Object.keys(EN)
      .filter((key) => !(key in ZH))
      .sort();
    expect(untranslated).toEqual([
      'builds.cleanup.count',
      'builds.filter.clear',
      'builds.none',
      'builds.truncated',
      'common.cancel',
      'common.expired',
      'detail.egress',
      'detail.image',
      'detail.norecord',
      'detail.profile',
      'err.forbidden.h1',
      'err.forbidden.hint',
      'err.notfound.h1',
      'err.notfound.hint',
      'err.render.h1',
      'err.render.hint',
      'err.render.reload',
      'factory.catalog.stat',
      'filter.policy',
      'mon.gateway.protocol.version',
      'mon.scheduler.group.budget',
      'mon.scheduler.group.detail',
      'mon.scheduler.group.flow',
      'mon.scheduler.group.stuck',
      'mon.scheduler.utilisation',
      'nav.main',
      'nav.public',
      'packages.signature',
      'packages.signature.unreported',
      'packages.sync',
      'set.secret.external.placeholder',
    ]);
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
    const zh = messagesFor('zh');
    expect(zh.t('detail.profile')).toBe(EN['detail.profile']);
  });

  it('degrades the three fallback screens to their source string, slot and all', () => {
    // The screens that state a render fault, a bad address and a missing grant
    // are read through the same lookup as every other surface now, which is
    // what makes their absence from the translated catalogue a reported gap
    // rather than a private decision. A Chinese reader sees the English today;
    // on the day a translator supplies a value, the screens change without
    // being edited.
    const zh = messagesFor('zh');
    expect(zh.t('err.render.h1')).toBe(EN['err.render.h1']);
    expect(zh.t('err.notfound.hint')).toBe(EN['err.notfound.hint']);
    expect(zh.t('err.forbidden.hint', { capability: 'system-admin' })).toBe(
      EN['err.forbidden.hint'].replace('{capability}', 'system-admin'),
    );
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
