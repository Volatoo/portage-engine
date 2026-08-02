/**
 * Capability gating, failing closed.
 *
 * The boot payload names the reader and asserts no capability, on purpose: a
 * local session can be named from its own claims without leaving the process,
 * a federated one cannot, and neither carries authority. So the only source of a
 * capability is `/api/iam/me`, and until it answers there are none.
 *
 * The direction is what matters. A gated destination is never rendered and then
 * removed — that is a destination an operator can see, click, and be refused by,
 * and it is what an IAM outage produced when the removal pass ran after paint.
 * `granted` only ever grows, so an outage leaves an admin route hidden rather
 * than revealing it and taking it back.
 *
 * It starts empty on every deployment that has authentication, and on the one
 * that does not it starts with everything, for the reason `deploymentGrant`
 * below states.
 */

import { useEffect, useMemo, useState } from 'react';

import { api } from '../api/endpoints';
import type { Principal, ProjectAccess } from '../api/types';
import { CAPABILITY_SYSTEM_ADMIN } from './routes';

export interface IdentityContext {
  /** Empty until IAM answers. Never populated from the boot payload. */
  granted: ReadonlySet<string>;
  /** The name to show. Empty while unresolved. */
  displayName: string;
  projects: readonly ProjectAccess[];
  /** False until the first answer, whatever that answer was. */
  resolved: boolean;
  /** Set when IAM refused or could not be reached, for the surface that says so. */
  failure: string | null;
}

export const UNRESOLVED_IDENTITY: IdentityContext = {
  granted: new Set<string>(),
  displayName: '',
  projects: [],
  resolved: false,
  failure: null,
};

/** Two stable sets, so the union below is a value and not a new object a render. */
const NOTHING_GRANTED: ReadonlySet<string> = new Set<string>();
const EVERYTHING_GRANTED: ReadonlySet<string> = new Set<string>([CAPABILITY_SYSTEM_ADMIN]);

/**
 * What the deployment itself grants, before IAM has said anything.
 *
 * With `AUTH_ENABLED=false` the dashboard installs no auth middleware at all —
 * `Router` in internal/dashboard/dashboard.go only wraps the mux when the flag
 * is on — and the control plane it proxies to answers as
 * `anonymous-compatibility` with `SystemAdmin` set (`authenticateRequest` in
 * internal/server/iam.go). So /monitor, /image-factory and /settings are served
 * to whoever asks for them, and the console this replaces showed all three:
 * ui.go's removal pass ran off the same `/api/iam/me` answer whether the flag
 * was on or off, and never consulted the flag.
 *
 * Hiding them there is not failing closed, because nothing is closed. It is the
 * console disagreeing with the server about what this deployment is, and what it
 * cost was half the navigation on every deployment that runs without auth.
 *
 * With auth on this is empty, which is what leaves IAM as the only source of a
 * capability and keeps a refusal a refusal.
 */
function deploymentGrant(authEnabled: boolean): ReadonlySet<string> {
  return authEnabled ? NOTHING_GRANTED : EVERYTHING_GRANTED;
}

/**
 * Read the principal out of an answer that came back 200.
 *
 * `IAMMe` describes what internal/server/iam.go documents, not what arrived. A
 * proxy that answers 200 with `null`, or an upstream that drops the field, is a
 * payload this used to read straight through — and the `TypeError` that produced
 * was thrown inside a `then` nobody caught, so the promise rejected, `setIdentity`
 * never ran, and the chrome sat on "Loading identity…" forever against a request
 * that had already come back. Failing to read the answer is a failure to resolve
 * the identity, not a reason to stop resolving it.
 */
function principalOf(value: unknown): Principal | null {
  if (typeof value !== 'object' || value === null) {
    return null;
  }
  const principal = (value as { principal?: unknown }).principal;
  return typeof principal === 'object' && principal !== null ? (principal as Principal) : null;
}

function projectsOf(value: unknown): readonly ProjectAccess[] {
  const projects = (value as { projects?: unknown }).projects;
  return Array.isArray(projects) ? (projects as ProjectAccess[]) : [];
}

/**
 * Ask IAM once per mount.
 *
 * Deliberately not retried on a timer: a console that re-asks every fifteen
 * seconds turns a control-plane outage into a request storm, and the reader
 * already has the one action that matters — reload.
 *
 * Asked whether or not this deployment has authentication, because the name and
 * the project list are facts only IAM holds, and a console that skipped the call
 * with auth off had no project to put in `X-Project-ID` and nothing to write on
 * the identity line. What the flag decides is the floor under `granted`, not
 * whether the question gets asked.
 */
export function useIdentity(authEnabled: boolean): IdentityContext {
  const [answer, setAnswer] = useState<IdentityContext>(UNRESOLVED_IDENTITY);

  useEffect(() => {
    const controller = new AbortController();
    // Resolved with no capabilities and a stated reason. Resolved, because the
    // chrome has to stop saying "loading identity" at some point; no
    // capabilities, because a refusal is not a grant.
    const refused = (reason: string): void => {
      if (!controller.signal.aborted) {
        setAnswer({ ...UNRESOLVED_IDENTITY, resolved: true, failure: reason });
      }
    };
    void api
      .iamMe({ signal: controller.signal })
      .then((outcome) => {
        if (controller.signal.aborted) {
          return;
        }
        if (outcome.kind !== 'ok') {
          refused('message' in outcome ? outcome.message : outcome.kind);
          return;
        }
        const principal = principalOf(outcome.value);
        if (principal === null) {
          refused('the identity answer named no principal');
          return;
        }
        const granted = new Set<string>();
        if (principal.system_admin) {
          granted.add(CAPABILITY_SYSTEM_ADMIN);
        }
        setAnswer({
          granted,
          // Empty rather than absent when the answer names none of the three.
          // The chrome reads '' as "say the identity is unavailable"; it reads
          // an undefined as a child React renders as nothing at all, which is a
          // blank identity line an operator reads as "signed in as nobody".
          displayName:
            principal.display_name ?? principal.preferred_username ?? principal.subject ?? '',
          projects: projectsOf(outcome.value),
          resolved: true,
          failure: null,
        });
      })
      .catch((cause: unknown) => {
        refused(cause instanceof Error ? cause.message : String(cause));
      });
    return () => {
      controller.abort();
    };
  }, []);

  const floor = deploymentGrant(authEnabled);
  return useMemo(() => {
    if (floor.size === 0) {
      return answer;
    }
    return { ...answer, granted: new Set<string>([...floor, ...answer.granted]) };
  }, [answer, floor]);
}

/**
 * Whether a destination may be shown.
 *
 * A route with no declared capability is public to anyone with a session; a
 * route with one is shown only when IAM has actually granted it. There is no
 * third answer and no `unknown` that renders as visible.
 */
export function isGranted(identity: IdentityContext, capability: string | undefined): boolean {
  return capability === undefined || identity.granted.has(capability);
}
