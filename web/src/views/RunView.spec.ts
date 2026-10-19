import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { http, HttpResponse } from 'msw';
import type { Schemas } from '@/api/client';
import { fakeEventSources } from '@/__tests__/eventSource';
import { confirmDialog, recordCommands } from '@/__tests__/commands';
import { mountWithApp } from '@/__tests__/mount';
import { maintainer, reader } from '@/mocks/session';
import { eventSourceKey } from '@/composables/useStream';
import { examples } from '@/mocks/examples';
import { server } from '@/mocks/node';
import { streamFrames } from '@/mocks/stream';
import { liveRun } from '@/mocks/workitems';
import RunView from './RunView.vue';

const EXAMPLE = 'run_01JAB3M3P4Q5R6S7T8V9W0X1YZ';
const LIVE = liveRun.metadata.name;
const exampleTask = examples.get('getTask') as Schemas['Task'];

let mounted: VueWrapper | undefined;

async function mountRun(id: string) {
  const es = fakeEventSources();
  const { wrapper } = await mountWithApp(RunView, {
    path: `/runs/${id}`,
    props: { id },
    global: { provide: { [eventSourceKey]: es.factory } },
  });
  mounted = wrapper;
  return { wrapper, es };
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
  vi.useRealTimers();
});

const messages = (wrapper: VueWrapper) =>
  wrapper
    .findAll('[data-testid="progress-event"]')
    .map((e) => e.findAll('span')[1]!.text());

describe('RunView', () => {
  it('shows a finished run: progress, Result, harness, usage, effects, agent and owner', async () => {
    const { wrapper, es } = await mountRun(EXAMPLE);
    expect(wrapper.get('h1').text()).toBe('Run');
    expect(wrapper.text()).toContain(`${EXAMPLE} · attempt 1 of task`);
    expect(messages(wrapper)).toEqual([
      'cloning compliance-framework/docs',
      'read docs/oscal/import.md',
    ]);
    expect(wrapper.get('[data-testid="run-agent"]').text()).toBe(
      'agt_gusfcarvalho-mbp-01',
    );
    expect(wrapper.get('[data-testid="run-owner"]').text()).toBe(
      'github-public:gusfcarvalho',
    );
    expect(wrapper.get('[data-testid="harness"]').text()).toContain(
      'gpt-5-codex',
    );
    expect(wrapper.get('[data-testid="usage"]').text()).toContain('120,400');
    expect(wrapper.get('[data-testid="effect"] a').attributes('href')).toBe(
      'https://github.com/compliance-framework/docs/issues/42#issuecomment-2401',
    );
    expect(
      wrapper.get('[role="region"][aria-label="Result JSON"]').text(),
    ).toContain('"outcome": "triaged"');
    expect(wrapper.get('nav a').attributes('href')).toBe('/');

    // Opened while the run loaded, closed once it turned out to be finished.
    expect(es.sources).toHaveLength(1);
    expect(es.last!.topics).toEqual([`runs/${EXAMPLE}`, 'tasks']);
    expect(es.last!.closed).toBe(true);
    expect(wrapper.find('[data-testid="stream-status"]').exists()).toBe(false);
  });

  it('loads the transcript lazily and shows its turns', async () => {
    const { wrapper } = await mountRun(EXAMPLE);
    expect(wrapper.find('[data-testid="transcript"]').exists()).toBe(false);
    const load = wrapper
      .findAll('button')
      .find((b) => b.text() === 'Load transcript')!;
    await load.trigger('click');
    await flushPromises();
    await vi.waitFor(() =>
      expect(wrapper.findAll('[data-testid="transcript-turn"]')).toHaveLength(
        4,
      ),
    );
  });

  it('streams the progress of a running run (mocked SSE)', async () => {
    const { wrapper, es } = await mountRun(LIVE);
    const source = es.last!;
    expect(source.topics).toEqual([`runs/${LIVE}`, 'tasks']);
    expect(source.closed).toBe(false);
    expect(wrapper.get('[data-testid="run-state"]').text()).toBe('running');
    expect(wrapper.get('[data-testid="stream-status"]').text()).toBe(
      'connecting',
    );
    expect(wrapper.text()).toContain('No transcript uploaded yet.');

    source.open();
    for (const frame of streamFrames.filter((f) => f.event === 'progress')) {
      source.emit(frame.event, frame.data, frame.id);
    }
    await flushPromises();
    expect(wrapper.get('[data-testid="stream-status"]').text()).toBe('live');
    // The other run's progress (id 419) is not this run's.
    expect(messages(wrapper)).toEqual([
      'cloning platform/release-notes',
      'read repos.yaml',
      'read CHANGELOG.md',
      'edit docs/release/v1.9.0.md',
      'drafting the release notes',
    ]);
  });

  it('backfills from the run after re-creating a stream the browser gave up on', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { wrapper, es } = await mountRun(LIVE);
    const first = es.last!;
    first.open();
    first.emit(
      'progress',
      streamFrames.find((f) => f.id === '500')!.data,
      '500',
    );
    await flushPromises();

    // While disconnected the run moved on: its ring buffer has what the page missed.
    const missed: Schemas['ProgressEvent'] = {
      at: '2026-10-19T11:59:30Z',
      kind: 'tool',
      message: 'missed while offline',
    };
    let reloads = 0;
    server.use(
      http.get('/api/v1/runs/:id', () => {
        reloads++;
        return HttpResponse.json<Schemas['RunDetail']>({
          run: {
            ...liveRun,
            status: {
              ...liveRun.status,
              progress: [...liveRun.status!.progress!, missed],
            },
          },
          effects: [],
        });
      }),
    );

    first.fail('give-up');
    await flushPromises();
    expect(wrapper.get('[data-testid="stream-status"]').text()).toBe(
      'reconnecting',
    );
    await vi.advanceTimersByTimeAsync(1000);
    expect(es.sources).toHaveLength(2);
    es.last!.open();
    await flushPromises();
    expect(reloads).toBe(1);
    expect(messages(wrapper)).toEqual([
      'cloning platform/release-notes',
      'read repos.yaml',
      'read CHANGELOG.md',
      'missed while offline',
    ]);
  });

  it('reloads the run when its task changes, and stops streaming once it ended', async () => {
    const { wrapper, es } = await mountRun(LIVE);
    const source = es.last!;
    source.open();

    let reloads = 0;
    server.use(
      http.get('/api/v1/runs/:id', () => {
        reloads++;
        return HttpResponse.json<Schemas['RunDetail']>({
          run: {
            ...liveRun,
            status: {
              ...liveRun.status,
              end: 'completed',
              endedAt: '2026-10-19T12:01:00Z',
            },
          },
          effects: [],
        });
      }),
    );
    // Another task's update changes nothing.
    source.emit('task.updated', exampleTask, '600');
    await flushPromises();
    expect(reloads).toBe(0);

    source.emit(
      'task.updated',
      {
        ...exampleTask,
        metadata: { ...exampleTask.metadata, id: liveRun.spec.task },
        status: { phase: 'Succeeded' },
      },
      '601',
    );
    await flushPromises();
    expect(reloads).toBe(1);
    expect(wrapper.get('[data-testid="run-state"]').text()).toBe('completed');
    expect(source.closed).toBe(true);
    expect(wrapper.find('[data-testid="stream-status"]').exists()).toBe(false);
  });

  it('says when the run does not exist, and does not stream', async () => {
    const { wrapper, es } = await mountRun('run_missing');
    expect(wrapper.get('h1').text()).toBe('Run not found');
    expect(wrapper.text()).toContain('run_missing');
    expect(es.last!.closed).toBe(true);
  });

  it('reports a failure to load', async () => {
    server.use(
      http.get('/api/v1/runs/:id', () =>
        HttpResponse.json<Schemas['Problem']>(
          { type: 'about:blank', title: 'Unavailable', status: 503 },
          {
            status: 503,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        ),
      ),
    );
    const { wrapper } = await mountRun(LIVE);
    expect(wrapper.get('[role="alert"]').text()).toContain(
      "Couldn't load the run: Unavailable",
    );
  });

  it('loads the new run when the id changes', async () => {
    const { wrapper, es } = await mountRun(EXAMPLE);
    await wrapper.setProps({ id: LIVE });
    await flushPromises();
    expect(wrapper.get('[data-testid="run-state"]').text()).toBe('running');
    expect(es.last!.topics).toEqual([`runs/${LIVE}`, 'tasks']);
    expect(es.last!.closed).toBe(false);
  });
});

