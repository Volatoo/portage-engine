/**
 * The log document, and the one question asked of it: is the pane lying?
 *
 * Pure, and separate from the components that draw it, because this is the part
 * worth a test. The pane and the chips above it are drawn from one payload, so
 * they can only disagree if a render did not land — and an empty box under a
 * chip reading "All (2628)" is a claim the reader has no way to check.
 */

import type { BuildLogStage, BuildLogs } from '../../api/types';
import { filterLineCount } from './stages';
import type { LogFilter } from './stages';

/** The log document, with every field this console reads given a value. */
export interface LogDocument {
  text: string;
  stages: readonly BuildLogStage[];
  bytes: number;
  generatedAt: string;
  truncated: boolean;
}

export const EMPTY_LOG: LogDocument = {
  text: '',
  stages: [],
  bytes: 0,
  generatedAt: '',
  truncated: false,
};

/**
 * Read the payload once, so no component reaches for a field by a second name.
 *
 * The text is `logs`, plural, which is what internal/server/handlers_build.go
 * encodes. Reading a differently-spelled field gives `undefined`, and undefined
 * renders as an empty pane under a chip claiming thousands of lines.
 */
export function readLogDocument(payload: BuildLogs | null): LogDocument {
  if (payload === null) {
    return EMPTY_LOG;
  }
  return {
    text: payload.logs ?? '',
    stages: payload.stages ?? [],
    bytes: payload.bytes ?? 0,
    generatedAt: payload.generated_at ?? '',
    truncated: payload.truncated === true,
  };
}

export type LogFault =
  | { kind: 'load'; message: string }
  /** The fetch reported `counted` lines and the pane holds none of them. */
  | { kind: 'blank'; counted: number }
  | null;

/**
 * Whether the pane is lying, decided from the numbers the fetch itself
 * reported.
 *
 * Three answers, and the third is the one that matters: no fault. A job that
 * genuinely produced no output is EMPTY, and saying "the log failed to load"
 * about a queued job would be as wrong in the other direction.
 *
 * A fault is either of two things. The chip claims lines for this filter and the
 * pane holds none of them; or the document arrived with a byte count and no text
 * at all, which is the case where chip and pane agree only because both are
 * reading nothing — the shape the embedded pane was in when it rendered zero
 * characters beside a reported 264 KiB.
 */
export function logFault(
  log: LogDocument,
  filter: LogFilter,
  renderedLines: number,
  loadError: string | null,
): LogFault {
  if (loadError !== null) {
    return { kind: 'load', message: loadError };
  }
  const counted = filterLineCount(filter, log.text, log.stages);
  if (counted > 0 && renderedLines === 0) {
    return { kind: 'blank', counted };
  }
  if (log.bytes > 0 && log.text === '') {
    const stageLines = log.stages.reduce((total, stage) => total + stage.line_count, 0);
    return { kind: 'blank', counted: stageLines };
  }
  return null;
}
