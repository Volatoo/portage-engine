/**
 * Finding one job among the ones the list endpoint handed over.
 *
 * Pure, because the honest half of this page is arithmetic: /api/v1/builds/list
 * takes `limit` and nothing else, so the query and the page number are applied
 * to the rows already in hand, and the page has to be able to say exactly which
 * rows those are.
 */

import type { BuildStatus } from '../../api/types';

/** How many rows a page of the table holds. */
export const PAGE_SIZE = 25;

/**
 * Whether a row answers the query, across the columns the reader can see.
 *
 * Only those columns: matching a field the table does not show produces a result
 * set nobody can explain, and the job id counts because the id is what an
 * operator pastes in from a CLI submission.
 */
export function matchesQuery(build: BuildStatus, query: string): boolean {
  const needle = query.trim().toLowerCase();
  if (needle === '') {
    return true;
  }
  return [build.package_name, build.version, build.arch, build.job_id].some((field) =>
    field.toLowerCase().includes(needle),
  );
}

export interface Page<T> {
  /** The rows this page shows. */
  rows: readonly T[];
  /** 1-based, clamped into range: a page past the end of a list that just shrank. */
  page: number;
  pages: number;
  /** 1-based index of the first row on this page, for the range readout. */
  first: number;
}

/**
 * One page of rows.
 *
 * The clamp is here rather than in an effect that corrects the number
 * afterwards: a page number past the end is a render the table never has to
 * perform, and correcting it after the fact means one frame showing an empty
 * page that is not the empty state.
 */
export function paginate<T>(rows: readonly T[], requested: number, size = PAGE_SIZE): Page<T> {
  const pages = Math.max(1, Math.ceil(rows.length / size));
  const page = Math.min(Math.max(1, requested), pages);
  const from = (page - 1) * size;
  return { rows: rows.slice(from, from + size), page, pages, first: from + 1 };
}
