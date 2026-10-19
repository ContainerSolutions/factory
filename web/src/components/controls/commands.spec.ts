import { describe, expect, it } from 'vitest';
import {
  cancelCommand,
  describeCommandFailure,
  describeScope,
  pauseCommand,
  pauseKey,
  redispatchCommand,
  resyncCatalogCommand,
} from './commands';

function response(status: number): Response {
  return new Response(null, { status });
}

describe('command builders', () => {
  it.each([
    [
      pauseCommand('pause', 'global'),
      { type: 'pause', target: { scope: 'global' } },
    ],
    [
      pauseCommand('pause', 'global', 'ignored'),
      { type: 'pause', target: { scope: 'global' } },
    ],
    [
      pauseCommand('resume', 'provider', 'github-public'),
      { type: 'resume', target: { scope: 'provider', name: 'github-public' } },
    ],
    [
      pauseCommand('pause', 'queue', 'triage'),
      { type: 'pause', target: { scope: 'queue', name: 'triage' } },
    ],
    [
      pauseCommand('pause', 'workitem', 'wi_1'),
      { type: 'pause', target: { scope: 'workitem', name: 'wi_1' } },
    ],
    [cancelCommand('tsk_1'), { type: 'cancel', target: { task: 'tsk_1' } }],
    [
      redispatchCommand('tsk_1'),
      { type: 'redispatch', target: { task: 'tsk_1' } },
    ],
    [
      redispatchCommand('tsk_1', { name: '  ', model: 'x' }),
      { type: 'redispatch', target: { task: 'tsk_1' } },
    ],
    [
      redispatchCommand('tsk_1', { name: ' codex ' }),
      {
        type: 'redispatch',
        target: { task: 'tsk_1' },
        args: { harness: { name: 'codex' } },
      },
    ],
    [
      redispatchCommand('tsk_1', { name: 'claude-code', model: 'claude-x' }),
      {
        type: 'redispatch',
        target: { task: 'tsk_1' },
        args: {
          harness: { name: 'claude-code', config: { model: 'claude-x' } },
        },
      },
    ],
    [resyncCatalogCommand(), { type: 'resync-catalog' }],
  ])('builds %j', (got, want) => {
    expect(got).toEqual(want);
  });

  it('keys and describes pause scopes', () => {
    expect(pauseKey('global', 'x')).toBe('global');
    expect(pauseKey('queue', 'triage')).toBe('queue:triage');
    expect(describeScope('global')).toBe('the whole factory');
    expect(describeScope('workitem', 'wi_1')).toBe('work item wi_1');
    expect(describeScope('provider', 'gh')).toBe('provider gh');
  });
});

describe('describeCommandFailure', () => {
  it('says who may send commands on a 403', () => {
    expect(
      describeCommandFailure(
        { title: 'Forbidden', status: 403, detail: 'not a maintainer' },
        response(403),
      ),
    ).toBe(
      'Only members of the maintainers Group can send commands. The server said: Forbidden: not a maintainer',
    );
  });

  it('asks to sign in again on a 401', () => {
    expect(describeCommandFailure(undefined, response(401))).toBe(
      'Your session has expired: sign in again.',
    );
  });

  it('lists field errors', () => {
    expect(
      describeCommandFailure(
        {
          title: 'Bad Request',
          status: 400,
          errors: [{ field: 'target.name', message: 'required' }],
        },
        response(400),
      ),
    ).toBe('Bad Request (target.name: required)');
  });

  it('falls back to the status', () => {
    expect(describeCommandFailure(null, response(409))).toBe('HTTP 409');
  });
});
