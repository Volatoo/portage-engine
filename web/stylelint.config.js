/**
 * A large part of what went wrong in the console this project replaces was CSS
 * nobody could lint. One container's `overflow-wrap: anywhere` inherited into a
 * grid child, starved a `1fr` track and broke the shell on every page, with go
 * build, go vet, go test and golangci-lint all green — because the stylesheet
 * was a Go string constant and no tool ever read it as CSS.
 *
 * So the rules below are in three groups. The first turns off the parts of
 * stylelint-config-standard that would rewrite values ported verbatim from
 * Apple's shipped stylesheet — provenance outranks house style. The second adds
 * the plain rules this project needs. The third wires up the five invariants
 * that used to live in Go and had to hand-parse a string constant to reach the
 * CSS: they are in ./stylelint-plugins/portage-engine.js, with the failure each
 * one refuses written above it.
 */

import portageEngine from './stylelint-plugins/portage-engine.js';

/**
 * Why a slot may spell `overflow-wrap: anywhere`. Grouped, because the reason is
 * a property of the kind of string the slot holds and not of the selector: a
 * blanket ban gets switched off within a week when two dozen slots genuinely
 * need it, and a list of two dozen individually-worded excuses does not get
 * read. Anything not on this list has to be added on purpose.
 */
const IDENTIFIER =
  'holds a machine-produced identifier — an atom, a digest, a path, an endpoint — with no break opportunity of its own';
const TRANSPORT_ERROR =
  'renders a raw transport or backend error string, which arrives as one unbroken token and used to scroll the whole document sideways';
const UPSTREAM_PROSE =
  'renders free text from an upstream payload, whose length this console does not control';

const OVERFLOW_WRAP_ANYWHERE = {
  /* Identifiers. */
  '.landing-flow .step .mono': IDENTIFIER,
  '.package-flags': IDENTIFIER,
  '.public-docs a': IDENTIFIER,
  '.project-context .identity': IDENTIFIER,
  '.device-identity': IDENTIFIER,
  'table.list td': IDENTIFIER,
  '.provider-status-row': IDENTIFIER,
  '.ledger-grid .builder-card .meta span': IDENTIFIER,
  '.target-card .meta span': IDENTIFIER,
  '.evidence-list span': IDENTIFIER,
  '.factory-step-head strong': IDENTIFIER,
  '.factory-step-log': IDENTIFIER,
  '.stage-log-card strong': IDENTIFIER,
  '.stage-log-card span': IDENTIFIER,
  '.milestone h3': IDENTIFIER,

  /* Transport and backend errors. */
  '.empty': TRANSPORT_ERROR,
  '.auth-err': TRANSPORT_ERROR,
  '.device-result': TRANSPORT_ERROR,
  '.save-msg': TRANSPORT_ERROR,
  '.log-fault': TRANSPORT_ERROR,
  '.failure-report h2': TRANSPORT_ERROR,
  '.failure-report p': TRANSPORT_ERROR,
  '.blocker strong': TRANSPORT_ERROR,
  '.blocker p': TRANSPORT_ERROR,

  /* Upstream prose. */
  '.status-row': UPSTREAM_PROSE,
  '.milestone p': UPSTREAM_PROSE,
  // The container the shell failure came from. It keeps `anywhere` because the
  // quota block genuinely renders unbroken provider names and paths; what makes
  // that safe is the pair of rules below it — .quota-k and .quota-dl dt override
  // back to break-word — so the inheritance stops at the grid child instead of
  // reaching it and collapsing the 1fr track.
  '.policy-summary': UPSTREAM_PROSE,
};

