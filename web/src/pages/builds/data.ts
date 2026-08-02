/**
 * How the four build pages get their data: one polled fetch and one identity.
 *
 * The poll keeps the last answer that carried a payload as well as the latest
 * answer of any kind, and both are handed to `resolveState`. That pair is the
 * difference between a surface that blanks itself because one poll blipped and
 * one that keeps the reading it has and says the refresh failed — which are
 * different facts, and on a page an operator is watching a build from, only one
 * of them is honest.
 */

import { useCallback, useEffect, useRef, useState } from 'react';

import type { ApiOutcome } from '../../api/client';
import { pollWhenVisible } from '../../api/stream';
import { singleWriter } from '../../api/write';
import type { SingleWriter } from '../../api/write';
import { useIdentity } from '../../app/capabilities';
import type { IdentityContext } from '../../app/capabilities';
import { CAPABILITY_SYSTEM_ADMIN } from '../../app/routes';

/**
 * Who the reader is, for the pages that gate a control on it.
 *
 * `useIdentity` asks IAM once per mount and starts with no capabilities, so a
 * destructive control is hidden until IAM has actually granted it and stays
 * hidden if IAM never answers. The flag comes from the boot payload the router
 * hands the page: a deployment with authentication off has nothing to ask and
 * nothing to gate, and asking anyway would answer 404 on every mount.
 */
export function useConsoleIdentity(authEnabled: boolean): IdentityContext {
  return useIdentity(authEnabled);
}

const ROLE_RANK: Record<string, number> = { viewer: 1, developer: 2, maintainer: 3, owner: 4 };
const MAINTAINER_RANK = 3;

/**
 * Whether this reader may write to this job.
 *
 * Against the job's OWN project, not against whichever project a switcher last
 * wrote to storage: a maintainer of project A must not be offered Delete on a
 * job belonging to project B just because A is the one selected. Unresolved
 * identity is refused, which is what keeps the direction fail-closed — the
 * control is never rendered and then taken away.
 */
export function canMaintainJob(identity: IdentityContext, projectID: string | undefined): boolean {
  if (!identity.resolved) {
    return false;
  }
  if (identity.granted.has(CAPABILITY_SYSTEM_ADMIN)) {
    return true;
  }
  if (projectID === undefined || projectID === '') {
    return false;
  }
  const access = identity.projects.find((project) => project.project_id === projectID);
  if (access === undefined) {
    return false;
  }
  return (ROLE_RANK[access.role] ?? 0) >= MAINTAINER_RANK;
}

/**
 * A `singleWriter` whose in-flight flag outlives the render that made it.
 *
 * The flag belongs to the resource and not to a render: a writer rebuilt because
 * one of its closed-over values changed identity comes back with the flag clear,
 * which is exactly the second activation the writer exists to discard. So the
 * writer is built once and the body it runs is read from a ref that every render
 * refreshes — the guard is stable, the values it writes with are current.
 */
export function useSingleWriter(write: () => Promise<void>): SingleWriter<void> {
  const latest = useRef(write);
  useEffect(() => {
    latest.current = write;
  });
  const [writer] = useState(() => singleWriter(() => latest.current()));
  return writer;
}

export interface PolledResource<T> {
  /** The latest answer to the current `load`. `null` until one has landed. */
  outcome: ApiOutcome<T> | null;
  /** The most recent answer that carried a payload, kept across a failed poll. */
  lastOk: T | null;
  /** True while the first request for the current `load` is still out. */
  loading: boolean;
  /** Fetch now. Returns a promise so a write can await the refresh it triggers. */
  refresh: () => Promise<void>;
}

/** What a page hands the poll: one request, cancellable. */
type Load<T> = (signal: AbortSignal) => Promise<ApiOutcome<T>>;

/**
 * Fetch on mount, then on a timer while the tab is visible.
 *
 * `load` has to be stable — wrap it in `useCallback` — because it is the
 * effect's dependency: an inline lambda would tear the poll down and set it up
 * again on every render, which is a request per render rather than per
 * interval.
 *
 * `ready` is the gate the old console's `api()` had as `await iamReady`: every
 * build route resolves its project from `X-Project-ID`, so a request issued
 * before IAM has said which project that is comes back refused for a reader
 * holding more than one. Not ready means not asked, which leaves the surface
 * loading — which is what it is.
 */
export function usePolledResource<T>(
  load: Load<T>,
  intervalMS: number,
  ready = true,
): PolledResource<T> {
  /**
   * The answer, and the `load` it is an answer to.
   *
   * Kept together rather than left standing when the request they came from is
   * gone: `load` closes over the project and, on the detail and log routes, over
   * the job, so an answer held past a change of either is one project's jobs
   * under another project's header — and it survives forever when the new
   * project is refused, because a refusal carries no payload to replace it with.
   * Naming what the answer is about makes a stale one unrenderable instead of
   * merely unlikely, the way ProjectQuota already does it.
   */
  const [answer, setAnswer] = useState<{
    load: Load<T>;
    outcome: ApiOutcome<T>;
    lastOk: T | null;
  } | null>(null);
  const tickRef = useRef<() => Promise<void>>(() => Promise.resolve());
  /** Issued and accepted request counts, so the newest answer is the one shown. */
  const issued = useRef(0);
  const accepted = useRef(0);

  useEffect(() => {
    if (!ready) {
      return undefined;
    }
    const controller = new AbortController();
    let disposed = false;
    const tick = async (): Promise<void> => {
      issued.current += 1;
      const sequence = issued.current;
      const next = await load(controller.signal);
      // An aborted request answers as a transport error; writing it would put
      // an unmounted page's failure on the page that replaced it. An answer
      // older than one already accepted is discarded for the same reason a
      // stale project's is: Refresh and the poll are two requests in flight at
      // once, they answer in whatever order the network settles them, and the
      // one the reader asked for last is the one that is current.
      if (disposed || sequence <= accepted.current) {
        return;
      }
      accepted.current = sequence;
      setAnswer((held) => ({
        load,
        outcome: next,
        // The payload is kept across a failed poll — but only across a failure
        // of the same request, never carried onto a different one.
        lastOk:
          next.kind === 'ok'
            ? next.value
            : held !== null && held.load === load
              ? held.lastOk
              : null,
      }));
    };
    tickRef.current = tick;
    void tick();
    const stop = pollWhenVisible(() => {
      void tick();
    }, intervalMS);
    return () => {
      disposed = true;
      controller.abort();
      stop();
      tickRef.current = () => Promise.resolve();
    };
  }, [load, intervalMS, ready]);

  const refresh = useCallback(() => tickRef.current(), []);
  // Read in the same render the request belongs to: an answer about anything
  // else is not an answer yet, which is what loading means.
  const current = answer !== null && answer.load === load ? answer : null;
  return {
    outcome: current === null ? null : current.outcome,
    lastOk: current === null ? null : current.lastOk,
    loading: current === null,
    refresh,
  };
}
