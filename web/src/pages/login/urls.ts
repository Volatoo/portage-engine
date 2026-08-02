/**
 * The sign-in URLs the console builds by hand, spelled once.
 *
 * None of these is an API call, which is why they are here and not in the
 * endpoint registry: `/login` is the dashboard's own page-and-credential route
 * and `/auth/...` hands the browser to a provider, so all three are navigations.
 * What they share with the registry is the reason it exists — the console this
 * replaces spelled `/login?return_to=` at four call sites, each assembling the
 * query with `+` and each with its own idea of what to encode.
 *
 * `safeReturnTo` mirrors the function of the same name in
 * internal/dashboard/dashboard.go. The server sanitises again on the way in and
 * is the authority; this copy exists because the console also puts the value
 * into a form action and an anchor href, which the server never sees until the
 * navigation has already been offered to the reader.
 */

/** Where the local credential POST goes: the dashboard owns /login at the root. */
export const LOCAL_LOGIN_ENDPOINT = '/login';

/**
 * The single-provider start route. internal/dashboard/dashboard.go registers it
 * unconditionally and resolves it against the first configured provider, so it
 * is the one federated destination that exists on every deployment with OIDC
 * enabled — including one whose provider names the console has not been told.
 */
export const LEGACY_PROVIDER_START = '/auth/oidc/start';

export interface LoginTarget {
  /** The path being decorated: a login page, or a provider's start route. */
  loginPath: string;
  /** Ask the provider to authenticate again rather than reuse its session. */
  stepUp: boolean;
  /** Where to land afterwards. Sanitised here; '' means "the default". */
  returnTo: string;
}

/** A code point no URL may carry unescaped: it splits the URL or ends a header. */
function isForbiddenInURL(character: string): boolean {
  const code = character.codePointAt(0) ?? 0;
  return code < 0x20 || code === 0x7f;
}

/**
 * A same-origin path, or nothing.
 *
 * Stricter than the Go original in one place: a leading `/\` is rejected as well
 * as a leading `//`. Go's url.Parse leaves `/\evil.test` as a relative path, and
 * every browser's URL parser treats that backslash as a solidus — so the one
 * spelling that survives the server's check is the one that navigates
 * off-origin. Everything else is refused for the reason the Go side refuses it:
 * an open redirect out of a sign-in page is a credential-phishing primitive.
 */
export function safeReturnTo(raw: string): string {
  const trimmed = raw.trim();
  if (trimmed === '' || !trimmed.startsWith('/')) {
    return '';
  }
  if (trimmed.startsWith('//') || trimmed.startsWith('/\\')) {
    return '';
  }
  for (const character of trimmed) {
    if (isForbiddenInURL(character)) {
      return '';
    }
  }
  return trimmed;
}

/**
 * `loginURLWithContext` from internal/dashboard/dashboard.go, in the browser.
 *
 * `URLSearchParams` rather than string concatenation is the whole point: a
 * return path with a query of its own — `/ui/device?user_code=ABCD-2345` is the
 * one this console actually produces — has to survive being carried inside
 * another query, and the console this replaces lost everything after the inner
 * `?` at two of its four call sites.
 */
export function loginURL(target: LoginTarget): string {
  const query = new URLSearchParams();
  if (target.stepUp) {
    query.set('step_up', '1');
  }
  const returnTo = safeReturnTo(target.returnTo);
  if (returnTo !== '') {
    query.set('return_to', returnTo);
  }
  const encoded = query.toString();
  return encoded === '' ? target.loginPath : `${target.loginPath}?${encoded}`;
}

/** internal/dashboard/dashboard.go pageData: /auth/provider/<id>/start. */
export function providerStartURL(providerID: string): string {
  return `/auth/provider/${encodeURIComponent(providerID)}/start`;
}
