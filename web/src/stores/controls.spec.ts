import { beforeEach, describe, expect, it } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { http, HttpResponse } from 'msw';
import { recordCommands } from '@/__tests__/commands';
import { pauseCommand } from '@/components/controls/commands';
import { server } from '@/mocks/node';
import { useControlsStore } from './controls';

describe('controls store', () => {
  beforeEach(() => setActivePinia(createPinia()));

  it('posts the command and remembers an accepted pause', async () => {
    const bodies = recordCommands();
    const store = useControlsStore();
    const outcome = await store.send(pauseCommand('pause', 'queue', 'triage'));
    expect(outcome).toEqual({ ok: true, accepted: { type: 'pause' } });
    expect(bodies).toEqual([
      { type: 'pause', target: { scope: 'queue', name: 'triage' } },
    ]);
    expect(store.pauses).toEqual({ 'queue:triage': 'paused' });

    await store.send(pauseCommand('resume', 'queue', 'triage'));
    await store.send(pauseCommand('pause', 'global'));
    expect(store.pauses).toEqual({
      'queue:triage': 'resumed',
      global: 'paused',
    });
  });

  it("doesn't remember other commands", async () => {
    const store = useControlsStore();
    const outcome = await store.send({
      type: 'cancel',
      target: { task: 'tsk_1' },
    });
    expect(outcome.ok).toBe(true);
    expect(store.pauses).toEqual({});
  });

  it('reports a refusal, with the field errors of a 400', async () => {
    const store = useControlsStore();
    const outcome = await store.send({
      type: 'pause',
      target: { scope: 'queue' },
    });
    expect(outcome).toEqual({
      ok: false,
      status: 400,
      message:
        'Bad Request: target.name: required for scope queue (target.name: required for scope queue)',
    });
    expect(store.pauses).toEqual({});
  });

  it('reports a network failure', async () => {
    server.use(http.post('/api/v1/commands', () => HttpResponse.error()));
    const outcome = await useControlsStore().send({ type: 'resync-catalog' });
    expect(outcome.ok).toBe(false);
  });
});
