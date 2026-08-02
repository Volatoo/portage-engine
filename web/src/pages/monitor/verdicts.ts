/**
 * The decisions /monitor and /image-factory make about a value before it is
 * rendered: is this an instant, is this a conclusion, and which of several
 * conclusions is the worst one.
 *
 * Separated from the components in ./parts.tsx so neither file mixes the two —
 * a module exporting a component and a helper together is a fast-refresh
 * boundary the bundler cannot reason about.
 */

import { resolveState } from '../../api/state';
import type { ResourceState } from '../../api/state';
import type { MessageKey, Messages } from '../../i18n/messages';
import { resolveStatus } from '../../i18n/status';
import type { StatusColor } from '../../i18n/status';

/**
 * A timestamp, or the word for a thing that has never happened.
 *
 * This is the one gate, and every timestamp site on both routes goes through
 * it. Go serialises `time.Time` unconditionally — `omitempty` does not suppress
 * a struct — so a reconciler that has never run still sends
 * `0001-01-01T00:00:00Z`, and a locale formatter turns that into
 * `1/1/1, 8:05:43 AM`: a plausible clock reading standing in for "never",
 * printed beside the ledger's write-error count, which are exactly the two
 * numbers a reader uses to decide whether the ledger can be trusted.
 *
 * `messages.instant` is what decides "this is not an instant", and it decides
 * it for a zero time, an empty string, null and an unparseable value alike, so
 * all four get the same answer instead of four call sites each guessing. The
 * absent word is a message key, so it is translated, and it defaults to
 * `common.never` because that is what the absence of an event means here.
 *
 * The options this used to spell out are now what `messages.dateTime` defaults
 * to, so the reading is the same one every other route takes: the ledger's
 * "last checked" and a build's Created column are the same kind of fact and had
 * no business being formatted by two different tables.
 */
export function momentText(
  messages: Messages,
  value: string | number | Date | null | undefined,
  absent: MessageKey = 'common.never',
): string {
  return messages.dateTime(value) ?? messages.t(absent);
}

/** True when a value is a real instant, so a verdict can account for its absence. */
export function happened(
  messages: Messages,
  value: string | number | Date | null | undefined,
): boolean {
  return messages.instant(value) !== null;
}

/**
 * The class and attribute a value carrying an outcome wears.
 *
 * Measured on the console this replaces: "24h · success / SLO 14.3% · SLO
 * breach" and "write errors 19" both rendered `rgba(0,0,0,0.56)` at weight 400 —
 * byte-identical to the neutral cost statistic beside them — while the card
 * verdicts got `rgba(0,0,0,0.88)` at 600. The token block declares
 * `--successInk` and `--dangerInk` for exactly this and says in writing that
 * conclusion-bearing text takes the deepened ink.
 *
 * The ink is bound through the same `resolveStatus` the badge reads, so a
 * card's headline verdict and the numbers under it come out of one table and a
 * card cannot headline "consistent" two lines above nineteen write errors. Only
 * green and red deepen (see .verdict in components.css): a token that concludes
 * nothing keeps the row's own ink rather than inventing a third. The ink lands
 * on --pageRaised and never on a tinted fill — --successInk over the 10% wash
 * measures 4.16:1, under the floor, which is why no verdict here sits inside
 * .factory-step or a status wash.
 */
export function verdictAttributes(
  messages: Messages,
  token: string,
): { className: string; 'data-verdict': StatusColor } {
  return { className: 'verdict', 'data-verdict': resolveStatus(token, messages).color };
}

/**
 * How bad a verdict is, taken from the vocabulary's own output.
 *
 * Deliberately keyed on the resolved colour and not on a token's spelling:
 * STATUS_VOCABULARY stays the single place a token's meaning is declared, a
 * token added upstream sorts by the hue that table gives it, and the catch-all's
 * gray lands between "fine" and "wrong", which is what an unknown state is. A
 * card's verdict is the worst of its groups' verdicts, so a headline cannot be
 * greener than anything underneath it.
 */
const SEVERITY: Record<StatusColor, number> = { green: 0, blue: 0, gray: 1, orange: 2, red: 3 };

export function worstToken(messages: Messages, tokens: readonly string[]): string {
  let worst = tokens[0] ?? 'pending';
  for (const token of tokens) {
    if (
      SEVERITY[resolveStatus(token, messages).color] >
      SEVERITY[resolveStatus(worst, messages).color]
    ) {
      worst = token;
    }
  }
  return worst;
}

/**
 * The sentence a resolved state says, or null when the surface is simply there.
 *
 * `ok` writes nothing, which is why a polling surface holds exactly the geometry
 * it held on the previous poll: the happy path adds no node to remove.
 */
export function stateSentence<T>(
  messages: Messages,
  state: ResourceState<T>,
  emptyKey?: MessageKey,
): string | null {
  switch (state.state) {
    case 'loading':
      return messages.t('common.loading');
    // A section with no empty sentence is one whose payload is a single status
    // document, where `resolveState` has no count to call empty. Reached only if
    // that ever stops being true, and then it says nothing rather than saying a
    // list's words under a card.
    case 'empty':
    case 'filtered-empty':
      return emptyKey === undefined ? null : messages.t(emptyKey);
    case 'error':
      return messages.t('common.loadfail') + state.message;
    // The sentence is supplied already translated, because what a half-loaded
    // surface has to say is which half is still trustworthy — not the name of
    // the section it lost.
    case 'partial':
      return state.missing.join(' · ');
    case 'ok':
      return null;
  }
}

/**
 * A section whose payload is one status document rather than a list, so "empty"
 * is not a member it has: the document either arrived or it did not.
 */
export function cardState<T>(
  outcome: Parameters<typeof resolveState<T>>[0]['outcome'],
): ResourceState<T> {
  return resolveState<T>({ outcome });
}
