/**
 * Where the dark mode block begins, spelled once for everything that reads the
 * stylesheet as text.
 *
 * Two test helpers each sliced tokens.css on the literal
 * `@media (prefers-color-scheme: dark)`, and so did the stylelint rule they back
 * up — so all three shared one blind spot rather than covering for each other.
 * `@media (prefers-color-scheme:dark)` and `@media screen and
 * (prefers-color-scheme: dark)` are the same query, both legal, and under either
 * of them the three checks find no dark block and quietly measure nothing.
 *
 * Matching the feature with the whitespace it is allowed is what makes a
 * reformat a reformat. The plugin's own `isDarkQuery` says the same thing about
 * the parsed at-rule, which is the form it has there.
 */
export const DARK_BLOCK = /@media[^{}]*\(\s*prefers-color-scheme\s*:\s*dark\s*\)[^{}]*\{/;
