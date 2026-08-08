/**
 * Counted strings.
 *
 * A message whose text carries a number goes through `Intl.PluralRules`, never
 * through a hand-written `=== 1`. English declares `one` and `other`; Chinese
 * declares `other` and nothing else, which is correct Chinese and is why the
 * translated table is typed as a partial set of categories rather than as the
 * source's shape. A language with `few`/`many` adds them here without any code
 * changing.
 *
 * Extracted from internal/dashboard/ui.go; not retyped.
 */

/** The CLDR categories `Intl.PluralRules` can return. */
export type PluralCategory = 'zero' | 'one' | 'two' | 'few' | 'many' | 'other';

export const EN_PLURALS = {
  'builds.count': { one: '{count} job total', other: '{count} jobs total' },
  'logs.lines': { one: '{count} line', other: '{count} lines' },
  'set.testok': { one: 'Connected — found {count} node', other: 'Connected — found {count} nodes' },
  'packages.count': { one: '{count} published package', other: '{count} published packages' },
} as const;

/** Every counted string the console may name. */
export type PluralKey = keyof typeof EN_PLURALS;

/**
 * The categories each language actually has, so a table carrying a category its
 * own language never selects — or missing one it does — is a test failure rather
 * than a form nobody ever sees. Values are CLDR's plural rules for the tag the
 * app formats with.
 */
export const PLURAL_CATEGORIES = {
  en: ['one', 'other'],
  zh: ['other'],
} as const satisfies Record<string, readonly PluralCategory[]>;

export const ZH_PLURALS: Partial<Record<PluralKey, Partial<Record<PluralCategory, string>>>> = {
  'builds.count': { other: '共 {count} 个任务' },
  'packages.count': { other: '共 {count} 个已发布软件包' },
  'logs.lines': { other: '{count} 行' },
  'set.testok': { other: '连接成功，发现 {count} 个节点' },
};
