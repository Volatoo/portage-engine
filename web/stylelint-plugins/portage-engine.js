/**
 * The console's own stylesheet invariants, as stylelint rules.
 *
 * These five checks lived in Go (internal/dashboard/css_invariants_test.go),
 * where they had to hand-parse a string constant to reach the CSS at all. Each
 * of them exists because the class of defect it refuses shipped at least once:
 * three tokens were referenced and declared nowhere, four dark-mode tokens were
 * missing so elevation silently stopped existing, seven radii were literals, and
 * the whole type ramp was in px. Now that the stylesheet is a stylesheet, the
 * checks belong to the tool that reads it as one.
 *
 * Two of them are cross-file — a component in components.css names a token
 * declared in tokens.css — and stylelint lints one file at a time. Those read
 * the sibling stylesheets off disk once and cache the result, keyed by the
 * directory the linted file is in, so the rule sees the same set the browser
 * will after index.css has done its imports.
 */

import { readFileSync, readdirSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';

import stylelint from 'stylelint';

const {
  createPlugin,
  utils: { report, ruleMessages },
} = stylelint;

const NAMESPACE = 'portage-engine';

/**
 * The dark mode block, recognised by what it asks rather than by how it is typed.
 *
 * This was a string comparison against `(prefers-color-scheme: dark)`, and the
 * two other legal spellings of the same query — `(prefers-color-scheme:dark)`
 * and `screen and (prefers-color-scheme: dark)` — did not merely go unmatched.
 * They switched the rule off: with no block recognised as the dark one, every
 * token in the file is light-only with nothing to compare it against, and the
 * check that exists because four dark tokens went missing reported nothing while
 * two more were deleted. A negated query is not the dark block either, so `not`
 * anywhere in the prelude disqualifies it.
 */
const DARK_FEATURE = /\(\s*prefers-color-scheme\s*:\s*dark\s*\)/i;
const NEGATION = /(^|[\s(])not(\s|$)/i;

function isDarkQuery(atRule) {
  return (
    atRule.name.toLowerCase() === 'media' &&
    DARK_FEATURE.test(atRule.params) &&
    !NEGATION.test(atRule.params)
  );
}

/**
 * Colour, in the notations this stylesheet uses. `rgba(` followed by a digit
 * rather than `rgba(` alone, because `rgba(var(--keyColor-rgb), …)` is an alpha
 * applied to a token and not a colour pick.
 */
const COLOUR_LITERAL = /#[0-9a-fA-F]{3,8}\b|rgba?\(\s*[0-9]|hsla?\(\s*[0-9]|oklch\(|lab\(|lch\(/;

/**
 * The CSS named colours, which are colour literals that happen to be spelled in
 * words — and were the hole in the rule: `color: red` passed a check built out
 * of hex and functional notations, and `red` is precisely the frozen light-mode
 * value this rule exists to refuse. `transparent` and `currentcolor` are absent
 * on purpose: neither picks a hue, and `currentcolor` follows whichever token
 * the ink came from, which is the behaviour being asked for.
 */
const NAMED_COLOURS = `
  aliceblue antiquewhite aqua aquamarine azure beige bisque black blanchedalmond blue blueviolet
  brown burlywood cadetblue chartreuse chocolate coral cornflowerblue cornsilk crimson cyan
  darkblue darkcyan darkgoldenrod darkgray darkgreen darkgrey darkkhaki darkmagenta darkolivegreen
  darkorange darkorchid darkred darksalmon darkseagreen darkslateblue darkslategray darkslategrey
  darkturquoise darkviolet deeppink deepskyblue dimgray dimgrey dodgerblue firebrick floralwhite
  forestgreen fuchsia gainsboro ghostwhite gold goldenrod gray green greenyellow grey honeydew
  hotpink indianred indigo ivory khaki lavender lavenderblush lawngreen lemonchiffon lightblue
  lightcoral lightcyan lightgoldenrodyellow lightgray lightgreen lightgrey lightpink lightsalmon
  lightseagreen lightskyblue lightslategray lightslategrey lightsteelblue lightyellow lime
  limegreen linen magenta maroon mediumaquamarine mediumblue mediumorchid mediumpurple
  mediumseagreen mediumslateblue mediumspringgreen mediumturquoise mediumvioletred midnightblue
  mintcream mistyrose moccasin navajowhite navy oldlace olive olivedrab orange orangered orchid
  palegoldenrod palegreen paleturquoise palevioletred papayawhip peachpuff peru pink plum
  powderblue purple rebeccapurple red rosybrown royalblue saddlebrown salmon sandybrown seagreen
  seashell sienna silver skyblue slateblue slategray slategrey snow springgreen steelblue tan teal
  thistle tomato turquoise violet wheat white whitesmoke yellow yellowgreen
`
  .trim()
  .split(/\s+/);

// Bounded by "not a word character and not a hyphen" on both sides, so a name
// cannot be found inside a longer identifier: --nav-red and mediumvioletred are
// each one token, not a token containing `red`.
const NAMED_COLOUR = new RegExp(`(^|[^\\w-])(${NAMED_COLOURS.join('|')})([^\\w-]|$)`, 'i');

/** Does this text pick a colour, in any notation? */
function spellsAColour(text) {
  return COLOUR_LITERAL.test(text) || NAMED_COLOUR.test(text);
}

/**
 * A token whose own value carries a colour, as opposed to one built out of other
 * tokens (--keyline, --controlBG). The second half is the bare `255, 59, 48`
 * channel triple --systemRed-rgb holds.
 */
const CHANNEL_TRIPLE = /^\s*[0-9]{1,3}\s*,\s*[0-9]{1,3}\s*,\s*[0-9]{1,3}\s*$/;

/**
 * A value reduced to the part of it that can pick a colour.
 *
 * Quoted text goes first: `content: " !"` and the typeface names in a font stack
 * are strings that happen to live in a declaration, and a colour word inside one
 * — "Red Hat Mono" — names a typeface. Naming colours in words is what made that
 * reachable at all, so it is handled here rather than left as the first false
 * positive that gets the rule switched off.
 */
const QUOTED = /"[^"]*"|'[^']*'/g;

function scannableValue(value) {
  return withoutTokenExpressions(value.replace(QUOTED, ' '));
}

function isColourValue(value) {
  return spellsAColour(scannableValue(value)) || CHANNEL_TRIPLE.test(value);
}

const TOKEN_REFERENCE = /var\(\s*(--[A-Za-z0-9_-]+)/g;
const TOKEN_DECLARATION = /(--[A-Za-z0-9_-]+)\s*:/g;

/**
 * A length in px or pt, anchored so a fragment of an identifier or of a longer
 * number cannot match. The Go original's anchor, kept: `1.5px` matches, `x1px`
 * does not.
 */
const ABSOLUTE_LENGTH = /(^|[\s/(,])[0-9]*\.?[0-9]+(px|pt)\b/;

/** The ramp rungs are custom properties holding a font shorthand. */
const FONT_SHORTHAND = /var\(--font-(family|mono)\)/;

/**
 * What is left of a value once the expressions that OPERATE on a token are out
 * of it, so that what remains is what the value spells for itself.
 *
 * Deleting `var(…)` and `color-mix(…)` whole — which is what this did — threw
 * away the justification for the exemption along with the operands. A fallback
 * is a colour that renders whenever the token is missing, so
 * `var(--x, #ff0000)` is a frozen red that never reached the scan; and
 * `color-mix(in srgb, #123456, #fff 10%)` names no token at all, which makes it
 * a colour pick wearing the syntax of an operation.
 *
 * So a token reference loses its NAME and keeps its fallback, and a mix is
 * removed only when it is genuinely an operation on a token: it names one, and
 * every colour it spells outright is achromatic — the darken this stylesheet
 * actually does (`color-mix(in srgb, var(--accentFill), #000 3%)`), where the
 * literal is the direction of the operation and moves with the token in dark
 * mode. Anything else keeps its operands and is scanned like any other value.
 */
const ACHROMATIC = /^(#000|#fff|#000000|#ffffff|black|white|transparent)$/i;
const TOKEN_CALL = /(^|[^\w-])(var|color-mix)\(/;
const NAMES_A_TOKEN = /(^|[^\w-])var\(\s*--/;

/** The index of the `)` closing the `(` at `open`, or the end of the text. */
function closingParen(text, open) {
  let depth = 0;
  for (let index = open; index < text.length; index += 1) {
    if (text[index] === '(') {
      depth += 1;
    } else if (text[index] === ')') {
      depth -= 1;
      if (depth === 0) {
        return index;
      }
    }
  }
  return text.length;
}

/** Split at top-level commas, so a nested call stays one operand. */
function operands(text) {
  const parts = [];
  let depth = 0;
  let start = 0;
  for (let index = 0; index < text.length; index += 1) {
    const character = text[index];
    if (character === '(') {
      depth += 1;
    } else if (character === ')') {
      depth -= 1;
    } else if (character === ',' && depth === 0) {
      parts.push(text.slice(start, index));
      start = index + 1;
    }
  }
  parts.push(text.slice(start));
  return parts;
}

function withoutTokenExpressions(value) {
  const call = TOKEN_CALL.exec(value);
  if (call === null) {
    return value;
  }
  const open = call.index + call[0].length - 1;
  const close = closingParen(value, open);
  const inner = value.slice(open + 1, close);
  const head = value.slice(0, open - (call[2] ?? '').length);
  const tail = withoutTokenExpressions(value.slice(close + 1));
  const args = operands(inner).map(withoutTokenExpressions);

  if (call[2] === 'var') {
    // The name is a reference and everything after the first comma is a value.
    return `${head} ${args.slice(1).join(',')} ${tail}`;
  }
  const mixed = args.join(',');
  const modifiesAToken =
    NAMES_A_TOKEN.test(inner) &&
    !spellsAColour(
      mixed
        .split(/[\s,]+/)
        .filter((part) => !ACHROMATIC.test(part))
        .join(' '),
    );
  return `${head} ${modifiesAToken ? '' : mixed} ${tail}`;
}

/**
 * The source root a stylesheet belongs to: the nearest ancestor named `src`, so
 * a component stylesheet sitting next to its component is measured against the
 * same token set as one in src/styles. A file outside any `src` falls back to
 * its own directory, which is the conservative answer rather than a walk to `/`.
 */
function sourceRoot(cssPath) {
  let directory = dirname(cssPath);
  for (let hop = 0; hop < 32; hop += 1) {
    if (basename(directory) === 'src') {
      return directory;
    }
    const parent = dirname(directory);
    if (parent === directory) {
      break;
    }
    directory = parent;
  }
  return dirname(cssPath);
}

/** Every custom property any stylesheet under the same source root declares. */
const declaredTokensByRoot = new Map();

function collectTokens(directory, names) {
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      collectTokens(path, names);
    } else if (entry.name.endsWith('.css')) {
      const text = readFileSync(path, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
      for (const match of text.matchAll(TOKEN_DECLARATION)) {
        names.add(match[1]);
      }
    }
  }
}

function declaredTokens(cssPath) {
  const root = sourceRoot(cssPath);
  const cached = declaredTokensByRoot.get(root);
  if (cached !== undefined) {
    return cached;
  }
  const names = new Set();
  collectTokens(root, names);
  declaredTokensByRoot.set(root, names);
  return names;
}

/** The at-rule chain above a node, outermost first. */
function atRuleChain(node) {
  const chain = [];
  for (let parent = node.parent; parent != null; parent = parent.parent) {
    if (parent.type === 'atrule') {
      chain.unshift(parent);
    }
  }
  return chain;
}

function definePlugin(name, messages, walk) {
  const ruleName = `${NAMESPACE}/${name}`;
  const built = ruleMessages(ruleName, messages);
  const rule = (primary, secondary) => (root, result) => {
    if (primary === false || primary == null) {
      return;
    }
    walk(root, result, ruleName, built, secondary ?? {});
  };
  rule.ruleName = ruleName;
  rule.messages = built;
  rule.meta = { url: 'web/stylelint-plugins/portage-engine.js' };
  return createPlugin(ruleName, rule);
}

/**
 * A component names a token; it never spells a value.
 *
 * The failure this refuses: the failed pipeline chip carried a frozen
 * light-mode red, so in dark it missed the alpha multiplier its neighbouring
 * chips got and the one stage an operator is hunting for rendered weaker than
 * the stages either side of it.
 */
const noColourLiterals = definePlugin(
  'no-color-literals',
  {
    rejected: (property, value) =>
      `"${property}: ${value}" picks a colour. Colour is declared in tokens.css and named from here.`,
  },
  (root, result, ruleName, messages) => {
    root.walkDecls((decl) => {
      if (!spellsAColour(scannableValue(decl.value))) {
        return;
      }
      report({
        result,
        ruleName,
        node: decl,
        message: messages.rejected(decl.prop, decl.value),
      });
    });
  },
);

/**
 * Every var(--x) resolves to a token declared somewhere in this stylesheet.
 *
 * An undeclared token does not fall back to anything sensible: the whole
 * declaration is dropped. `border: 1px solid var(--separator)` computed to no
 * border at all, and `font: var(--caption-1)` left the label at inherited body
 * size — both silent, both shipped.
 */
const tokenReferencesResolve = definePlugin(
  'token-references-resolve',
  {
    rejected: (token, property) =>
      `${property} references ${token}, which no stylesheet declares. An undeclared token drops the whole declaration.`,
  },
  (root, result, ruleName, messages) => {
    const source = root.source?.input?.file;
    if (source == null) {
      return;
    }
    const declared = declaredTokens(source);
    root.walkDecls((decl) => {
      for (const match of decl.value.matchAll(TOKEN_REFERENCE)) {
        if (declared.has(match[1])) {
          continue;
        }
        report({
          result,
          ruleName,
          node: decl,
          message: messages.rejected(match[1], decl.prop),
        });
      }
    });
  },
);

/**
 * The two mode blocks declare the same token names, and the dark block declares
 * nothing else.
 *
 * --keyColor, --shadow-small and --shadow-medium were all light-only once: dark
 * mode ran a light-mode blue beside a remapped one, and its 8%-black shadows
 * resolved to a 2/255 shift on a #151515 ground — elevation that was not there
 * at all, which is what five per-component dark repaints existed to compensate
 * for. A dark rule on a component is a token the layer could not express.
 */
const modeBlocksAgree = definePlugin(
  'mode-blocks-agree',
  {
    lightOnly: (token, value) => `${token} is a colour (${value}) declared only in light mode.`,
    darkOnly: (token) => `${token} is declared only in the dark block, so light mode has no value.`,
    componentRule: (selector, property) =>
      `"${selector} { ${property} }" is a component rule inside the dark block; remap a token instead.`,
    noDarkBlock: (token) =>
      `${token} is a colour and this file has no @media (prefers-color-scheme: dark) block, so every colour in it is light-only.`,
  },
  (root, result, ruleName, messages) => {
    const light = new Map();
    const dark = new Map();
    let darkBlockSeen = false;

    root.walkDecls((decl) => {
      const chain = atRuleChain(decl);
      const rule = decl.parent;
      if (rule?.type !== 'rule') {
        return;
      }
      const inDark = chain.length === 1 && isDarkQuery(chain[0]);
      if (inDark) {
        darkBlockSeen = true;
      }
      // The @supports fallback declares its own :root and is deliberately out of
      // scope: it patches two rungs for non-Apple platforms, it is not a mode.
      if (rule.selector !== ':root') {
        if (inDark) {
          report({
            result,
            ruleName,
            node: decl,
            message: messages.componentRule(rule.selector, decl.prop),
          });
        }
        return;
      }
      if (!decl.prop.startsWith('--')) {
        return;
      }
      if (chain.length === 0) {
        light.set(decl.prop, decl.value);
      } else if (inDark) {
        dark.set(decl.prop, decl.value);
      }
    });

    const colours = [...light].filter(([, value]) => isColourValue(value));
    if (!darkBlockSeen) {
      // Returning quietly here is how the rule used to disable itself: a file
      // that declares the palette and has no dark block is not a file with
      // nothing to check, it is the defect this rule is named for. Keyed on the
      // file's own contents rather than on its path, so it holds for whichever
      // stylesheet declares the light-mode colours. A component sheet declares
      // no :root colour at all and is asked for nothing.
      if (colours.length > 0) {
        const [[token]] = colours;
        report({ result, ruleName, node: root, message: messages.noDarkBlock(token) });
      }
      return;
    }
    for (const [token, value] of colours) {
      if (!dark.has(token)) {
        report({ result, ruleName, node: root, message: messages.lightOnly(token, value) });
      }
    }
    for (const token of dark.keys()) {
      if (!light.has(token)) {
        report({ result, ruleName, node: root, message: messages.darkOnly(token) });
      }
    }
  },
);

/**
 * Type sizes are relative, including the ramp rungs.
 *
 * Checking only `font-size` reads as a passing gate while the thirteen rungs
 * the gate exists for sit in px inside a `font` shorthand, which is exactly the
 * state this stylesheet was in. The allowlist is keyed by selector so an
 * exemption cannot spread to the next rule that wants the same number.
 */
const typeSizesAreRelative = definePlugin(
  'type-sizes-are-relative',
  {
    rejected: (property, value) =>
      `"${property}: ${value}" sizes type in px. The ramp is rem against the root, so a reader who raises the browser font size moves all of it.`,
  },
  (root, result, ruleName, messages, options) => {
    const allowed = options.allow ?? {};
    root.walkDecls((decl) => {
      const sizesType =
        decl.prop === 'font' || decl.prop === 'font-size' || FONT_SHORTHAND.test(decl.value);
      if (!sizesType || !ABSOLUTE_LENGTH.test(decl.value)) {
        return;
      }
      const selector = decl.parent?.type === 'rule' ? decl.parent.selector : '';
      if (Object.prototype.hasOwnProperty.call(allowed, selector)) {
        return;
      }
      report({
        result,
        ruleName,
        node: decl,
        message: messages.rejected(decl.prop, decl.value),
      });
    });
  },
);

/**
 * `overflow-wrap: anywhere` is argued for by name, or it does not land.
 *
 * This is the declaration that broke the shell. It differs from `break-word`
 * precisely in that it also counts its break opportunities toward the element's
 * min-content size, and it inherits — which is how a caption's wrapping rule
 * reached a grid child, starved a `1fr` track, and rendered a nine-character
 * label as a six-line column 9.7px wide. A blanket ban would have been switched
 * off within a week, because two dozen slots in this console genuinely carry an
 * unbreakable machine-produced string. So each slot is listed, with the reason
 * it is one, and a slot that is not on the list has to be added deliberately.
 */
const overflowWrapAnywhereIsArguedFor = definePlugin(
  'overflow-wrap-anywhere-is-argued-for',
  {
    rejected: (selector) =>
      `"${selector}" declares overflow-wrap: anywhere and is not on the allowlist. anywhere changes the element's min-content size and inherits; say which slot needs it and why, or use break-word.`,
  },
  (root, result, ruleName, messages, options) => {
    const allowed = options.allow ?? {};
    root.walkDecls((decl) => {
      if (decl.prop !== 'overflow-wrap' || decl.value.trim() !== 'anywhere') {
        return;
      }
      const selector = decl.parent?.type === 'rule' ? decl.parent.selector : '';
      if (Object.prototype.hasOwnProperty.call(allowed, selector)) {
        return;
      }
      report({ result, ruleName, node: decl, message: messages.rejected(selector) });
    });
  },
);

export default [
  noColourLiterals,
  tokenReferencesResolve,
  modeBlocksAgree,
  typeSizesAreRelative,
  overflowWrapAnywhereIsArguedFor,
];
