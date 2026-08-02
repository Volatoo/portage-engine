/**
 * Which project the console is speaking for.
 *
 * This is not a preference. `ResolveProject` in internal/persistence/iam.go
 * refuses an empty selector for any non-system-admin holding more than one
 * membership — "X-Project-ID is required when the identity has %d projects" —
 * and handlers_build.go resolves the build list and the build detail through it.
 * So a reader with two projects and no header gets no overview, no build list
 * and no build detail at all: the header is what makes those pages exist for
 * exactly the readers the switcher was put there for.
 *
 * The selection is state here and a header in `client.ts`, seeded from the IAM
 * answer and written to the same `localStorage` key the console this replaces
 * used, so a reader who chose a project there is still in it after the port.
 *
 * `ready` is the second half of what the old `api()` did. It awaited `iamReady`
 * before every request, so nothing went out ahead of the answer naming the
 * projects; a request issued in front of that answer is precisely the unscoped
 * one the control plane refuses. Pages gate their first load on it rather than
 * firing, failing, and recovering on the next tick.
 */

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';

import type { IdentityContext } from './capabilities';

/** The key the console this replaces wrote. Same key, so the choice survives. */
export const PROJECT_STORAGE_KEY = 'pe_project_id';

function readStoredProject(): string {
  try {
    return localStorage.getItem(PROJECT_STORAGE_KEY) ?? '';
  } catch {
    // Reading storage can throw rather than answer — a browser with site data
    // blocked throws on the property itself — and a console that cannot
    // remember the choice still has to render.
    return '';
  }
}

function storeProject(projectID: string): void {
  try {
    localStorage.setItem(PROJECT_STORAGE_KEY, projectID);
  } catch {
    // Same reason as above. Failing to persist costs the reader one extra
    // choice on the next load; failing to render costs them the console.
  }
}

export interface ProjectScope {
  /** Sent as `X-Project-ID`. Empty when the reader holds none, or IAM refused. */
  projectID: string;
  /**
   * Whether the selection is settled: false until IAM has answered, whatever
   * the answer was. A scoped request issued before this is the one the control
   * plane rejects, so it is also the gate on a page's first load.
   */
  ready: boolean;
}

/**
 * Seeded unresolved rather than null. A surface rendered outside the provider —
 * a test, an error boundary that replaced the tree — asks for nothing rather
 * than asking with a selector nobody chose.
 */
export const ProjectScopeContext = createContext<ProjectScope>({ projectID: '', ready: false });

export function useProjectScope(): ProjectScope {
  return useContext(ProjectScopeContext);
}

export interface ProjectSelection {
  /** What every page sends. One object, so consumers re-render when it changes. */
  scope: ProjectScope;
  /**
   * What the switcher writes. The list it offers is read straight off the
   * identity, so the control and the reconciliation below cannot come to
   * different conclusions about which projects exist.
   */
  select: (projectID: string) => void;
}

/**
 * The selection, reconciled against what IAM says this reader actually holds.
 *
 * A stored id is a claim about a membership, not a membership: a project the
 * reader has been removed from must not stay in the header, because every
 * request carrying it is refused with a sentence about a project rather than
 * about access. So the stored value is honoured only while it names a project
 * in the answer, and the first membership is the fallback — which is what the
 * console this replaces did in initIAMContext.
 */
export function useProjectSelection(identity: IdentityContext): ProjectSelection {
  const [chosen, setChosen] = useState<string>(() => readStoredProject());
  const projects = identity.projects;
  const resolved = identity.resolved;

  const projectID = useMemo(() => {
    if (!resolved) {
      return '';
    }
    if (projects.some((project) => project.project_id === chosen)) {
      return chosen;
    }
    return projects[0]?.project_id ?? '';
  }, [chosen, projects, resolved]);

  // Written after the reconciliation and not at the click, so what is stored is
  // always a project this reader holds: a stale id would otherwise be handed
  // back to the next load and refused there.
  useEffect(() => {
    if (projectID !== '') {
      storeProject(projectID);
    }
  }, [projectID]);

  const select = useCallback((next: string) => {
    setChosen(next);
  }, []);

  // One object, held across renders that did not change it, so a page consuming
  // the scope re-runs its load when the project changes and on no other render.
  // The console this replaces reloaded the document instead, which threw away
  // the scrollback of every surface on the page to change one header.
  const scope = useMemo(() => ({ projectID, ready: resolved }), [projectID, resolved]);
  return { scope, select };
}
