/**
 * The names /monitor calls the two shapes it reads, and nothing else.
 *
 * This module used to hold a second transcription of `/api/scheduler/status`
 * and a local correction of `/api/runtime-metadata/status`, written because the
 * shared types in src/api/types.ts stopped short of what those two handlers
 * answer. Both have been carried into the shared module, which is the only
 * place either shape is declared now: a second transcription is a second thing
 * to keep true, and the two had already disagreed about five field names that
 * exist on neither handler.
 *
 * What is left here is the page's vocabulary for them. `MonitorSchedulerStatus`
 * is `SchedulerStatus` without `builders`: that key is the in-memory queue's
 * per-instance task list, which /monitor answers with the instance table and
 * has never read off the scheduler. Deriving the view rather than restating it
 * is what makes drift impossible — a field added to the wire type arrives here
 * without anyone remembering to add it twice.
 */

import type { SchedulerStatus } from '../../api/types';

export type MonitorSchedulerStatus = Omit<SchedulerStatus, 'builders'>;

export type {
  CapacityActionStatus,
  CapacityActuatorStatus,
  CapacityInstanceStatus,
  LeaseExpiryStatus,
  MonitorProjectionStatus,
  RuntimeMetadataEnvelope,
  SchedulerAutoscaleStatus,
  SchedulerCapacityPoolStatus,
  SchedulerFairnessStatus,
  TargetHistoryStatus,
  TargetReliabilityStatus,
  TargetReliabilityWindow,
  WorkerDecisionStatus,
  WorkerScoringStatus,
} from '../../api/types';
