/**
 * The message layer.
 *
 * One object per language, built once, carrying the lookup, the plural rule and
 * every formatter already bound to that language's locale tag. Binding is the
 * point: a component that has the messages object cannot format a date without a
 * locale, because there is no unbound formatter in reach.
 *
 * Resolution order for a lookup is source-first, not translation-first:
 * `ZH[key] ?? EN[key]`. A key with no Chinese value renders the English string,
 * which is the correct degradation for an incomplete translated catalogue — the
 * alternative, letting the lookup miss, is how a missing key white-screened a
 * production page while type-check and build were both green.
 */

import { formatBytes, formatDateTime, formatDuration, formatNumber, instant } from './format';
import type { LocaleTag } from './format';
import { EN } from './messages.en';
import type { MessageKey } from './messages.en';
import { ZH } from './messages.zh';
import { EN_PLURALS, ZH_PLURALS } from './plurals';
import type { PluralCategory, PluralKey } from './plurals';

export type { MessageKey } from './messages.en';
export type { PluralKey } from './plurals';
export type { LocaleTag } from './format';

/** The two languages the console ships strings for. */
export type Language = 'en' | 'zh';

/** What a `{name}` slot may be filled with. */
export type MessageValues = Readonly<Record<string, string | number>>;

/** The locale tag each language formats with. */
const LOCALE_TAGS: Record<Language, LocaleTag> = { en: 'en', zh: 'zh-CN' };

const PLACEHOLDER = /\{([a-zA-Z][a-zA-Z0-9_]*)\}/g;

/**
 * Fill the `{name}` slots.
 *
 * A slot with no value is left standing rather than replaced with an empty
 * string: `{count}` on screen is a bug report a reader can send, and a sentence
 * silently missing its number is one nobody can tell from a correct one. That is
 * the failure the placeholder-parity check in i18n.test.ts exists for, and this
 * is what it looks like when it reaches a browser anyway.
 */
function interpolate(template: string, values: MessageValues | undefined): string {
  if (values === undefined) {
    return template;
  }
  return template.replace(PLACEHOLDER, (slot, name: string) => {
    const value = values[name];
    return value === undefined ? slot : String(value);
  });
}

export interface Messages {
  readonly lang: Language;
  readonly locale: LocaleTag;

  /** A translated string. `t('typo')` does not compile. */
  t(key: MessageKey, values?: MessageValues): string;

  /** A counted string, through `Intl.PluralRules`. */
  plural(key: PluralKey, count: number, values?: MessageValues): string;

  dateTime(
    value: string | number | Date | null | undefined,
    options?: Intl.DateTimeFormatOptions,
  ): string | null;
  number(value: number, options?: Intl.NumberFormatOptions): string;
  bytes(value: number): string;
  duration(seconds: number): string;
  instant(value: string | number | Date | null | undefined): Date | null;
}

function pluralCategory(locale: LocaleTag, count: number): PluralCategory {
  try {
    return new Intl.PluralRules(locale).select(count);
  } catch {
    // A browser without PluralRules for this tag is not a reason to render
    // nothing. `other` is the category every language declares.
    return 'other';
  }
}

const CACHE = new Map<Language, Messages>();

/**
 * The messages object for a language. Cached, because the plural rule and the
 * formatters below it are the expensive part and a component may ask on every
 * render.
 */
export function messagesFor(lang: Language): Messages {
  const cached = CACHE.get(lang);
  if (cached !== undefined) {
    return cached;
  }
  const locale = LOCALE_TAGS[lang];
  const built: Messages = {
    lang,
    locale,
    t(key, values) {
      const translated = lang === 'zh' ? ZH[key] : undefined;
      return interpolate(translated ?? EN[key], values);
    },
    plural(key, count, values) {
      const forms: Partial<Record<PluralCategory, string>> =
        (lang === 'zh' ? ZH_PLURALS[key] : undefined) ?? EN_PLURALS[key];
      const category = pluralCategory(locale, count);
      const template = forms[category] ?? EN_PLURALS[key].other;
      return interpolate(template, { count, ...values });
    },
    dateTime: (value, options) => formatDateTime(locale, value, options),
    number: (value, options) => formatNumber(locale, value, options),
    bytes: (value) => formatBytes(locale, value),
    duration: (seconds) => formatDuration(locale, seconds),
    instant,
  };
  CACHE.set(lang, built);
  return built;
}
