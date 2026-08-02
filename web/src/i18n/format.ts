/**
 * Every number, date and duration the console prints goes through here, and
 * every function takes the locale as a required first parameter.
 *
 * The diagnosis, from the console this replaces: a plain `toLocaleString()` with
 * no locale argument formats using the browser's own locale, not the one the
 * reader chose in the app — so a reader who picked English on a zh-CN machine
 * saw `2026/8/1` in the console and `8/1/2026` on the public pages of the same
 * deployment. Documenting that hazard was not enough the first time. Making the
 * locale a required positional parameter is: the call does not compile without
 * it, and eslint's no-restricted-syntax rejects a bare `Intl` construction and a
 * zero-argument `toLocale*` besides.
 */

/** The BCP 47 tag an app language formats with. */
export type LocaleTag = 'en' | 'zh-CN';

/**
 * Go's zero `time.Time` is on the wire.
 *
 * A `time.Time` field serialises as `0001-01-01T00:00:00Z` whether or not the
 * event it names has ever happened, and `omitempty` does not suppress it because
 * a struct is never empty. Put through a locale formatter it came back as
 * `1/1/1, 8:05:43 AM` — a plausible clock reading standing in for "never",
 * printed beside the ledger's write-error count, which are exactly the two
 * numbers a reader uses to decide whether the ledger can be trusted.
 *
 * Every formatter below resolves its value here first, so "this is not an
 * instant" is one state decided in one place rather than a special case
 * pattern-matched inside each of them. The floor is a year, not that one
 * literal: nothing this product can produce predates the epoch it computes in,
 * and a second zero value would otherwise need a second special case.
 */
export function instant(value: string | number | Date | null | undefined): Date | null {
  if (value === null || value === undefined || value === '') {
    return null;
  }
  const moment = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(moment.getTime())) {
    return null;
  }
  return moment.getUTCFullYear() < 1900 ? null : moment;
}

/**
 * What a clock reading is, on every route.
 *
 * `new Intl.DateTimeFormat(locale)` with no options is a DATE — no hour, no
 * minute, no second — while the `toLocaleString()` in fmtTime that this port
 * replaces was date AND time. So an options-less call here does not merely
 * format differently from the old console, it drops a field: /builds printed
 * `8/1/2026` in both its Created and Updated columns for jobs queued minutes
 * apart, which is the pair of columns an operator reads to tell a job that has
 * been sitting there from one that just landed.
 *
 * It is the DEFAULT and not merely a constant callers may reach for, because
 * the hazard is precisely that the bare call looks correct and compiles. Two
 * pages here already spelled the options out to avoid it and four pages did
 * not. A caller that genuinely wants a date with no time of day now has to ask
 * for one, which is the direction the mistake should run in.
 */
export const CLOCK_READING: Intl.DateTimeFormatOptions = {
  dateStyle: 'short',
  timeStyle: 'medium',
};

export function formatDateTime(
  locale: LocaleTag,
  value: string | number | Date | null | undefined,
  options: Intl.DateTimeFormatOptions = CLOCK_READING,
): string | null {
  const moment = instant(value);
  return moment === null ? null : new Intl.DateTimeFormat(locale, options).format(moment);
}

export function formatNumber(
  locale: LocaleTag,
  value: number,
  options: Intl.NumberFormatOptions = {},
): string {
  return new Intl.NumberFormat(locale, options).format(value);
}

/**
 * A byte count, in the units the rest of the platform reports in.
 *
 * The unit ladder is not localised because the symbols are SI-derived and the
 * backend logs them the same way; the number in front of it is, which is the
 * half a reader compares against another number on the same screen.
 */
const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'] as const;

export function formatBytes(locale: LocaleTag, bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) {
    return `0 ${BYTE_UNITS[0]}`;
  }
  let scaled = bytes;
  let step = 0;
  while (scaled >= 1024 && step < BYTE_UNITS.length - 1) {
    scaled /= 1024;
    step += 1;
  }
  const digits = step === 0 ? 0 : 1;
  return `${formatNumber(locale, scaled, { maximumFractionDigits: digits })} ${BYTE_UNITS[step]}`;
}

/**
 * A duration in seconds, as `1h 04m 09s`.
 *
 * Deliberately not `Intl.DurationFormat`: its support floor is above the
 * browsers this console is expected on, and its narrow style still spells the
 * units in the reader's language, which would make two builds' durations
 * uncomparable across a language switch in an operator's own screenshot.
 */
export function formatDuration(locale: LocaleTag, seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) {
    return '-';
  }
  const whole = Math.floor(seconds);
  const hours = Math.floor(whole / 3600);
  const minutes = Math.floor((whole % 3600) / 60);
  const rest = whole % 60;
  const pad = (value: number): string =>
    formatNumber(locale, value, { minimumIntegerDigits: 2, useGrouping: false });
  if (hours > 0) {
    return `${formatNumber(locale, hours)}h ${pad(minutes)}m ${pad(rest)}s`;
  }
  if (minutes > 0) {
    return `${formatNumber(locale, minutes)}m ${pad(rest)}s`;
  }
  return `${formatNumber(locale, rest)}s`;
}
