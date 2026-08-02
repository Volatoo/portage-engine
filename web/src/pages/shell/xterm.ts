/**
 * The vendored terminal, loaded the way it is served.
 *
 * xterm is a vendored dependency: internal/dashboard embeds `assets/xterm.min.js`
 * and `assets/xterm.css` and serves them at `/static/xterm.js` and
 * `/static/xterm.css`, and it is reported rather than edited. It is not an npm
 * dependency of this bundle and deliberately not made one here — bundling a
 * second copy would ship the terminal twice on a deployment whose old console is
 * still serving the first, and the two could drift.
 *
 * So it is attached as a global script, once per document, and the surface below
 * is exactly the four members this page uses. Typing it here rather than reaching
 * for `any` is what makes the rest of the page type-check against something: a
 * vendored global with no declaration is a hole every call site falls into.
 */

/** The constructor `/static/xterm.js` puts on window. */
export interface VendoredTerminal {
  open(container: HTMLElement): void;
  write(data: string | Uint8Array): void;
  onData(handler: (data: string) => void): void;
  focus(): void;
  dispose(): void;
}

export interface TerminalOptions {
  fontSize: number;
  fontFamily: string;
  cursorBlink: boolean;
  cols: number;
  rows: number;
  theme: { background: string };
}

type TerminalConstructor = new (options: TerminalOptions) => VendoredTerminal;

declare global {
  interface Window {
    Terminal?: TerminalConstructor;
  }
}

const SCRIPT_URL = '/static/xterm.js';
const STYLESHEET_URL = '/static/xterm.css';

/**
 * The terminal's geometry, fixed.
 *
 * internal/server/handlers_shell.go opens the SSH command with `stty cols 220
 * rows 50` hardcoded, so this is not a preference — it is the size the remote
 * side is already drawing at, and any other value here would render a screen the
 * builder is not producing. Making it follow the viewport needs a resize message
 * on the socket and a matching change on the server; until then the screen is
 * wider than every viewport and is scrolled inside its own box, which is the
 * half of the fix that needs no protocol change.
 */
export const TERMINAL_COLUMNS = 220;
export const TERMINAL_ROWS = 50;

/**
 * The PTY's own ground, in both modes.
 *
 * A remote program draws its colours against a fixed black background and picks
 * them assuming it — this is the fixed-dark-surface exception, not a theme
 * colour, which is why it is stated here as a terminal option rather than added
 * to the token layer as a token nothing else would ever bind.
 */
const TERMINAL_BACKGROUND = '#000000';

export const TERMINAL_OPTIONS: TerminalOptions = {
  fontSize: 13,
  fontFamily: 'ui-monospace, SF Mono, Menlo, monospace',
  cursorBlink: true,
  cols: TERMINAL_COLUMNS,
  rows: TERMINAL_ROWS,
  theme: { background: TERMINAL_BACKGROUND },
};

let loading: Promise<TerminalConstructor> | null = null;

function attach<T extends HTMLElement>(element: T): Promise<void> {
  return new Promise((resolve, reject) => {
    element.addEventListener('load', () => {
      resolve();
    });
    element.addEventListener('error', () => {
      reject(new Error('console: the vendored terminal could not be loaded'));
    });
    document.head.appendChild(element);
  });
}

/**
 * Load the terminal once per document.
 *
 * The promise is cached rather than the constructor: two mounts in the same
 * frame — which StrictMode produces on every development mount — would otherwise
 * append two copies of a 300KB script.
 */
export function loadTerminal(): Promise<TerminalConstructor> {
  const alreadyThere = window.Terminal;
  if (alreadyThere !== undefined) {
    return Promise.resolve(alreadyThere);
  }
  loading ??= (async () => {
    const stylesheet = document.createElement('link');
    stylesheet.rel = 'stylesheet';
    stylesheet.href = STYLESHEET_URL;
    const script = document.createElement('script');
    script.src = SCRIPT_URL;
    // The stylesheet is not awaited: a terminal drawn before its rules land is
    // a terminal, and a terminal that never appears because a stylesheet 404ed
    // is not. The script is the load-bearing half.
    void attach(stylesheet).catch(() => undefined);
    await attach(script);
    const constructor = window.Terminal;
    if (constructor === undefined) {
      throw new Error('console: /static/xterm.js loaded and defined no Terminal');
    }
    return constructor;
  })();
  return loading;
}
