/**
 * What a build is made of, in one place: the nine stages, the state each of
 * them is in, the filters over the log they write, and the settings an error
 * points at.
 *
 * Every function here is pure and takes the two payloads a job has — its record
 * and its log document — because the pipeline row, the nine stage cards and the
 * failure report are three readings of the same two payloads. In the console
 * this replaces they were three independent readings taken at whichever moment
 * their own fetch answered, and they disagreed: the chip said a stage had
 * failed while the card under it showed that stage's last line, which was a
 * passing message.
 */

import type { BuildLogStage, BuildStatus } from '../../api/types';
import type { MessageKey } from '../../i18n/messages';

/** The six things a stage can be. Not five, and not a boolean. */
export type StageState = 'done' | 'current' | 'failed' | 'skipped' | 'stopped' | 'pending';

export interface Stage {
  /** The id the log summary uses, which is also the stage's own name. */
  key: string;
  labelKey: MessageKey;
  /**
   * The substring whose presence in the log proves the stage started. Queued
   * has none: nothing is written when a job is accepted, so the first stage is
   * read from the job's status alone.
   */
  marker?: string;
}

export const STAGES: readonly Stage[] = [
  { key: 'queued', labelKey: 'pipe.queued' },
  { key: 'provision', labelKey: 'pipe.provision', marker: '[provision]' },
  { key: 'deploy', labelKey: 'pipe.deploy', marker: '[deploy]' },
  { key: 'build', labelKey: 'pipe.build', marker: '[build] submitting' },
  { key: 'collect', labelKey: 'pipe.collect', marker: '[collect]' },
  { key: 'verify', labelKey: 'pipe.verify', marker: '[verify]' },
  { key: 'sign', labelKey: 'pipe.sign', marker: '[sign]' },
  { key: 'publish', labelKey: 'pipe.publish', marker: '[publish]' },
  { key: 'cleanup', labelKey: 'pipe.cleanup', marker: '[cleanup]' },
];

/**
 * One table for the six states, for the same reason the status vocabulary is
 * one table: the chip and the card below it are two renderings of one reading,
 * and a state that had no word would otherwise take whichever branch happened
 * to be last.
 */
export const STAGE_STATE_WORDS: Record<StageState, MessageKey> = {
  done: 'stage.done',
  current: 'stage.current',
  failed: 'stage.failed',
  skipped: 'stage.skipped',
  stopped: 'stage.stopped',
  pending: 'stage.pending',
};

/** The statuses a job never leaves. */
const TERMINAL = new Set(['failed', 'completed', 'success', 'canceled']);

export function isTerminal(status: string): boolean {
  return TERMINAL.has(status);
}

/**
 * How far a status alone says the job got, for the case where the log is
 * truncated or has not reached the console yet.
 */
const STATUS_STAGE: Record<string, number> = {
  queued: 0,
  claimed: 0,
  provisioning: 1,
  deploying: 2,
  forwarding: 3,
  building: 3,
  collecting: 4,
  verifying: 5,
  signing: 6,
  publishing: 7,
};

function stageState(
  index: number,
  reached: number,
  status: string,
  failedIndex: number,
  cleanupDone: boolean,
): StageState {
  if (status === 'completed' || status === 'success') {
    return 'done';
  }
  // Nothing is queued behind a job that stopped. Everything past the stopping
  // point read "pending" before, which is the word for a stage still waiting
  // its turn on a job that is still running — it said the pipeline was about to
  // carry on from the stage it had just failed in. Release is the one
  // exception: it runs whatever happened, so it is read from the log rather
  // than from its position in the row.
  if (status === 'failed' || status === 'canceled') {
    const stopIndex = status === 'failed' && failedIndex >= 0 ? failedIndex : reached;
    if (STAGES[index]?.key === 'cleanup') {
      return cleanupDone ? 'done' : 'skipped';
    }
    if (index === stopIndex) {
      return status === 'failed' ? 'failed' : 'stopped';
    }
    // A failure can be recorded against a stage whose marker never reached the
    // log, so position alone cannot say a stage ran: the markers do.
    if (index < stopIndex) {
      return index <= reached ? 'done' : 'skipped';
    }
    return 'skipped';
  }
  if (index < reached) {
    return 'done';
  }
  return index === reached ? 'current' : 'pending';
}

