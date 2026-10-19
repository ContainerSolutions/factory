import { describe, expect, it } from 'vitest';
import type { Schemas } from '@/api/client';
import { liveRun } from '@/mocks/workitems';
import {
  formatCost,
  formatCount,
  isRunning,
  mergeProgress,
  runStateOf,
  turnOf,
} from './run';

type ProgressEvent = Schemas['ProgressEvent'];

function withStatus(status: Schemas['Run']['status']): Schemas['Run'] {
  return { ...liveRun, status };
}

describe('runStateOf', () => {
  it.each<[string, Schemas['Run']['status'], string, boolean]>([
    ['running', { startedAt: '2026-10-19T11:55:30Z' }, 'running', true],
    ['no status', undefined, 'running', true],
    [
      'ended without saying how',
      { endedAt: '2026-10-19T11:59:00Z' },
      'ended',
      false,
    ],
    ['completed', { end: 'completed' }, 'completed', false],
    ['lost', { end: 'lost', endedAt: '2026-10-19T11:59:00Z' }, 'lost', false],
  ])('%s', (_, status, state, running) => {
    expect(runStateOf(withStatus(status))).toBe(state);
    expect(isRunning(withStatus(status))).toBe(running);
  });
});

describe('turnOf', () => {
  it.each<[ProgressEvent['data'], number | undefined]>([
    [{ turn: 3 }, 3],
    [{ turn: '3' }, undefined],
    [{ turn: 1.5 }, undefined],
    [undefined, undefined],
  ])('%o → %s', (data, want) => {
    const event: ProgressEvent = {
      at: '2026-10-19T11:55:40Z',
      kind: 'tool',
      data,
    };
    expect(turnOf(event)).toBe(want);
  });
});

describe('mergeProgress', () => {
  const a: ProgressEvent = { at: '2026-10-19T11:55:40Z', kind: 'phase' };
  const b: ProgressEvent = {
    at: '2026-10-19T11:56:00Z',
    kind: 'tool',
    message: 'read',
  };
  const c: ProgressEvent = {
    at: '2026-10-19T11:56:00Z',
    kind: 'tool',
    message: 'edit',
  };

  it('keeps each event once, oldest first, in arrival order at the same time', () => {
    expect(mergeProgress([b, a], [a, c, b], [{ ...c }])).toEqual([a, b, c]);
  });

  it('tells events apart by every field', () => {
    const withData: ProgressEvent = { ...b, data: { turn: 2 } };
    expect(mergeProgress([b], [withData])).toEqual([b, withData]);
  });

  it('puts events with an unreadable time last', () => {
    const bad: ProgressEvent = { at: 'later', kind: 'phase' };
    expect(mergeProgress([bad, b, a])).toEqual([a, b, bad]);
  });
});

describe('formatCount and formatCost', () => {
  it('formats counts with separators and costs in USD', () => {
    expect(formatCount(120400)).toBe('120,400');
    expect(formatCount(undefined)).toBe('');
    expect(formatCost(1.5)).toBe('$1.50');
    expect(formatCost(0)).toBe('$0.00');
    expect(formatCost(null)).toBe('not reported');
  });
});
