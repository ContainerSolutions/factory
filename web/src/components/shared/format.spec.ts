import { describe, expect, it } from 'vitest';
import {
  formatTime,
  formatValue,
  railVerdictLabel,
  safeURL,
  toneOf,
} from './format';

describe('formatTime', () => {
  it.each<[string | Date | undefined, string]>([
    ['2026-10-16T09:30:00Z', '2026-10-16 09:30:00 UTC'],
    ['2026-10-16T11:30:00+02:00', '2026-10-16 09:30:00 UTC'],
    [new Date('2026-10-16T09:30:00.123Z'), '2026-10-16 09:30:00 UTC'],
    ['yesterday', 'yesterday'],
    [undefined, ''],
  ])('%s → %s', (at, want) => {
    expect(formatTime(at)).toBe(want);
  });
});

describe('formatValue', () => {
  it.each<[unknown, string]>([
    ['running', 'running'],
    [3, '3'],
    [true, 'true'],
    [['a', 'b'], '["a","b"]'],
    [{ k: 1 }, '{"k":1}'],
    [null, 'null'],
    [undefined, 'undefined'],
  ])('%o → %s', (value, want) => {
    expect(formatValue(value)).toBe(want);
  });
});

describe('safeURL', () => {
  it.each<[string | undefined, string | undefined]>([
    ['https://github.com/o/r/pull/1', 'https://github.com/o/r/pull/1'],
    ['http://github.com/o/r/pull/1', undefined],
    ['javascript:alert(1)', undefined],
    [' https://x', undefined],
    ['', undefined],
    [undefined, undefined],
  ])('%s → %s', (url, want) => {
    expect(safeURL(url)).toBe(want);
  });
});

describe('toneOf', () => {
  it.each([
    ['needs-human', 'danger'],
    ['running', 'info'],
    ['merged', 'success'],
    ['Suppressed', 'danger'],
    ['Deferred', 'warn'],
    ['Pending', 'warn'],
    ['Ready', 'success'],
    ['Throttled', 'warn'],
    ['Offline', 'neutral'],
    ['needs-info', 'neutral'],
    [undefined, 'neutral'],
  ])('%s → %s', (status, want) => {
    expect(toneOf(status)).toBe(want);
  });
});

describe('railVerdictLabel', () => {
  it.each([
    ['allowed', 'Allowed'],
    ['suppressed', 'Suppressed'],
    ['deferred', 'Deferred'],
    ['unsupported', 'Unsupported'],
  ])('%s → %s', (verdict, want) => {
    expect(railVerdictLabel(verdict)).toBe(want);
    expect(toneOf(want)).not.toBe('neutral');
  });
});
