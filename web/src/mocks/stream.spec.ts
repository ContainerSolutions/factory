import { describe, expect, it } from 'vitest';
import { framesFor, streamFrames, topicsOf } from './stream';
import { liveRun } from './workitems';

const LIVE = `runs/${liveRun.metadata.name}`;

describe('mock stream', () => {
  it('sends each frame on the topics the contract names', () => {
    expect(streamFrames.map((f) => [f.event, f.id, topicsOf(f)])).toEqual([
      ['task.updated', '418', ['tasks']],
      ['progress', '419', ['runs/run_01JAB3M3P4Q5R6S7T8V9W0X1YZ']],
      [
        'workitem.updated',
        '420',
        ['workitems', 'workitems/wi_01JAB3J9T1D2E3F4G5H6J7K8LM'],
      ],
      ['agent.updated', '421', ['agents']],
      ['progress', '500', [LIVE]],
      ['progress', '501', [LIVE]],
      ['progress', '502', [LIVE]],
      ['agent.updated', '503', ['agents']],
    ]);
  });

  it('picks the frames of the requested topics', () => {
    expect(framesFor([LIVE, 'tasks']).map((f) => f.id)).toEqual([
      '418',
      '500',
      '501',
      '502',
    ]);
    expect(framesFor(['agents', 'tasks']).map((f) => f.id)).toEqual([
      '418',
      '421',
      '503',
    ]);
    expect(framesFor(['workitems/wi_other'])).toEqual([]);
  });

  it('resumes after Last-Event-ID, as a reconnect is backfilled', () => {
    expect(framesFor([LIVE], '500').map((f) => f.id)).toEqual(['501', '502']);
    expect(framesFor([LIVE], '502')).toEqual([]);
    expect(framesFor([LIVE], null).map((f) => f.id)).toHaveLength(3);
  });
});
