/**
 * The route vocabulary, mirroring `consoleRoutes` in
 * internal/dashboard/console.go.
 *
 * Three facts live here and nowhere else: which chrome a route sits in, what
 * capability it needs, and which component renders it. None is derived from a
 * path's spelling. In the console
 * this replaces the removal pass matched three route paths re-spelled as
 * JavaScript selectors, so a route added to the table defaulted to visible for
 * everyone — declaring it once is what makes a forgotten declaration a public
 * route by construction rather than by omission.
 *
 * The Go table is the source of truth for `name` and `public`, and
 * console_test.go's route table is what keeps the two in step: a path only
 * reaches this bundle if the server matched it there first.
 */

import type { ComponentType } from 'react';

import type { BootPayload } from '../boot/payload';
import type { Language, MessageKey } from '../i18n/messages';
import { BuildDetailPage } from '../pages/builds/BuildDetailPage';
import { BuildsPage } from '../pages/builds/BuildsPage';
import { LogsPage } from '../pages/builds/LogsPage';
import { OverviewPage } from '../pages/builds/OverviewPage';
import { SettingsPage } from '../pages/settings/SettingsPage';
import { DevicePage } from '../pages/device/DevicePage';
import { DocsPage } from '../pages/docs/DocsPage';
import { ImageFactoryPage } from '../pages/image-factory/ImageFactoryPage';
import { LandingPage } from '../pages/landing/LandingPage';
import { LoginPage } from '../pages/login/LoginPage';
import { MonitorPage } from '../pages/monitor/MonitorPage';
import { PackagesPage } from '../pages/packages/PackagesPage';
import { ShellPage } from '../pages/shell/ShellPage';
import { StatusPage } from '../pages/status/StatusPage';

/** The one capability the console gates on, spelled the way IAM spells it. */
export const CAPABILITY_SYSTEM_ADMIN = 'system-admin';

/** Which chrome wraps a route. */
export type Chrome =
  /** Sidebar, project context, identity, sign out. */
  | 'app'
  /** The public community bar: brand, three links, language, sign in. */
  | 'public'
  /** No chrome at all: the landing page, the auth cards and the terminal. */
  | 'bare';

export interface ConsoleRoute {
  /** The name the server's route table gave it. */
  name: string;
  /** The path under the console base, as react-router spells it. */
  path: string;
  chrome: Chrome;
  /** Reachable without a session, mirroring the Go table's `public` flag. */
  public: boolean;
  /**
   * The document title. A whole title including the product name, in both
   * languages: the console this replaces appended " — Portage Engine" in the Go
   * template on the English side only, so the Chinese catalogue carried the
   * suffix and the English one did not, and the two keys described structurally
   * different documents. Nothing with a placeholder in it, so no placeholder
   * check could have seen that. These strings only ever reach `document.title`.
   */
  titleKey: MessageKey;
  /**
   * The page heading. Separate from the title on purpose — a heading is a name
   * and a title is a name plus the product — and taking one for the other is
   * what put "总览 — Portage Engine" in an h1 the first time this was rendered.
   */
  headingKey: MessageKey;
  /**
   * What IAM must have granted before this route renders anything.
   *
   * On the route and not on the navigation entry it used to sit on. A capability
   * declared only on a nav item hides a link, and a hidden link is not a gate:
   * the three admin paths were still typed into the address bar, still matched,
   * and still rendered a whole page whose every request the control plane was
   * about to refuse. The server is the authority either way — this is about not
   * handing a reader a page of refusals to read.
   */
  capability?: string;
  /** Present only for a destination in the main navigation. */
  nav?: { labelKey: MessageKey };
  /**
   * What renders inside the chrome. Absent until the route's page is ported,
   * which is what leaves it on `PlaceholderPage` — so a half-ported console
   * still routes, still wears its chrome and still renders its own loading
   * state, rather than answering a real path with a blank frame.
   */
  component?: ComponentType<PageProps>;
}

/**
 * What every page is handed.
 *
 * The same three things for all of them, so the router has one call shape and a
 * page that needs none of them declares none: its own row in the table, the
 * payload the server stamped into the shell, and the language setter — which the
 * bare routes need because they carry their own copy of the language control
 * rather than sitting inside a chrome that has one.
 */
export interface PageProps {
  route: ConsoleRoute;
  boot: BootPayload;
  onLanguageChange: (lang: Language) => void;
}

