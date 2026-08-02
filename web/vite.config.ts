/// <reference types="vitest/config" />
import react from '@vitejs/plugin-react';
import { defineConfig, type Plugin } from 'vite';

import type { BootPayload } from './src/boot/payload.ts';

/**
 * The URL prefix the hashed asset tree is mounted at. It has to agree with
 * `consoleAssetPrefix` in internal/dashboard/console.go, and a Go test asserts
 * that every asset URL in the built index.html starts with it — so a change on
 * one side turns the build red instead of producing 404s at runtime.
 */
const ASSET_BASE = '/static/ui/';

/**
 * `npm run build` writes straight into the Go embed root. There is no copy step
 * and therefore nothing that can copy a stale tree: the bytes go:embed compiles
 * in are the bytes Vite just emitted.
 */
const EMBED_ROOT = '../internal/dashboard/webassets/bundle/dist';

/** What the Go handler would have stamped, for `vite dev`, which has no Go handler. */
const DEV_BOOT: BootPayload = {
  lang: 'en',
  html_lang: 'en',
  auth_enabled: false,
  oidc_enabled: false,
  local_login_enabled: false,
  identity_providers: [],
  principal: null,
  route: { name: 'overview', path: '/ui/overview', job_id: '', instance_id: '', user_code: '' },
  asset_base: ASSET_BASE,
};

const DEV_SUBSTITUTIONS: Record<string, string> = {
  '{{.HTMLLang}}': DEV_BOOT.html_lang,
  '{{.Lang}}': DEV_BOOT.lang,
  '{{.Boot}}': JSON.stringify(DEV_BOOT),
};

/**
 * Fills index.html's Go template actions in serve mode.
 *
 * The table above is the contract between the shell and the Go handler. An
 * action added to index.html with no entry here stops the dev server with the
 * action's own name in the message, rather than shipping `{{.Whatever}}` to the
 * browser as text and letting someone find it in a screenshot.
 */
function goTemplateShell(): Plugin {
  return {
    name: 'portage-engine:go-template-shell',
    apply: 'serve',
    transformIndexHtml: {
      order: 'pre',
      handler(html: string): string {
        return html.replace(/\{\{[^{}]*\}\}/g, (action) => {
          const value = DEV_SUBSTITUTIONS[action];
          if (value === undefined) {
            throw new Error(
              `console: index.html carries ${action}, which has no dev substitution in vite.config.ts`,
            );
          }
          return value;
        });
      },
    },
  };
}

export default defineConfig({
  base: ASSET_BASE,
  plugins: [react(), goTemplateShell()],
  build: {
    outDir: EMBED_ROOT,
    emptyOutDir: true,
    // Hashed filenames live under assets/ and nowhere else. That is the rule the
    // Go handler uses to decide what may be served immutable: a name containing a
    // digest of its own bytes can never mean two different files, so a year-long
    // cache is safe; anything outside assets/ is revalidated.
    assetsDir: 'assets',
    // The bundle is compiled into the Go binary and shipped to operators. A
    // source map would roughly double that and is not something an operator can
    // act on; it is regenerated locally when someone is actually debugging.
    sourcemap: false,
    target: 'es2022',
  },
  test: {
    // jsdom, not node: the boot reader's whole job is to read a global and an
    // attribute off the served document, and asserting that against a fake
    // object would assert the fake.
    environment: 'jsdom',
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
  },
});
