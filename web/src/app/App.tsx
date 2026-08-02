import { useCallback, useState } from 'react';
import type { ReactNode } from 'react';
import { BrowserRouter, Route, Routes } from 'react-router';

import { api } from '../api/endpoints';
import type { BootPayload } from '../boot/payload';
import { MessagesProvider } from '../i18n/context';
import type { Language } from '../i18n/messages';
import { AppChrome, PublicChrome } from './Chrome';
import { PlaceholderPage } from './PlaceholderPage';
import { isGranted, useIdentity } from './capabilities';
import type { IdentityContext } from './capabilities';
import { ConsoleErrorBoundary, RouteNotFound, RouteNotPermitted } from './fallbacks';
import { ProjectScopeContext, useProjectSelection } from './project';
import { useSessionGuard } from './session';
import { CONSOLE_BASE, CONSOLE_ROUTES } from './routes';
import type { Chrome, ConsoleRoute } from './routes';

/**
 * The console root.
 *
 * Language is state seeded from the boot payload, never fetched and never read
 * from the browser. The server already resolved it — cookie, then
 * `Accept-Language` — and stamped it on `<html>` and into `window.__PE_BOOT__`,
 * so the first render is in the right language and there is no frame in the
 * wrong one. `navigator.language` is banned by lint for the same reason: it is
 * the operating system's locale, which is the leak server-side resolution exists
 * to prevent.
 *
 * Theme is not here at all. It is `prefers-color-scheme` remapping tokens in
 * CSS, with no stored preference, no `matchMedia`, and no component-level dark
 * rule — so a reader who flips their system theme mid-session gets the new one
 * on the next paint with nothing subscribing to anything.
 *
 * The selected project is here for the reason language is: one value, one
 * owner, and every page reading it from the same place. It is what puts
 * `X-Project-ID` on the requests the control plane refuses without it.
 */

/**
 * One route's frame.
 *
 * It exists so the two decisions that need the router and the matched route —
 * where an expired session goes, and whether this route needs a session at all
 * — are made once per route rather than once per page that remembered to.
 */
function RouteFrame({ route, children }: { route: ConsoleRoute; children: ReactNode }): ReactNode {
  useSessionGuard(route.public);
  return children;
}

/**
 * The capability gate, on the route rather than on the link to it.
 *
 * `Chrome` already hides a destination this reader has no grant for, and hiding
 * a link is not a gate: /monitor, /image-factory and /settings were reachable by
 * typing them, by a bookmark and by a link someone pasted into chat, and every
 * one of those rendered the whole page — which then filled with the control
 * plane's refusals, one per card. The server is still the authority; this is
 * about not handing a reader a page of them to read.
 *
 * Nothing is decided before IAM has answered. Rendering the refusal against an
 * empty capability set would show it to the administrator too, for as long as
 * the round trip takes, and "not available to you" is not a sentence to put in
 * front of someone it is untrue about — the same reason the removal pass this
 * console replaces was wrong to run after paint.
 */
function gate(route: ConsoleRoute, identity: IdentityContext, page: ReactNode): ReactNode {
  if (route.capability === undefined || isGranted(identity, route.capability)) {
    return page;
  }
  if (!identity.resolved) {
    return <PlaceholderPage route={route} />;
  }
  return <RouteNotPermitted capability={route.capability} />;
}

export function App({ boot }: { boot: BootPayload }) {
  const [lang, setLang] = useState<Language>(boot.lang);
  const identity = useIdentity(boot.auth_enabled);
  const project = useProjectSelection(identity);

  const changeLanguage = useCallback((next: Language) => {
    setLang(next);
    // The attributes the server stamped are the ones the boot reader falls back
    // to, so they are kept true rather than left describing the served language.
    document.documentElement.lang = next === 'zh' ? 'zh-CN' : 'en';
    document.documentElement.setAttribute('data-pe-lang', next);
    // The cookie is what makes the NEXT navigation arrive already translated.
    // Without it the server renders the served language again and the flash the
    // boot payload exists to prevent comes back on the next full page load.
    void api.setLanguage(next);
  }, []);

  const wrap = (chrome: Chrome, content: React.ReactNode): React.ReactNode => {
    // Inside the chrome and not around it: a page that throws should cost the
    // reader the page, not the navigation, the project switcher and the language
    // control they need to get somewhere else. The boundary in main.tsx is the
    // one underneath this — it catches what this one cannot, which is the chrome.
    const page = <ConsoleErrorBoundary>{content}</ConsoleErrorBoundary>;
    switch (chrome) {
      case 'app':
        return (
          <AppChrome
            identity={identity}
            authEnabled={boot.auth_enabled}
            projectID={project.scope.projectID}
            onProjectChange={project.select}
            onLanguageChange={changeLanguage}
          >
            {page}
          </AppChrome>
        );
      case 'public':
        return <PublicChrome onLanguageChange={changeLanguage}>{page}</PublicChrome>;
      case 'bare':
        return page;
    }
  };

  return (
    <MessagesProvider lang={lang}>
      {/* Above the router, so the selection survives a navigation: the header
          every page sends is one value the chrome owns, not a value each page
          re-reads from storage on mount. */}
      <ProjectScopeContext value={project.scope}>
        <BrowserRouter basename={CONSOLE_BASE}>
          <Routes>
            {CONSOLE_ROUTES.map((route: ConsoleRoute) => {
              // One line, and the table decides: a ported route names its
              // component there, an unported one names none and keeps the
              // placeholder. Nothing here matches on a route name, so porting a
              // page never means editing the router.
              const Page = route.component;
              return (
                <Route
                  key={route.name}
                  path={route.path}
                  element={
                    <RouteFrame route={route}>
                      {wrap(
                        route.chrome,
                        gate(
                          route,
                          identity,
                          Page === undefined ? (
                            <PlaceholderPage route={route} />
                          ) : (
                            // One call shape for every page: its own row, the
                            // payload the server stamped into the shell, and the
                            // language setter the bare routes carry their own
                            // control for.
                            <Page route={route} boot={boot} onLanguageChange={changeLanguage} />
                          ),
                        ),
                      )}
                    </RouteFrame>
                  }
                />
              );
            })}
            {/* Every path the server serves the shell for is in the table above,
                so this catches what a client-side navigation and a stale link
                produce. Without it react-router matched nothing and rendered
                nothing, which is a blank white page an operator cannot tell from
                an outage. */}
            <Route path="*" element={wrap('public', <RouteNotFound />)} />
          </Routes>
        </BrowserRouter>
      </ProjectScopeContext>
    </MessagesProvider>
  );
}
