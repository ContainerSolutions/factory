import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { NOW } from '@/__tests__/mount';
import { queueViews } from '@/mocks/pools';
import QueueCard from './QueueCard.vue';

const view = (name: string) =>
  queueViews.find((q) => q.queue.metadata.name === name)!;

function rows(wrapper: ReturnType<typeof mount>) {
  const dts = wrapper.findAll('dt').map((d) => d.text());
  const dds = wrapper.findAll('dd').map((d) => d.text());
  return Object.fromEntries(dts.map((t, i) => [t, dds[i]]));
}

describe('QueueCard', () => {
  it('shows depth, running against concurrency, and each pending task with why it waits', () => {
    const wrapper = mount(QueueCard, {
      props: { view: view('default'), now: NOW },
    });
    expect(wrapper.get('h3').text()).toBe('default');
    expect(wrapper.get('[data-testid="queue-depth"]').text()).toBe('3 pending');
    expect(rows(wrapper)).toEqual({
      Pool: 'maintainer-agents',
      Running: '1 of 2',
      Priority: '50',
      Budget: '5,000,000 tokens, $20.00 a day',
    });
    const pending = wrapper.findAll('[data-testid="pending-task"]');
    expect(
      pending.map((p) => [
        p.get('code').text(),
        p.get('[data-testid="pending-reason"]').text(),
        p.get('time').text(),
      ]),
    ).toEqual([
      ['tsk_01JAB3Q8R9S0T1V2W3X4Y5Z6AB', 'agent-throttled', 'waiting 20 min'],
      [
        'tsk_01JAC0IMPLGPU0000000000000',
        'no-capable-agent',
        'waiting 1 h 55 min',
      ],
      ['tsk_01JAC0IMPLLENDING00000000', 'lending', 'waiting 2 min'],
    ]);
    expect(pending[1]!.get('[data-testid="pending-why"]').text()).toContain(
      'No agent in the pool offers',
    );
    expect(pending[0]!.get('time').attributes('title')).toBe(
      '2026-10-19 11:40:00 UTC',
    );
  });

  it("shows the queue's budget and retry policy", () => {
    const wrapper = mount(QueueCard, {
      props: { view: view('triage'), now: NOW },
    });
    expect(rows(wrapper)).toEqual({
      Pool: 'maintainer-agents',
      Running: '0 of 4',
      Priority: '60',
      Budget: '80 runs a day',
      Retry: '1 time on agent-lost, infra-failure',
    });
    expect(wrapper.get('[data-testid="pending-reason"]').text()).toBe('budget');
  });

  it('says when nothing is pending', () => {
    const wrapper = mount(QueueCard, {
      props: { view: view('nightly'), now: NOW },
    });
    expect(wrapper.get('h3').text()).toBe('nightly');
    expect(wrapper.get('[data-testid="queue-depth"]').text()).toBe('0 pending');
    expect(wrapper.text()).toContain('Nothing pending.');
  });

  it("shows a reason it doesn't know as it is, and a task with no reason yet", () => {
    const odd: Schemas['QueueView'] = {
      queue: { kind: 'Queue', metadata: { name: 'q' }, spec: { pool: 'p' } },
      depth: 2,
      running: 0,
      pending: [
        { task: 'tsk_a', reason: '<b>new-check</b>' },
        { task: 'tsk_b' },
      ],
    };
    const wrapper = mount(QueueCard, { props: { view: odd, now: NOW } });
    expect(rows(wrapper)).toEqual({ Pool: 'p', Running: '0' });
    const reasons = wrapper.findAll('[data-testid="pending-reason"]');
    expect(reasons.map((r) => r.text())).toEqual([
      '<b>new-check</b>',
      'no reason yet',
    ]);
    expect(wrapper.find('b').exists()).toBe(false);
    expect(wrapper.find('[data-testid="pending-why"]').exists()).toBe(false);
    expect(wrapper.find('time').exists()).toBe(false);
  });
});