/**
 * The one reading of the nine stages, taken from the job record and the log
 * together and handed to everything that draws them.
 */
export function stageStates(job: BuildStatus | null, logText: string): readonly StageState[] {
  if (job === null) {
    // No record is a state, not a shape to paint around. A pipeline drawn from
    // an empty object marked Queued as running and pulsed its dot — a live
    // reading of a job that had failed hours before.
    return [];
  }
  const status = job.status;
  let reached = 0;
  STAGES.forEach((stage, index) => {
    if (stage.marker !== undefined && logText.includes(stage.marker)) {
      reached = index;
    }
  });
  // Status is authoritative when it maps further than the (possibly truncated)
  // log markers.
  const mapped = STATUS_STAGE[status];
  if (mapped !== undefined && mapped > reached) {
    reached = mapped;
  }
  if (status === 'completed' || status === 'success') {
    reached = STAGES.length - 1;
  }
  const failedIndex = STAGES.findIndex((stage) => stage.key === job.failed_stage);
  const cleanupDone = logText.includes('[cleanup]');
  return STAGES.map((_stage, index) =>
    stageState(index, reached, status, failedIndex, cleanupDone),
  );
}

/** Which stage the eye should land on: the one that failed, else the one running. */
export function focusStage(states: readonly StageState[]): number {
  const failed = states.indexOf('failed');
  return failed >= 0 ? failed : states.indexOf('current');
}

export interface LogFilter {
  key: string;
  labelKey: MessageKey;
  /** The stage id whose line count this chip reports, when it differs from `key`. */
  stage?: string;
  /** Absent on `all`, which selects the whole log. */
  prefixes?: readonly string[];
}

/**
 * One filter table for the embedded pane and for the full-log page.
 *
 * They were two tables, and the shorter one silently dropped Sign and Publish —
 * so the two pages over one log document offered different answers to "show me
 * the publish step", which is the step most failures happen in.
 */
export const LOG_FILTERS: readonly LogFilter[] = [
  { key: 'all', labelKey: 'filter.all' },
  { key: 'queued', labelKey: 'filter.queued', prefixes: ['[queued]'] },
  {
    key: 'provision',
    labelKey: 'filter.provision',
    prefixes: ['[provision]', '[terraform]', '[policy]'],
  },
  { key: 'policy', labelKey: 'filter.policy', prefixes: ['[policy]'] },
  { key: 'deploy', labelKey: 'filter.deploy', prefixes: ['[deploy]', '[remote]'] },
  { key: 'build', labelKey: 'filter.build', prefixes: ['[build]'] },
  { key: 'collect', labelKey: 'filter.collect', prefixes: ['[collect]'] },
  { key: 'verify', labelKey: 'filter.verify', prefixes: ['[verify]'] },
  { key: 'sign', labelKey: 'filter.sign', prefixes: ['[sign]'] },
  { key: 'publish', labelKey: 'filter.publish', prefixes: ['[publish]'] },
  { key: 'release', labelKey: 'filter.release', stage: 'cleanup', prefixes: ['[cleanup]'] },
];

export function findFilter(key: string): LogFilter {
  return LOG_FILTERS.find((filter) => filter.key === key) ?? (LOG_FILTERS[0] as LogFilter);
}

/** Non-blank lines, which is what the whole-log chip counts. */
export function countLogLines(text: string): number {
  return text.split('\n').filter((line) => line.trim() !== '').length;
}

/** The lines a filter selects, in order. */
export function filterLines(text: string, filter: LogFilter): string[] {
  const lines = text.split('\n');
  const prefixes = filter.prefixes;
  if (prefixes === undefined) {
    return lines;
  }
  return lines.filter((line) => prefixes.some((prefix) => line.includes(prefix)));
}

/**
 * The number on a chip, and the number the pane under it is held to.
 *
 * It comes from the server's own stage summary rather than from counting the
 * text, because that is the number the chip claims — holding the pane to a
 * count the pane itself produced would make the check vacuous.
 */
