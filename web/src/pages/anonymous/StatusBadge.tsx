/**
 * The public surface's badge is the console's badge.
 *
 * The status page this replaces carried a `statusIndicator` of its own whose
 * catch-all was red plus the word "Degraded", so a component state added
 * upstream — a maintenance window, a new dependency reporting `unknown` — was
 * announced to anonymous readers as a fault. Everything that wears a dot here
 * resolves through the shared vocabulary instead, which is what makes the
 * catch-all gray and the word the token's own.
 *
 * Re-exported rather than re-implemented: these three pages are read by people
 * with no account and no way to ask what a colour meant, so the badge they see
 * has to be the one the operator pages were tested with.
 */

export { StatusBadge } from '../../components/StatusBadge';
export type { StatusBadgeProps } from '../../components/StatusBadge';
