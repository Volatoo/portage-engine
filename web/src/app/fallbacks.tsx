/**
 * The three screens that exist so the console never answers with nothing.
 *
 * A blank white page is the worst answer an operator console can give, because
 * it reads identically as an outage, as a bad deploy and as a typo in the
 * address bar, and it carries no way back. This port could produce three of
 * them: a render that threw took the whole tree out with no boundary anywhere,
 * `/ui/build/` and its two siblings matched on the server and matched nothing in
 * the router, and a capability the router never checked let a reader with no
 * grant open a whole admin page whose every request the server was about to
 * refuse.
 *
 * Each screen states what happened and offers the one thing that acts on it.
 * None of them is a row in `CONSOLE_ROUTES`: they are what the shell renders in
 * place of one.
 */

import { Component } from 'react';
import type { ErrorInfo, ReactNode } from 'react';
import { NavLink } from 'react-router';

import { useMessages } from '../i18n/context';
import { fallback } from './fallbackCopy';

/**
 * The catch-all route's page.
 *
 * It wears the public chrome, because a reader who mistyped an address may hold
 * no session at all, and sending them through sign-in to be told the page does
 * not exist is two wrong answers where there was one.
 */
export function RouteNotFound() {
  const messages = useMessages();
  return (
    <div className="page-head">
      <div>
        <h1>{fallback('err.notfound.h1')}</h1>
        <p className="sub">{fallback('err.notfound.hint')}</p>
      </div>
      <div className="actions">
        <NavLink className="btn" to="/overview">
          {messages.t('nav.overview')}
        </NavLink>
      </div>
    </div>
  );
}

/**
 * What a gated route renders for a reader who has not been granted it.
 *
 * The capability is named rather than hinted at: an operator told only "not
 * available" opens a ticket, and one told which grant is missing asks for that
 * grant. The navigation stays around it, because this is a wrong destination and
 * not a broken console.
 */
export function RouteNotPermitted({ capability }: { capability: string }) {
  return (
    <div className="page-head">
      <div>
        <h1>{fallback('err.forbidden.h1')}</h1>
        <p className="sub" role="status">
          {fallback('err.forbidden.hint', { capability })}
        </p>
      </div>
    </div>
  );
}

interface BoundaryState {
  error: Error | null;
}

/**
 * The boundary, which is a class because React offers no hook that catches.
 *
 * It renders no chrome and reads no context. Whatever threw is somewhere below
 * this, the providers are part of what it is catching, and a boundary that needs
 * the tree it caught is a boundary that throws while rendering the error.
 *
 * The way out is a full reload rather than a router navigation: React has
 * unmounted the subtree that failed, every piece of state above it is now
 * suspect, and rebuilding the document is the only recovery worth offering.
 */
export class ConsoleErrorBoundary extends Component<{ children: ReactNode }, BoundaryState> {
  override state: BoundaryState = { error: null };

  static getDerivedStateFromError(error: unknown): BoundaryState {
    return { error: error instanceof Error ? error : new Error(String(error)) };
  }

  override componentDidCatch(error: unknown, info: ErrorInfo): void {
    // Where the component stack goes. The screen below deliberately does not
    // print one at an operator who cannot act on it, so this is the only place
    // the tree that failed is named at all.
    console.error('console: render failed', error, info.componentStack);
  }

  override render(): ReactNode {
    const error = this.state.error;
    if (error === null) {
      return this.props.children;
    }
    // A fragment and not a layout element: this renders both directly under
    // #root, where the whole shell failed, and inside the chrome's own <main>,
    // where only a page did. Nesting a second landmark inside the first is what
    // a wrapper here would do to the second case.
    return (
      <>
        <div className="page-head">
          <div>
            <h1>{fallback('err.render.h1')}</h1>
            <p className="sub">{fallback('err.render.hint')}</p>
          </div>
          <div className="actions">
            <button
              className="btn"
              type="button"
              onClick={() => {
                window.location.reload();
              }}
            >
              {fallback('err.render.reload')}
            </button>
          </div>
        </div>
        <div className="card">
          <div className="card-pad">
            <div className="empty" data-state="error" role="alert">
              {error.message}
            </div>
          </div>
        </div>
      </>
    );
  }
}