export default {
  extends: ['stylelint-config-standard'],
  plugins: [portageEngine],
  rules: {
    /* --- ported verbatim: formatting and notation are not ours to change --- */

    // The token block's annotations sit directly above the declarations they
    // explain, and its rules sit in the order they are read in. Blank-line rules
    // would reflow both.
    'comment-empty-line-before': null,
    'custom-property-empty-line-before': null,
    'declaration-empty-line-before': null,
    'rule-empty-line-before': null,
    'at-rule-empty-line-before': null,

    // rgba(0, 0, 0, .85) and hsla(0, 0%, 100%, .85) are Apple's own alpha ladder,
    // transcribed. Rewriting them to modern notation, to the rgb()/hsl() aliases,
    // to percentage alphas, to 0deg hues or to three-digit hex would make the file
    // no longer say what it says it says.
    'alpha-value-notation': null,
    'color-function-notation': null,
    'color-function-alias-notation': null,
    'color-hex-length': null,
    'hue-degree-notation': null,

    // The reset and the one-line rules are one line each in the source, and the
    // token block puts a shorthand and its dark-mode twin on matching lines so
    // the two are read side by side.
    'declaration-block-single-line-max-declarations': null,

    // --systemPrimary, --keyColor, --pageRaised: Apple's names for Apple's values.
    'custom-property-pattern': null,

    // (min-width: 1000px) rather than (width >= 1000px): the range syntax has a
    // shorter support floor than the browsers this console is expected on.
    'media-feature-range-notation': null,

    // Two kinds of proper noun live in these values: typeface names in the font
    // stacks (Helvetica, Arial, BlinkMacSystemFont) and CSS system colours in the
    // forced-colors block (Highlight). Both are conventionally capitalised, and
    // lowercasing either makes the rule harder to recognise, not easier.
    'value-keyword-case': null,

    // .78125rem and .71875rem are 12.5px and 11.5px at the 16px root — the two
    // half-step rungs the monospace slots take. Rounding them to four places
    // moves both by a fraction of a pixel and buys nothing.
    'number-max-precision': 5,

    // [type=text] rather than [type="text"]. The console's own attribute
    // selectors — [data-state], [aria-current] — are unquoted for the same
    // reason, so quoting only the typed inputs would make the file less
    // consistent, not more.
    'selector-attribute-quotes': null,

    // `table.list` is declared twice on purpose, and the second block carries
    // eight lines explaining why the min-width floors sit on the table and never
    // on the scroll wrapper. Folding them together would put that paragraph
    // above three declarations it does not describe.
    'no-duplicate-selectors': null,

    // `word-break: break-word` on the log pane is the legacy alias, and it is
    // not equivalent to the modern spelling: it breaks an overlong token
    // without contributing that break to min-content, which is precisely the
    // distinction the `overflow-wrap` rule below exists to police. Rewriting it
    // to the "correct" property would change the pane's minimum width.
    'declaration-property-value-keyword-no-deprecated': [true, { ignoreKeywords: ['break-word'] }],

    // Specificity here is deliberate and annotated at each site: `.ledger-grid
    // .builder-card h3` exists to out-specify `.builder-card h3`, and the second
    // `.counter` rule exists to out-specify the `min-width: 0` one line above it.
    // The rule cannot tell those from an accident and every one of them says in
    // writing which rule it is beating and why.
    'no-descending-specificity': null,

    // -webkit-font-smoothing and -moz-osx-font-smoothing have no unprefixed
    // form; ::-webkit-details-marker is how the default disclosure triangle is
    // suppressed, and the suppression is deliberate — the marker is drawn
    // outside the summary's own box and cannot be aligned with the rail's
    // gutter. Every other prefix stays banned.
    'property-no-vendor-prefix': [
      true,
      { ignoreProperties: ['font-smoothing', 'osx-font-smoothing'] },
    ],
    'selector-pseudo-element-no-unknown': [
      true,
      { ignorePseudoElements: ['-webkit-details-marker'] },
    ],

    /* --- this project's own plain rules --- */

    // One named radius set, and every use names it. A literal here is a radius
    // the token layer does not know about and a theme cannot move.
    'declaration-property-value-disallowed-list': {
      'border-radius': ['/^(?!.*var\\().*$/'],
    },

    /* --- the five invariants, from the plugin --- */

    // Colour. `color-no-hex` is deliberately not the mechanism: the ported
    // stylesheet darkens a token with `color-mix(in srgb, var(--accentFill),
    // #000 3%)`, which is an operation on a token rather than a colour pick, and
    // a rule that cannot tell those apart fires on correct code and gets
    // switched off. The plugin rule strips var() and color-mix() operands first.
    'color-no-hex': null,
    'portage-engine/no-color-literals': true,

    'portage-engine/token-references-resolve': true,
    'portage-engine/mode-blocks-agree': true,

    // Type sizes. The built-in `font-size` check cannot see the thirteen ramp
    // rungs, which are custom properties holding a `font` shorthand — which is
    // exactly the state this ramp was in when every rung was px and a
    // font-size-only gate read green.
    'portage-engine/type-sizes-are-relative': [
      true,
      {
        allow: {
          '.field input, .field select':
            'iOS Safari zooms the page when a focused input computes under 16px, and rem would follow a shrunken root',
        },
      },
    ],

    'portage-engine/overflow-wrap-anywhere-is-argued-for': [
      true,
      { allow: OVERFLOW_WRAP_ANYWHERE },
    ],
  },
  overrides: [
    {
      // The one file allowed to spell values: it is the token layer, and the
      // values in it are cited.
      files: ['src/styles/tokens.css'],
      rules: {
        'portage-engine/no-color-literals': null,
        'declaration-property-value-disallowed-list': null,
      },
    },
  ],
};