describe('RunView controls', () => {
  async function mountAs(id: string, me: Schemas['Me']) {
    const es = fakeEventSources();
    const { wrapper } = await mountWithApp(RunView, {
      path: `/runs/${id}`,
      props: { id },
      me,
      attachTo: document.body,
      global: { provide: { [eventSourceKey]: es.factory } },
    });
    mounted = wrapper;
    return wrapper;
  }

  it('cancels the live run once a maintainer confirms', async () => {
    const bodies = recordCommands();
    const wrapper = await mountAs(LIVE, maintainer);
    const controls = wrapper.get('[data-testid="run-controls"]');
    expect(controls.findAll('button').map((b) => b.text())).toEqual([
      'Cancel run',
    ]);
    await controls.get('button').trigger('click');
    await flushPromises();
    await confirmDialog('Cancel run');
    expect(bodies).toEqual([
      { type: 'cancel', target: { task: liveRun.spec.task } },
    ]);
  });

  it('re-dispatches the finished run once a maintainer confirms', async () => {
    const bodies = recordCommands();
    const wrapper = await mountAs(EXAMPLE, maintainer);
    const controls = wrapper.get('[data-testid="run-controls"]');
    expect(controls.findAll('button').map((b) => b.text())).toEqual([
      'Re-dispatch',
    ]);
    await controls.get('button').trigger('click');
    await flushPromises();
    await confirmDialog('Re-dispatch');
    expect(bodies).toEqual([
      { type: 'redispatch', target: { task: exampleTask.metadata.id } },
    ]);
  });

  it('shows a reader no controls', async () => {
    const wrapper = await mountAs(LIVE, reader);
    expect(wrapper.find('[data-testid="run-controls"]').exists()).toBe(false);
  });
});
