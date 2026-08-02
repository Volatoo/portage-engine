/**
 * A status, never as colour alone. One implementation, for every surface.
 *
 * Three arrived from the parallel port — builds/parts.tsx, monitor/parts.tsx and
 * anonymous/StatusBadge.tsx — agreeing on the markup by coincidence and on
 * nothing that would keep them agreeing. They are the two rules this console is
 * measured on most directly, so three copies meant three places each rule could
 * break on its own:
 *
 *   - the dot carries the hue and the word beside it carries the status, so a
 *     reader in forced-colors mode, where every dot resolves to the same ink,
 *     reads the same fact as everyone else;
 *   - `resolveStatus` is the single lookup, with a documented catch-all — a
 *     token shipped by a backend newer than this bundle comes back gray under
 *     its own raw name, rather than confidently coloured as whichever branch a
 *     conditional chain happened to end on.
 *
 * The two overrides are different facts and are kept apart on purpose. `label`
 * renames a KNOWN token — the ledger card calling its healthy state "consistent"
 * — and deliberately cannot touch the hue, so a card cannot disagree with the
 * token it was judged by. `token={null}` is the payload carrying no such field
 * at all, which is not a status this console has never heard of; it takes
 * `absentLabel` and never a token invented on the payload's behalf.
 */

import { useMessages } from '../i18n/context';
import { resolveStatus } from '../i18n/status';

export interface StatusBadgeProps {
  /** The backend's own token, or `null` when the payload carries no such field. */
  token: string | null;
  /** Renames a known token's word. Never its colour. */
  label?: string;
  /** The word for `token={null}`. Not a status: the absence of one. */
  absentLabel?: string;
}

export function StatusBadge({ token, label, absentLabel }: StatusBadgeProps) {
  const messages = useMessages();
  const presentation =
    token === null
      ? { color: 'gray' as const, label: absentLabel ?? '-' }
      : resolveStatus(token, messages);
  return (
    <span className={`status ${presentation.color}`}>
      <span className="dot" />
      <span>{label ?? presentation.label}</span>
    </span>
  );
}
