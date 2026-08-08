/**
 * The boot payload contract.
 *
 * The Go struct `webassets.Boot` (internal/dashboard/webassets/webassets.go) is
 * the source of truth for these field names; this file mirrors it. Field names
 * are snake_case because every other JSON surface the console reads — the IAM
 * principal, the job ledger, the scheduler status — is snake_case, and a boot
 * payload spelled differently would be the one object in the app needing its own
 * casing rule.
 *
 * The server writes this into the shell as a synchronous `<script>` ahead of the
 * module script, so the bundle reads it before its first render. That is what
 * keeps the language resolved before first paint and what lets a deep link
 * render its frame without a round trip for the id already in the URL.
 */

// Type-only, so this stays a leaf module at runtime: the import is erased and no
// edge from boot to api survives compilation. The union is declared where the
// wire types live because the same three values arrive in a step-up refusal.
import type { StepUpMethod } from '../api/types';

/** The two languages the console ships strings for. */
export const LANGUAGES = ['en', 'zh'] as const;
export type Language = (typeof LANGUAGES)[number];

/**
 * Who the server already knows the reader to be. `null` means "not resolved
 * here" — not "anonymous". The dashboard can name a local session's subject
 * without leaving the process, but a federated session's principal lives in the
 * control plane and costs a round trip, so it arrives later over /api/iam/me.
 *
 * This object carries identity, never authority. No capability is asserted from
 * the boot payload: a destination that needs one stays hidden until IAM answers,
 * so an IAM outage leaves it hidden rather than revealing it and taking it back.
 */
export interface BootPrincipal {
  subject: string;
  preferred_username: string;
  provider_id: string;
  /** How the session was established, e.g. "local-session". */
  authentication: string;
}

/**
 * What the server could resolve about the route from the URL alone.
 *
 * Every field is always present. An id the route does not carry is the empty
 * string rather than an absent key, so the bundle reads one shape and no code
 * path branches on whether the server bothered to send a field.
 */
export interface BootRoute {
  /** Route name from the server's route table, not derived from the path. */
  name: string;
  path: string;
  job_id: string;
  instance_id: string;
  user_code: string;
}

/**
 * A federated sign-in destination the deployment has initialised.
 *
 * The URL is not here on purpose. The id is what `/auth/provider/<id>/start` is
 * built from, and where the reader lands afterwards — and whether the trip is a
 * step-up — belongs to the page offering the button rather than to the
 * deployment.
 */
export interface BootIdentityProvider {
  id: string;
  display_name: string;
  /**
   * Whether asking this provider to prove the reader again means anything. The
   * server sends `prompt=login` to an OIDC provider and cannot to a GitHub OAuth
   * app, which has no such parameter — so a GitHub button on a step-up card
   * returns a session exactly as old as the one that was just refused.
   */
  supports_step_up: boolean;
}

export interface BootPayload {
  lang: Language;
  /** The BCP 47 tag on <html lang>, e.g. "zh-CN" for lang "zh". */
  html_lang: string;
  auth_enabled: boolean;
  oidc_enabled: boolean;
  local_login_enabled: boolean;
  /**
   * Every provider this deployment can complete a sign-in through, in
   * configuration order. Empty on a deployment configured the pre-multi-provider
   * way, whose one unnamed provider answers at `/auth/oidc/start`; `oidc_enabled`
   * is what stays true there.
   */
  identity_providers: BootIdentityProvider[];
  principal: BootPrincipal | null;
  /**
   * The credential that could satisfy a step-up for this session.
   *
   * Beside `principal` rather than inside it, because `principal` is null for a
   * federated session — naming that principal costs the server an upstream round
   * trip — and this fact does not. Reading the method off the principal read
   * nothing on exactly the deployments where the answer is `federated`, and the
   * page then offered a local administrator prompt that session can never pass.
   *
   * `unstated` is a payload that did not say, which is a server older than this
   * bundle. It offers no credential rather than guessing at one.
   */
  step_up_method: StepUpMethod | 'unstated';
  route: BootRoute;
  /** URL prefix the hashed asset tree is mounted at, e.g. "/static/ui/". */
  asset_base: string;
}

/** The global the server stamps the payload onto. */
export const BOOT_GLOBAL = '__PE_BOOT__';

/** The attribute the server stamps the resolved language onto. */
export const LANG_ATTRIBUTE = 'data-pe-lang';
