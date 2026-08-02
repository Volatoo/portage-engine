import type { ReactNode } from 'react';
import { NavLink } from 'react-router';

import { useMessages } from '../i18n/context';
import type { Language } from '../i18n/messages';
import { LanguageControl } from './LanguageControl';
import { ProjectQuota } from './ProjectQuota';
import { isGranted } from './capabilities';
import type { IdentityContext } from './capabilities';
import { NAV_ROUTES, PUBLIC_NAV_ROUTES } from './routes';

/**
 * The chrome, in one file, for both audiences.
 *
 * There is no route that renders itself without chrome by accident: `App`
 * selects between these two and `bare` from the route table's own `chrome`
 * field, so there is no code path that could forget, and therefore nothing to
 * diff between page and page. Both carry the language control, and the public
 * one carries it for the reason in LanguageControl.tsx.
 *
 * The rail and the phone-width bar are two renderings of one list, not two
 * lists: under 700px `.sidebar` is `display: none` and `.topbar` is the only
 * navigation there is, so everything that lived only in the rail — the project
 * switcher, the identity, the language control, sign out — has a copy there.
 */

/**
 * Where the binary packages are served. `mux.Handle("/binpkgs/", ...)` in
 * internal/dashboard/dashboard.go, and the string the old rail printed.
 */
const BINHOST_PATH = '/binpkgs';

export interface ChromeProps {
  identity: IdentityContext;
  authEnabled: boolean;
  /** The project every request is scoped to. '' until IAM has named one. */
  projectID: string;
  onProjectChange: (projectID: string) => void;
  onLanguageChange: (lang: Language) => void;
  children: ReactNode;
}

/** The destinations this reader may see. Empty of gated routes until IAM answers. */
function useVisibleNav(identity: IdentityContext) {
  return NAV_ROUTES.filter((route) => isGranted(identity, route.capability));
}

function NavItems({ identity }: { identity: IdentityContext }) {
  const messages = useMessages();
  return useVisibleNav(identity).map((route) => (
    <NavLink
      key={route.name}
      className="nav-item"
      // NavLink stamps aria-current="page" itself when the route is active, and
      // the stylesheet reads that attribute rather than a class: the state the
      // reader is in is on the element, so the stylesheet and a screen reader
      // cannot come to different conclusions about it.
      to={route.path}
    >
      {messages.t(route.nav.labelKey)}
    </NavLink>
  ));
}

/**
 * The identity line, in the three states it actually has.
 *
 * Never blank: a slot that renders nothing while a request is out is a slot a
 * reader reads as "signed in as nobody". The failure branch says the identity is
 * unavailable rather than leaving the loading sentence up forever, because those
 * two mean different things to an operator deciding whether to trust the page.
 */
function Identity({ identity }: { identity: IdentityContext }) {
  const messages = useMessages();
  if (!identity.resolved) {
    return messages.t('iam.identity.loading');
  }
  if (identity.failure !== null || identity.displayName === '') {
    return messages.t('iam.unavailable');
  }
  return identity.displayName;
}

/**
 * The switcher, which is not a convenience.
 *
 * `ResolveProject` in internal/persistence/iam.go refuses a request with no
 * `X-Project-ID` from any non-system-admin holding more than one membership, so
 * for exactly the reader this control exists for, the value it holds is what
 * makes /overview, /builds and /build/:id load at all. Bound in both copies of
 * the chrome, because under 700px the rail is `display: none` and the bar is the
 * only one of them on screen.
 *
 * The role rides on the label because the same project grants different things
 * to different people, and which of them the reader is decides whether the
 * controls on the page they are about to open will be there.
 */
function ProjectSwitcher({
  id,
  identity,
  projectID,
  onProjectChange,
}: {
  id: string;
  identity: IdentityContext;
  projectID: string;
  onProjectChange: (projectID: string) => void;
}) {
  const messages = useMessages();
  return (
    <select
      id={id}
      aria-label={messages.t('iam.project')}
      value={projectID}
      // Nothing to switch between is the state an IAM outage produces, and a
      // control offering an empty list is one the reader tries before believing
      // it. The console this replaces disabled it in the same branch that says
      // the identity is unavailable.
      disabled={identity.projects.length === 0}
      onChange={(chosen) => {
        onProjectChange(chosen.target.value);
      }}
    >
      {identity.projects.map((project) => (
        <option key={project.project_id} value={project.project_id}>
          {`${project.project_name} · ${project.role}`}
        </option>
      ))}
    </select>
  );
}

export function AppChrome({
  identity,
  authEnabled,
  projectID,
  onProjectChange,
  onLanguageChange,
  children,
}: ChromeProps) {
  const messages = useMessages();
  return (
    <>
      <div className="topbar">
        <span className="brand">Portage Engine</span>
        <NavItems identity={identity} />
        <span className="topbar-chrome">
          <ProjectSwitcher
            id="project-switcher-compact"
            identity={identity}
            projectID={projectID}
            onProjectChange={onProjectChange}
          />
          <span className="identity" data-iam-identity>
            <Identity identity={identity} />
          </span>
          <LanguageControl onChange={onLanguageChange} />
          {authEnabled ? <a href="/logout">{messages.t('nav.signout')}</a> : null}
        </span>
      </div>
      <div className="shell">
        <nav className="sidebar" aria-label={messages.t('nav.main')}>
          <div className="brand">
            Portage Engine<span>{messages.t('brand.sub')}</span>
          </div>
          <NavItems identity={identity} />
          <div className="spacer" />
          <div className="project-context">
            <label htmlFor="project-switcher">{messages.t('iam.project')}</label>
            <ProjectSwitcher
              id="project-switcher"
              identity={identity}
              projectID={projectID}
              onProjectChange={onProjectChange}
            />
            <div className="identity" data-iam-identity>
              <Identity identity={identity} />
            </div>
            {/* The rail only. It is twenty numbers about one project, and the
                bar it would go in under 700px is one line tall. */}
            <ProjectQuota />
          </div>
          <div className="foot">
            <LanguageControl onChange={onLanguageChange} />
          </div>
          {authEnabled ? (
            <div className="foot">
              <a href="/logout">{messages.t('nav.signout')}</a>
            </div>
          ) : null}
          {/* The one line of the old rail (ui.go) that says where the packages
              this console builds are actually served from. It is the path an
              operator puts in binrepos.conf, it is the same on every deployment,
              and it is untranslated there for that reason: it is a path, not a
              sentence. */}
          <div className="foot">
            binhost:<span className="mono"> {BINHOST_PATH}</span>
          </div>
        </nav>
        <main className="content">{children}</main>
      </div>
    </>
  );
}

export function PublicChrome({
  onLanguageChange,
  children,
}: Omit<ChromeProps, 'identity' | 'authEnabled' | 'projectID' | 'onProjectChange'>) {
  const messages = useMessages();
  return (
    <>
      <nav className="landing-nav" aria-label={messages.t('nav.public')}>
        <NavLink className="brand" to="/">
          Portage Engine
        </NavLink>
        <span className="public-links">
          {PUBLIC_NAV_ROUTES.map((route) => (
            <NavLink key={route.name} className="public-link" to={route.path}>
              {messages.t(route.nav.labelKey)}
            </NavLink>
          ))}
        </span>
        <span className="side">
          <LanguageControl onChange={onLanguageChange} />
          <NavLink className="btn" to="/overview">
            {messages.t('landing.signin')}
          </NavLink>
        </span>
      </nav>
      <main className="public-main">{children}</main>
      <footer className="landing-footer">{messages.t('landing.footer')}</footer>
    </>
  );
}
