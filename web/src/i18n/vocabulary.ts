/**
 * The backend's status vocabulary, in one table.
 *
 * The colour map and the label catalogue used to be two hand-maintained mirrors
 * of this one vocabulary and had already diverged: the builder emits `busy` and
 * `draining`, which had a colour in one table and no label in the other, so the
 * badge rendered a raw English token on a Chinese page.
 *
 * A token this table does not carry falls to gray with its own raw name — see
 * `resolveStatus` in ./status.ts. That catch-all is the whole reason this is a
 * lookup and not a conditional chain: a status added upstream renders neutrally
 * and legibly instead of taking whichever branch happened to be last.
 *
 * Extracted from internal/dashboard/ui.go; not retyped.
 */

/** The five dot hues the badge can wear, and the only five. */
export type StatusColor = 'green' | 'blue' | 'orange' | 'red' | 'gray';

export const STATUS_VOCABULARY = {
  queued: 'gray',
  claimed: 'orange',
  provisioning: 'orange',
  forwarding: 'orange',
  deploying: 'orange',
  building: 'blue',
  collecting: 'blue',
  verifying: 'blue',
  signing: 'blue',
  publishing: 'blue',
  success: 'green',
  completed: 'green',
  failed: 'red',
  canceled: 'gray',
  online: 'green',
  busy: 'blue',
  draining: 'orange',
  offline: 'red',
  running: 'green',
  destroy_failed: 'red',
  healthy: 'green',
  unhealthy: 'red',
  degraded: 'red',
  pending: 'gray',
  active: 'green',
  revoked: 'red',
  not_started: 'gray',
  planned: 'gray',
  in_progress: 'blue',
  blocked: 'orange',
  passed: 'green',
  // handlers_public.go answers the anonymous status page in its own two-word
  // vocabulary — `operational` and `degraded` — and only the second of them was
  // here. The page that reads it had therefore built its own label and colour
  // functions around this table, and their catch-all was red plus the word
  // "Degraded": a component state added upstream was reported to the public as
  // a fault. `degraded` already sits above; this is the other half.
  operational: 'green',
  // The artifact half of the same idea, in the spelling the build record
  // already uses (`BuildStatus.signed`). `unsigned` is orange and not red: a
  // binhost may legitimately publish unsigned while a deployment is still
  // establishing its signing trust, and the client-side verification the docs
  // page insists on is what actually refuses it.
  signed: 'green',
  unsigned: 'orange',
} as const satisfies Record<string, StatusColor>;

/** Every status token this console has a colour and a translation for. */
export type StatusToken = keyof typeof STATUS_VOCABULARY;
