/**
 * Four English catalogue entries were transcribed out of the Go template, where
 * `&` and `<` had to be written as XML entities to be legal inside the HTML
 * string constant. React escapes a text node, so those entities survive the
 * round trip and reach the reader as characters: the subnav says
 * "Backend &amp; Test", "Network &amp; Delivery" and "Sessions &amp; Security",
 * and the artifact-directory hint says "/local/&lt;dir&gt;/". The Chinese
 * catalogue is unaffected — it was written, not transcribed — so the defect is
 * visible in one language only, which is why nobody reading the served Chinese
 * page could have seen it.
 *
 * The catalogue is where this belongs and it is in the handoffs. Until then it
 * is decoded here, at the one page that reads those four keys, rather than in a
 * shared file six other pages are being written against.
 *
 * Only the five XML named entities, and only into a string that becomes a text
 * node. Nothing here can introduce an element: there is no markup path out of
 * this function.
 */

/**
 * The three strings on this page the message layer does not carry.
 *
 * The builder-binary digest field is the only one of the forty-five whose
 * label, hint and placeholder were written straight into the Go markup with no
 * `data-i18n` attribute, so they rendered in English on the Chinese page of the
 * console this replaces too. Keeping them is parity; retyping them into forty
 * translated siblings without saying so would not be. They are gathered here,
 * in one block, so moving them into `src/i18n/messages.en.ts` and
 * `messages.zh.ts` is a cut and a paste — the keys they want are named in the
 * handoffs as `set.binsha`, `set.binsha.hint` and `set.binsha.placeholder`.
 */
export const UNTRANSLATED = {
  binsha: 'Builder binary SHA-256',
  binshaHint:
    'Required when using a URL; the instance verifies it before installing or running the binary',
  binshaPlaceholder: '64 lowercase hexadecimal characters',
} as const;

const ENTITIES: Readonly<Record<string, string>> = {
  '&amp;': '&',
  '&lt;': '<',
  '&gt;': '>',
  '&quot;': '"',
  '&#39;': "'",
};

const ENTITY = /&(?:amp|lt|gt|quot|#39);/g;

export function decodeEntities(value: string): string {
  return value.replace(ENTITY, (entity) => ENTITIES[entity] ?? entity);
}