export function filterLineCount(
  filter: LogFilter,
  text: string,
  stages: readonly BuildLogStage[],
): number {
  if (filter.key === 'all') {
    return countLogLines(text);
  }
  const id = filter.stage ?? filter.key;
  return stages.find((stage) => stage.id === id)?.line_count ?? 0;
}

/**
 * The words the control plane writes when a step reports a failure.
 *
 * The builder writes every event through one appender, so a log line carries no
 * level to read: "[publish] artifact promotion failed: …" and "[publish]
 * artifact byte gate passed: …" are the same shape, and taking the last line of
 * a stage put a PASSING message on the card of the stage that failed. In one
 * table so the next word is added here rather than branched around.
 */
const FAILURE_WORDS = [
  'failed',
  'failure',
  'error',
  'rejected',
  'denied',
  'refused',
  'aborted',
  'timed out',
  'unavailable',
  'not configured',
  'is disabled',
];

/**
 * The last line of a stage that says something went wrong, searched backwards.
 *
 * A warning is a step that carried on regardless; it is the answer only when the
 * stage reported nothing worse than itself.
 */
export function lastFailureLine(text: string, stageID: string): string {
  const marker = `[${stageID}]`;
  const lines = text.split('\n');
  let warning = '';
  for (let index = lines.length - 1; index >= 0; index -= 1) {
    const line = lines[index] ?? '';
    if (!line.includes(marker)) {
      continue;
    }
    const lower = line.toLowerCase();
    if (!FAILURE_WORDS.some((word) => lower.includes(word))) {
      continue;
    }
    if (lower.includes('warning:')) {
      if (warning === '') {
        warning = line;
      }
      continue;
    }
    return line;
  }
  return warning;
}

/**
 * Where the value an error names is written.
 *
 * An error string is the only machine-readable thing a failure hands the
 * console, so each entry matches the token inside it that names the value — the
 * deployment key the control plane refused on, or the sentence the storage layer
 * answers with — and then says where that value is set. Two shapes, because the
 * console owns some of these settings and not others: a destination it can link
 * to, or a deployment key it can only name. Linking to a field that is not there
 * would send the reader somewhere the value is not.
 */
export type Remedy =
  | {
      kind: 'console';
      /** Where the field lives, as a console path with the panel's own fragment. */
      href: string;
      fieldKey: MessageKey;
      sectionKey: MessageKey;
    }
  | { kind: 'deployment'; env: string };

interface RemedyRule {
  match: string;
  remedy: Remedy;
}

const ERROR_REMEDIES: readonly RemedyRule[] = [
  {
    match: 'SERVER_CALLBACK_URL',
    remedy: {
      kind: 'console',
      href: '/settings#net',
      fieldKey: 'set.callback',
      sectionKey: 'set.sec.net',
    },
  },
  {
    match: 'CLOUD_BUILDER_BINARY_URL',
    remedy: {
      kind: 'console',
      href: '/settings#net',
      fieldKey: 'set.binurl',
      sectionKey: 'set.sec.net',
    },
  },
  {
    match: 'CLOUD_SSH_KEY_PATH',
    remedy: {
      kind: 'console',
      href: '/settings#ssh',
      fieldKey: 'set.keypath',
      sectionKey: 'set.sec.ssh',
    },
  },
  // internal/storage/s3.go — ErrDeleteDisabled. The setting exists, but it is a
  // deployment variable and not a console field, so it is named and not linked.
  {
    match: 'S3 object deletion is disabled',
    remedy: { kind: 'deployment', env: 'STORAGE_S3_ALLOW_DELETE' },
  },
  // internal/storage/generation.go — a generation whose repository provenance
  // carries no revision and no digest. The repositories are declared by the
  // catalog the image factory publishes, which is where an operator fixes it.
  {
    match: 'generation repository identity and revision or digest are required',
    remedy: {
      kind: 'console',
      href: '/image-factory',
      fieldKey: 'factory.catalog',
      sectionKey: 'nav.factory',
    },
  },
];

/** The first rule whose token the message carries, or none. */
export function findRemedy(message: string): Remedy | null {
  return ERROR_REMEDIES.find((rule) => message.includes(rule.match))?.remedy ?? null;
}
