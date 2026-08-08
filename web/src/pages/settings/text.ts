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
