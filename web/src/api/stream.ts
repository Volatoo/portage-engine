/**
 * The two long-lived connections, and the poll that is the correctness fallback
 * for one of them.
 *
 * Neither stream is authoritative. `/api/events/jobs` is a Redis fan-out the
 * server refuses outright when no cache is configured, and it says so in the
 * refusal: "live events require Redis; poll build status as fallback". So a
 * surface subscribes to shorten the latency and polls to be correct, and losing
 * the stream costs freshness rather than truth.
 */

import type { JobEvent } from './types';

/**
 * Subscribe to one project's job events. Returns the unsubscribe.
 *
 * `EventSource` reconnects by itself, including after the server closes the
 * stream mid-flight — which it does deliberately, because the headers are
 * already committed by then and there is no status left to send.
 *
 * The project is required rather than optional, and required is the whole point:
 * an optional scope is one a caller forgets, and the subscription that forgets it
 * is the unscoped stream the control plane refuses for exactly the readers who
 * hold more than one project — the readers scoping exists for. Required, the
 * compiler names the call site; optional, a reviewer has to. An empty string is
 * still accepted, because a reader who holds no project has none to name.
 */
export function subscribeToJobEvents(
  onJob: (event: JobEvent) => void,
  options: { projectID: string },
): () => void {
  if (typeof EventSource === 'undefined') {
    return () => undefined;
  }
  const query =
    options.projectID === '' ? '' : `?project_id=${encodeURIComponent(options.projectID)}`;
  const source = new EventSource(`/api/events/jobs${query}`);
  const handle = (message: MessageEvent<string>): void => {
    try {
      const parsed: unknown = JSON.parse(message.data);
      if (typeof parsed === 'object' && parsed !== null && 'job_id' in parsed) {
        onJob(parsed as JobEvent);
      }
    } catch {
      // A frame that is not JSON is a frame from a proxy, not from us.
    }
  };
  source.addEventListener('job', handle);
  return () => {
    source.removeEventListener('job', handle);
    source.close();
  };
}

/** What the instance shell's socket is doing, in the vocabulary the page shows. */
export type ShellConnectionState = 'connecting' | 'connected' | 'closed' | 'error';

export interface ShellSocket {
  send(data: string): void;
  close(): void;
}

/**
 * Open the interactive shell for an instance.
 *
 * Call only after `api.shellPreflight` has answered `ok`. The handshake carries
 * no way to report a refusal — a rejected upgrade arrives as an untyped error
 * event with no status and no body — so the credential is established over plain
 * HTTP first, where the refusal names the authentication that would satisfy it.
 */
export function openShellSocket(
  instanceID: string,
  handlers: {
    onState: (state: ShellConnectionState) => void;
    onData: (chunk: string | Uint8Array) => void;
  },
): ShellSocket {
  const scheme = location.protocol === 'https:' ? 'wss://' : 'ws://';
  const socket = new WebSocket(
    `${scheme}${location.host}/api/shell?id=${encodeURIComponent(instanceID)}`,
  );
  socket.binaryType = 'arraybuffer';
  handlers.onState('connecting');
  socket.onopen = () => {
    handlers.onState('connected');
  };
  socket.onclose = () => {
    handlers.onState('closed');
  };
  socket.onerror = () => {
    handlers.onState('error');
  };
  socket.onmessage = (event: MessageEvent<string | ArrayBuffer>) => {
    handlers.onData(typeof event.data === 'string' ? event.data : new Uint8Array(event.data));
  };
  return {
    send(data) {
      if (socket.readyState === WebSocket.OPEN) {
        socket.send(data);
      }
    },
    close() {
      socket.close();
    },
  };
}

/**
 * Poll while the tab is visible, and never while it is not.
 *
 * A backgrounded tab used to keep every poll running: /monitor alone made 32
 * round trips a minute, about 15000 of them unread over an eight-hour day. The
 * first tick after the tab comes back is immediate, so returning to it never
 * shows numbers a whole interval out of date.
 */
export function pollWhenVisible(tick: () => void, intervalMS: number): () => void {
  let timer: ReturnType<typeof setInterval> | null = null;
  const stop = (): void => {
    if (timer !== null) {
      clearInterval(timer);
      timer = null;
    }
  };
  const start = (): void => {
    if (timer === null) {
      timer = setInterval(tick, intervalMS);
    }
  };
  const onVisibility = (): void => {
    if (document.hidden) {
      stop();
      return;
    }
    tick();
    start();
  };
  document.addEventListener('visibilitychange', onVisibility);
  if (!document.hidden) {
    start();
  }
  return () => {
    document.removeEventListener('visibilitychange', onVisibility);
    stop();
  };
}
