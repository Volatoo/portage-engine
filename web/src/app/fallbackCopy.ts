/**
 * The English source strings the three fallback screens need.
 *
 * They belong in src/i18n/messages.en.ts beside the other six hundred, and they
 * are written here because that catalogue is not this change's file to edit. The
 * shape is the catalogue's, so promoting them is a move rather than a rewrite,
 * and until that happens a Chinese reader sees the source string — which is what
 * `messagesFor` already does for the two dozen keys the translated catalogue has
 * not caught up with. None of these has a Chinese value yet, and inventing one
 * here would be worse than not having one.
 *
 * Its own file rather than a constant beside the components, because a module
 * that exports both a component and a table is a module fast refresh cannot
 * reload.
 */

export const FALLBACK_EN = {
  'err.render.h1': 'The console could not render this page',
  'err.render.hint':
    'This is a fault in the console itself, not a refusal by the server. Reload; if it keeps happening, the message below is what to report.',
  'err.render.reload': 'Reload the console',
  'err.notfound.h1': 'No such page',
  'err.notfound.hint':
    'The console serves no page at this address. It may have been mistyped, or it may have been a page in an earlier version.',
  'err.forbidden.h1': 'Not available to you',
  'err.forbidden.hint':
    'This page needs the {capability} capability, which the identity service has not granted this session. Every request it made would be refused.',
} as const;

type FallbackKey = keyof typeof FALLBACK_EN;

/** The same `{name}` interpolation the catalogue uses, over the table above. */
export function fallback(key: FallbackKey, values: Readonly<Record<string, string>> = {}): string {
  return FALLBACK_EN[key].replace(
    /\{([a-zA-Z][a-zA-Z0-9_]*)\}/g,
    (slot, name: string) => values[name] ?? slot,
  );
}
