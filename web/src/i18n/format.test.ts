import { describe, expect, it } from 'vitest';

import { formatBytes, formatDuration } from './format';

/**
 * The two formatters that print a magnitude rather than an instant.
 *
 * Both are read the way an operator reads them: against another reading on the
 * same screen. A byte count that silently changed unit without saying so, or a
 * duration that dropped its leading zero, is not wrong by much — it is wrong in
 * the one way that makes two numbers beside each other uncomparable, which is
 * the whole reason they are formatted here instead of at forty call sites.
 *
 * So the boundaries are the cases: the value just under a unit step and the
 * value at it, the value that carries no unit at all, and the value large
 * enough to run off the end of the ladder. Nothing below asserts a shape the
 * source does not already commit to in prose.
 */

const KIB = 1024;
const MIB = KIB * 1024;
const GIB = MIB * 1024;
const TIB = GIB * 1024;
const PIB = TIB * 1024;

describe('a byte count', () => {
  it('counts bytes with no fraction below the first step', () => {
    // Whole bytes, and grouped: the digits are what a reader compares, and
    // `1023 B` beside `1,023 B` is the difference between reading a number and
    // counting its digits.
    expect(formatBytes('en', 1)).toBe('1 B');
    expect(formatBytes('en', 512)).toBe('512 B');
    expect(formatBytes('en', KIB - 1)).toBe('1,023 B');
  });

  it('steps at exactly 1024 and at every power of it', () => {
    expect(formatBytes('en', KIB)).toBe('1 KiB');
    expect(formatBytes('en', MIB)).toBe('1 MiB');
    expect(formatBytes('en', GIB)).toBe('1 GiB');
    expect(formatBytes('en', TIB)).toBe('1 TiB');
    expect(formatBytes('en', PIB)).toBe('1 PiB');
  });

  it('keeps one fractional digit above the first step and none below it', () => {
    expect(formatBytes('en', 1536)).toBe('1.5 KiB');
    expect(formatBytes('en', MIB + MIB / 4)).toBe('1.3 MiB');
    // Bytes are counted, not measured: half a byte is not a reading this
    // product can produce, and a fraction of one would be noise in the column.
    expect(formatBytes('en', 512.7)).toBe('513 B');
  });

  it('stays on the last unit rather than inventing one past it', () => {
    // The ladder ends at PiB, so a value a thousand times larger is a large
    // number of PiB. Running off the end would index past the array and print
    // `undefined` as the unit.
    expect(formatBytes('en', KIB ** 6)).toBe('1,024 PiB');
    expect(formatBytes('en', Number.MAX_SAFE_INTEGER)).toBe('8 PiB');
  });

  it('answers nothing-there with zero rather than with a negative or a NaN', () => {
    // Every one of these has been on the wire: an absent field, a counter read
    // before its first sample, and a subtraction of two counters taken in the
    // wrong order.
    expect(formatBytes('en', 0)).toBe('0 B');
    expect(formatBytes('en', -1)).toBe('0 B');
    expect(formatBytes('en', -GIB)).toBe('0 B');
    expect(formatBytes('en', Number.NaN)).toBe('0 B');
    expect(formatBytes('en', Number.POSITIVE_INFINITY)).toBe('0 B');
    expect(formatBytes('en', Number.NEGATIVE_INFINITY)).toBe('0 B');
  });

  it('localises the number and not the unit', () => {
    // The symbols are SI-derived and the backend logs them this way, so they
    // are the same string in both catalogues — an operator pasting a console
    // reading into a bug report next to a server log line is comparing the two.
    expect(formatBytes('zh-CN', KIB)).toBe('1 KiB');
    expect(formatBytes('zh-CN', 1536)).toBe('1.5 KiB');
    expect(formatBytes('zh-CN', KIB - 1)).toBe('1,023 B');
  });
});

describe('a duration in seconds', () => {
  it('prints seconds alone under a minute', () => {
    expect(formatDuration('en', 0)).toBe('0s');
    expect(formatDuration('en', 9)).toBe('9s');
    expect(formatDuration('en', 59)).toBe('59s');
  });

  it('truncates to the whole second rather than rounding up into the next unit', () => {
    // A build that has run 59.9 seconds has not run a minute. Rounding here
    // would make the elapsed counter on /build tick to `1m 00s` while the
    // control plane still says 59.
    expect(formatDuration('en', 59.9)).toBe('59s');
    expect(formatDuration('en', 0.4)).toBe('0s');
    expect(formatDuration('en', 3599.999)).toBe('59m 59s');
  });

  it('adds a field at each boundary and pads the ones behind the leading unit', () => {
    // The padding is the point: `1h 6m 9s` and `1h 06m 09s` in one column do
    // not line up, and this column is read down.
    expect(formatDuration('en', 60)).toBe('1m 00s');
    expect(formatDuration('en', 61)).toBe('1m 01s');
    expect(formatDuration('en', 3599)).toBe('59m 59s');
    expect(formatDuration('en', 3600)).toBe('1h 00m 00s');
    expect(formatDuration('en', 3969)).toBe('1h 06m 09s');
  });

  it('lets the leading unit grow without a ceiling', () => {
    // Nothing wraps at 24: a queue age or a cumulative build budget is hours,
    // and `1d 4h` would be a third format for a reader to learn.
    expect(formatDuration('en', 86400)).toBe('24h 00m 00s');
    expect(formatDuration('en', 360000)).toBe('100h 00m 00s');
    // Grouped in the leading unit and never in the padded ones, which is what
    // `useGrouping: false` on the pad is there for.
    expect(formatDuration('en', 3600000)).toBe('1,000h 00m 00s');
  });

  it('refuses to print a duration it does not have', () => {
    // A negative interval is two clock readings in the wrong order — a Go zero
    // `updated_at` subtracted from a real `created_at` produces one — and the
    // honest answer is the dash, not `-1s` or `NaNs`.
    expect(formatDuration('en', -1)).toBe('-');
    expect(formatDuration('en', -3600)).toBe('-');
    expect(formatDuration('en', Number.NaN)).toBe('-');
    expect(formatDuration('en', Number.POSITIVE_INFINITY)).toBe('-');
    expect(formatDuration('en', Number.NEGATIVE_INFINITY)).toBe('-');
  });

  it('spells the units the same way in both catalogues', () => {
    // Deliberately not Intl.DurationFormat: a unit spelled in the reader's
    // language would make two durations in one operator's own screenshot
    // uncomparable across a language switch.
    expect(formatDuration('zh-CN', 3969)).toBe('1h 06m 09s');
    expect(formatDuration('zh-CN', 61)).toBe('1m 01s');
    expect(formatDuration('zh-CN', 3600000)).toBe('1,000h 00m 00s');
  });
});
