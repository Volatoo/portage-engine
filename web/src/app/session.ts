/**
 * What an expired session means, decided once.
 *
 * `if (r.status === 401) { location.href = '/login'; }` sat in the shared
 * `api()` of the console this replaces (internal/dashboard/ui.go), and the port
 * kept the outcome and dropped the navigation: a reader whose cookie expired
 * behind a fifteen-second poll got an error box, a Retry that could only produce
 * another 401, and a poll still running against a credential the server had
 * already forgotten.
 *
 * It is a hook and not a rule in the transport because it needs the one thing
 * the transport cannot see: whether this route needs a session at all. The
 * sign-in page and the three public pages ask the control plane things an
 * anonymous reader may ask, and a 401 there is a fact about the request, not
 * about the reader — sending them to sign in would be a redirect loop through
 * the very page that answers it.
 */

import { useEffect, useRef } from 'react';
import { useHref, useLocation } from 'react-router';

import { onSessionExpired } from '../api/client';
import { loginURL } from '../pages/login/urls';

export function useSessionGuard(routeIsPublic: boolean): void {
  const loginPath = useHref('/login');
  const here = useHref(useLocation());
  // Eight rollups poll in parallel on /monitor, so an expired session produces
  // eight 401s inside one tick. The first one owns the navigation; assigning
  // location.href seven more times would restart it and lose the return trip.
  const leaving = useRef(false);

  useEffect(() => {
    // Cleared here, and not only set: the ref is about one navigation, and the
    // effect re-runs on the one thing that ends it. Left standing it silenced
    // every later expiry for the life of the document — a reader who was sent to
    // sign in, came back, and expired again on another route kept the error box
    // and the still-running poll this hook exists to replace.
    leaving.current = false;
    if (routeIsPublic) {
      return undefined;
    }
    return onSessionExpired(() => {
      if (leaving.current) {
        return;
      }
      leaving.current = true;
      // A full navigation, not a client-side one: the credential is gone, so
      // every request in flight and every timer above it should go with it.
      window.location.href = loginURL({ loginPath, stepUp: false, returnTo: here });
    });
  }, [routeIsPublic, loginPath, here]);
}
