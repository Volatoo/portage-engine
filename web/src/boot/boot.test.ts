import { describe, expect, it } from 'vitest';

import { coerceBootPayload, readBootPayload } from './index';
import { BOOT_GLOBAL, LANG_ATTRIBUTE, type BootPayload } from './payload';

function serverPayload(overrides: Partial<BootPayload> = {}): unknown {
  return {
    lang: 'zh',
    html_lang: 'zh-CN',
    auth_enabled: true,
    oidc_enabled: true,
    local_login_enabled: false,
    identity_providers: [],
    principal: {
      subject: 'operator',
      preferred_username: 'operator',
      provider_id: '',
      authentication: 'local-session',
    },
    route: {
      name: 'build-detail',
      path: '/ui/build/job-4711',
      job_id: 'job-4711',
      instance_id: '',
      user_code: '',
    },
    asset_base: '/static/ui/',
    ...overrides,
  };
}

describe('coerceBootPayload', () => {
  it('keeps the language the server resolved', () => {
    expect(coerceBootPayload(serverPayload(), 'en').lang).toBe('zh');
  });

  it('falls back to the language stamped on the html element when the payload is missing', () => {
    expect(coerceBootPayload(undefined, 'zh').lang).toBe('zh');
    expect(coerceBootPayload(undefined, 'zh').html_lang).toBe('zh-CN');
  });

  it('refuses a language token it ships no strings for', () => {
    expect(coerceBootPayload(serverPayload({ lang: 'de' as BootPayload['lang'] }), null).lang).toBe(
      'en',
    );
  });

  it('carries the route params the server already resolved', () => {
    const boot = coerceBootPayload(serverPayload(), null);
    expect(boot.route.name).toBe('build-detail');
    expect(boot.route.job_id).toBe('job-4711');
    expect(boot.route.instance_id).toBe('');
    expect(boot.route.user_code).toBe('');
  });

  it('reads one shape: an id the route does not carry is empty, never absent', () => {
    const boot = coerceBootPayload({ route: { name: 'overview' } }, null);
    expect(Object.keys(boot.route).sort()).toEqual([
      'instance_id',
      'job_id',
      'name',
      'path',
      'user_code',
    ]);
    expect(boot.route.job_id).toBe('');
  });

  it('carries every provider the server named, in the order it named them', () => {
    const boot = coerceBootPayload(
      serverPayload({
        identity_providers: [
          { id: 'authentik', display_name: 'Authentik', supports_step_up: true },
          { id: 'github', display_name: 'GitHub', supports_step_up: false },
        ],
      }),
      null,
    );
    expect(boot.identity_providers).toEqual([
      { id: 'authentik', display_name: 'Authentik', supports_step_up: true },
      { id: 'github', display_name: 'GitHub', supports_step_up: false },
    ]);
  });

  it('drops an entry with no id, because the id is the whole start route', () => {
    const boot = coerceBootPayload(
      { identity_providers: [{ display_name: 'Nameless' }, 'authentik', null] },
      null,
    );
    expect(boot.identity_providers).toEqual([]);
  });

  it('reads a missing provider list as no names, never as an absent key', () => {
    // Which is what the pre-multi-provider deployment sends, and what makes the
    // sign-in card offer the one legacy destination such a deployment answers.
    for (const raw of [{}, { identity_providers: null }, { identity_providers: 'authentik' }]) {
      expect(coerceBootPayload(raw, null).identity_providers).toEqual([]);
    }
  });

  it('leaves the principal unknown rather than inventing an anonymous one', () => {
    expect(coerceBootPayload(serverPayload({ principal: null }), null).principal).toBeNull();
    expect(coerceBootPayload({}, null).principal).toBeNull();
    expect(coerceBootPayload({ principal: 'operator' }, null).principal).toBeNull();
  });

  it('turns a flag on only for a literal true, so a drifted payload fails closed', () => {
    expect(coerceBootPayload({ auth_enabled: true }, null).auth_enabled).toBe(true);
    expect(coerceBootPayload({ auth_enabled: 'true' }, null).auth_enabled).toBe(false);
    expect(coerceBootPayload({ auth_enabled: 1 }, null).auth_enabled).toBe(false);
    expect(coerceBootPayload({}, null).auth_enabled).toBe(false);
  });

  it('never asserts authority: the payload carries identity and no capability', () => {
    const boot = coerceBootPayload(
      serverPayload({
        principal: {
          subject: 'operator',
          preferred_username: 'operator',
          provider_id: '',
          authentication: 'local-session',
        },
      }),
      null,
    );
    expect(Object.keys(boot.principal ?? {}).sort()).toEqual([
      'authentication',
      'preferred_username',
      'provider_id',
      'subject',
    ]);
  });

  it('survives a payload that is not an object at all', () => {
    for (const raw of [null, undefined, 'boot', 42, ['boot']]) {
      const boot = coerceBootPayload(raw, null);
      expect(boot.lang).toBe('en');
      expect(boot.principal).toBeNull();
      expect(boot.route.name).toBe('');
    }
  });
});

describe('readBootPayload', () => {
  it('reads the global the server stamped and the language attribute beside it', () => {
    const root = document.createElement('html');
    root.setAttribute(LANG_ATTRIBUTE, 'zh');
    const boot = readBootPayload({ [BOOT_GLOBAL]: serverPayload() }, root);
    expect(boot.lang).toBe('zh');
    expect(boot.route.job_id).toBe('job-4711');
  });

  it('does not read the operating system locale when the server sent nothing', () => {
    const root = document.createElement('html');
    expect(readBootPayload({}, root).lang).toBe('en');
    expect(readBootPayload({}, null).lang).toBe('en');
  });
});
