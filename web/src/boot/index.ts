import {
  BOOT_GLOBAL,
  LANG_ATTRIBUTE,
  LANGUAGES,
  type BootIdentityProvider,
  type BootPayload,
  type BootPrincipal,
  type BootRoute,
  type Language,
} from './payload';
import type { StepUpMethod } from '../api/types';

declare global {
  interface Window {
    /**
     * Deliberately `unknown`. The shell is server-rendered and the payload is
     * trusted in practice, but typing the global as BootPayload would let every
     * call site read a field the server never sent and get no complaint.
     * coerceBootPayload below is the only thing allowed to narrow it.
     */
    __PE_BOOT__?: unknown;
  }
}

/** Language key to the BCP 47 tag that belongs in <html lang>. */
const LANGUAGE_TAGS: Record<Language, string> = { en: 'en', zh: 'zh-CN' };

const EMPTY_ROUTE: BootRoute = {
  name: '',
  path: '',
  job_id: '',
  instance_id: '',
  user_code: '',
};

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function readText(source: Record<string, unknown> | null, key: string): string {
  const value = source?.[key];
  return typeof value === 'string' ? value : '';
}

/**
 * Anything that is not literally `true` is false. A truthy string or a 1 from a
 * payload that drifted would otherwise turn a flag on, and every flag here
 * enables something: an off-by-default flag fails in the safe direction.
 */
function readFlag(source: Record<string, unknown> | null, key: string): boolean {
  return source?.[key] === true;
}

function asLanguage(value: unknown): Language | null {
  return typeof value === 'string' && (LANGUAGES as readonly string[]).includes(value)
    ? (value as Language)
    : null;
}

function readPrincipal(value: unknown): BootPrincipal | null {
  const source = asRecord(value);
  if (source === null) {
    return null;
  }
  return {
    subject: readText(source, 'subject'),
    preferred_username: readText(source, 'preferred_username'),
    provider_id: readText(source, 'provider_id'),
    authentication: readText(source, 'authentication'),
  };
}

/**
 * The credential that could satisfy a step-up, or `unstated`.
 *
 * Anything the server did not spell is `unstated` and offers no credential.
 * Defaulting to `local` here is what put the administrator password prompt in
 * front of federated operators, who have no such password to give.
 */
function readStepUpMethod(source: Record<string, unknown> | null): StepUpMethod | 'unstated' {
  const value = readText(source, 'step_up_method');
  return value === 'local' || value === 'federated' || value === 'unavailable' ? value : 'unstated';
}

/**
 * The provider list, or nothing.
 *
 * An entry with no id is dropped rather than kept: the id is the whole of
 * `/auth/provider/<id>/start`, so an entry without one describes a button that
 * navigates to a 404. A payload that is not a list at all yields the empty list,
 * which is the same thing the pre-multi-provider deployment sends — the caller
 * then offers the single legacy destination, which is exactly right for both.
 */
function readIdentityProviders(value: unknown): BootIdentityProvider[] {
  if (!Array.isArray(value)) {
    return [];
  }
  const providers: BootIdentityProvider[] = [];
  for (const entry of value) {
    const source = asRecord(entry);
    const id = readText(source, 'id');
    if (id !== '') {
      providers.push({
        id,
        display_name: readText(source, 'display_name'),
        supports_step_up: readFlag(source, 'supports_step_up'),
      });
    }
  }
  return providers;
}

function readRoute(value: unknown): BootRoute {
  const source = asRecord(value);
  if (source === null) {
    return { ...EMPTY_ROUTE };
  }
  return {
    name: readText(source, 'name'),
    path: readText(source, 'path'),
    job_id: readText(source, 'job_id'),
    instance_id: readText(source, 'instance_id'),
    user_code: readText(source, 'user_code'),
  };
}

/**
 * Narrow the untrusted global into the payload the app reads.
 *
 * `servedLang` is the value the server stamped on `<html data-pe-lang>`. It is
 * the fallback rather than the browser's own locale on purpose: the whole point
 * of resolving language server-side is that a reader who chose English on a
 * zh-CN machine gets English, and a client-side `navigator.language` fallback
 * would reintroduce exactly that leak the one time the payload is missing.
 */
export function coerceBootPayload(raw: unknown, servedLang: string | null): BootPayload {
  const source = asRecord(raw);
  const lang = asLanguage(source?.['lang']) ?? asLanguage(servedLang) ?? 'en';
  const htmlLang = readText(source, 'html_lang');
  return {
    lang,
    html_lang: htmlLang === '' ? LANGUAGE_TAGS[lang] : htmlLang,
    auth_enabled: readFlag(source, 'auth_enabled'),
    oidc_enabled: readFlag(source, 'oidc_enabled'),
    local_login_enabled: readFlag(source, 'local_login_enabled'),
    identity_providers: readIdentityProviders(source?.['identity_providers']),
    principal: readPrincipal(source?.['principal']),
    step_up_method: readStepUpMethod(source),
    route: readRoute(source?.['route']),
    asset_base: readText(source, 'asset_base'),
  };
}

/**
 * Read the payload the server stamped into the shell. Called once, synchronously,
 * before the first render — the language, the auth shape and the route params are
 * all inputs to that render, and fetching any of them would cost a frame in the
 * wrong state.
 */
export function readBootPayload(
  scope: Pick<Window, '__PE_BOOT__'> = window,
  root: Element | null = document.documentElement,
): BootPayload {
  return coerceBootPayload(scope[BOOT_GLOBAL], root?.getAttribute(LANG_ATTRIBUTE) ?? null);
}
