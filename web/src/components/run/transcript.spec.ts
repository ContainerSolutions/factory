import { describe, expect, it } from 'vitest';
import { examples } from '@/mocks/examples';
import { decodeTranscript, parseTranscript, turnsOf } from './transcript';

const example = examples.get('getTranscript') as string;

async function gzip(text: string): Promise<ArrayBuffer> {
  const stream = new Response(text).body!.pipeThrough(
    new CompressionStream('gzip'),
  );
  return new Response(stream).arrayBuffer();
}

describe('decodeTranscript', () => {
  it('decompresses a gzip body', async () => {
    expect(await decodeTranscript(await gzip(example))).toBe(example);
  });

  it('takes a body that is already decoded as it is', async () => {
    const bytes = new TextEncoder().encode(example).buffer as ArrayBuffer;
    expect(await decodeTranscript(bytes)).toBe(example);
  });

  it('decodes an empty body', async () => {
    expect(await decodeTranscript(new ArrayBuffer(0))).toBe('');
  });
});

describe('parseTranscript', () => {
  it('picks out the common fields of the example and keeps the rest', () => {
    const entries = parseTranscript(example);
    expect(entries).toEqual([
      {
        index: 0,
        type: 'session',
        turn: undefined,
        role: undefined,
        text: undefined,
        name: undefined,
        detail: {
          harness: 'codex',
          version: '0.46.0',
          model: 'gpt-5-codex',
          at: '2026-10-16T09:30:41Z',
        },
      },
      {
        index: 1,
        type: 'message',
        turn: 1,
        role: 'user',
        text: 'You are triaging compliance-framework/docs#42.',
        name: undefined,
        detail: undefined,
      },
      {
        index: 2,
        type: 'tool',
        turn: 3,
        role: undefined,
        text: undefined,
        name: 'read',
        detail: { input: { path: 'docs/oscal/import.md' } },
      },
      {
        index: 3,
        type: 'message',
        turn: 17,
        role: 'assistant',
        text: 'Triaged: missing --profile flag; wrote out/result.json.',
        name: undefined,
        detail: undefined,
      },
    ]);
  });

  it('keeps a line that is not a JSON object as text and skips blank lines', () => {
    const entries = parseTranscript('not json\r\n\n  \n42\n{"turn":"x"}\n');
    expect(entries.map((e) => [e.index, e.type, e.text, e.turn])).toEqual([
      [0, 'text', 'not json', undefined],
      [3, 'text', '42', undefined],
      [4, 'text', undefined, undefined],
    ]);
  });
});

describe('turnsOf', () => {
  it('groups the example by turn, the session first', () => {
    const turns = turnsOf(parseTranscript(example));
    expect(turns.map((t) => [t.key, t.label, t.entries.length])).toEqual([
      ['session', 'Session', 1],
      ['turn/1', 'Turn 1', 1],
      ['turn/3', 'Turn 3', 1],
      ['turn/17', 'Turn 17', 1],
    ]);
  });

  it('puts an entry without a turn in the turn before it, and a turn seen again in its group', () => {
    const turns = turnsOf(
      parseTranscript(
        [
          '{"type":"message","turn":1,"text":"a"}',
          '{"type":"factory.repair"}',
          '{"type":"message","turn":2,"text":"b"}',
          '{"type":"tool","turn":1,"name":"late"}',
        ].join('\n'),
      ),
    );
    expect(turns.map((t) => [t.label, t.entries.map((e) => e.index)])).toEqual([
      ['Turn 1', [0, 1, 3]],
      ['Turn 2', [2]],
    ]);
  });

  it('has no turns for no entries', () => {
    expect(turnsOf([])).toEqual([]);
  });
});