export const CONSOLE_ROUTES: readonly ConsoleRoute[] = [
  {
    name: 'landing',
    path: '/',
    // Public, not bare. The console this replaces hardcoded a second public nav
    // into the landing template — the same three links, spelled twice, with no
    // aria-current on this copy and nothing keeping the two in step. There is
    // one public bar now and this page wears it.
    chrome: 'public',
    public: true,
    titleKey: 'title.landing',
    headingKey: 'landing.h1',
    component: LandingPage,
  },
  {
    name: 'login',
    path: '/login',
    chrome: 'bare',
    public: true,
    titleKey: 'title.login',
    headingKey: 'login.h1',
    component: LoginPage,
  },
  {
    name: 'device',
    path: '/device',
    chrome: 'bare',
    public: false,
    titleKey: 'title.device',
    headingKey: 'device.h1',
    component: DevicePage,
  },

  {
    name: 'overview',
    titleKey: 'title.overview',
    headingKey: 'ov.h1',
    path: '/overview',
    chrome: 'app',
    public: false,
    nav: { labelKey: 'nav.overview' },
    component: OverviewPage,
  },
  {
    name: 'builds',
    titleKey: 'title.builds',
    headingKey: 'builds.h1',
    path: '/builds',
    chrome: 'app',
    public: false,
    nav: { labelKey: 'nav.builds' },
    component: BuildsPage,
  },
  {
    name: 'build-detail',
    path: '/build/:jobID',
    chrome: 'app',
    public: false,
    titleKey: 'title.detail',
    headingKey: 'detail.h1',
    component: BuildDetailPage,
  },
  {
    name: 'logs',
    path: '/logs/:jobID',
    chrome: 'app',
    public: false,
    titleKey: 'title.logs',
    headingKey: 'logs.h1',
    component: LogsPage,
  },
  {
    name: 'monitor',
    titleKey: 'title.monitor',
    headingKey: 'mon.h1',
    path: '/monitor',
    chrome: 'app',
    public: false,
    capability: CAPABILITY_SYSTEM_ADMIN,
    nav: { labelKey: 'nav.monitor' },
    component: MonitorPage,
  },
  {
    name: 'image-factory',
    titleKey: 'title.factory',
    headingKey: 'factory.h1',
    path: '/image-factory',
    chrome: 'app',
    public: false,
    capability: CAPABILITY_SYSTEM_ADMIN,
    nav: { labelKey: 'nav.factory' },
    component: ImageFactoryPage,
  },
  {
    name: 'settings',
    titleKey: 'title.settings',
    headingKey: 'set.h1',
    path: '/settings',
    chrome: 'app',
    public: false,
    capability: CAPABILITY_SYSTEM_ADMIN,
    nav: { labelKey: 'nav.settings' },
    component: SettingsPage,
  },

  {
    name: 'packages',
    titleKey: 'title.packages',
    headingKey: 'packages.h1',
    path: '/packages',
    chrome: 'public',
    public: true,
    nav: { labelKey: 'nav.packages' },
    component: PackagesPage,
  },
  {
    name: 'docs',
    path: '/docs',
    chrome: 'public',
    public: true,
    titleKey: 'title.docs',
    headingKey: 'docs.h1',
    nav: { labelKey: 'nav.docs' },
    component: DocsPage,
  },
  {
    name: 'status',
    titleKey: 'title.status',
    headingKey: 'status.h1',
    path: '/status',
    chrome: 'public',
    public: true,
    nav: { labelKey: 'nav.status' },
    component: StatusPage,
  },

  {
    name: 'shell',
    path: '/shell/:instanceID',
    chrome: 'bare',
    public: false,
    titleKey: 'title.shell',
    headingKey: 'shell.title',
    component: ShellPage,
  },
];

/**
 * The console base. Every path above is relative to it, and the router's
 * basename is what keeps a hand-written `href` from escaping it.
 *
 * It has to equal `consoleBase` in internal/dashboard/console.go; a Go test
 * asserts the built bundle's asset URLs sit under the matching asset prefix, and
 * a mismatch here would produce a shell that 404s every link instead.
 */
export const CONSOLE_BASE = '/ui';

/** The destinations the main navigation offers, in the order it offers them. */
export const NAV_ROUTES = CONSOLE_ROUTES.filter(
  (route): route is ConsoleRoute & { nav: NonNullable<ConsoleRoute['nav']> } =>
    route.nav !== undefined && route.chrome === 'app',
);

/** The three links the public bar carries. */
export const PUBLIC_NAV_ROUTES = CONSOLE_ROUTES.filter(
  (route): route is ConsoleRoute & { nav: NonNullable<ConsoleRoute['nav']> } =>
    route.nav !== undefined && route.chrome === 'public',
);
