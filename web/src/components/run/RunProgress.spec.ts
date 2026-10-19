import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import type { StreamStatus } from '@/composables/useStream';
import { liveRun } from '@/mocks/workitems';
import RunProgress from './RunProgress.vue';

const events = liveRun.status!.progress!;

function mountProgress(props: {
  events: Schemas['ProgressEvent'][];
  running: boolean;
  stream?: StreamStatus;
}) {
  return mount(RunProgress, { props });
}

describe('RunProgress', () => {
  it('lists the events with their time, kind, message and turn', () => {
    const wrapper = mountProgress({ events, running: true, stream: 'open' });
    const items = wrapper.findAll('[data-testid="progress-event"]');
    expect(
      items.map((i) => [...i.element.children].map((c) => c.textContent)),
    ).toEqual([
      ['2026-10-19 11:55:40 UTC', 'phase', 'cloning platform/release-notes'],
      ['2026-10-19 11:56:10 UTC', 'tool', 'read repos.yaml', 'turn 2'],
    ]);
    expect(items[0]!.get('time').attributes('datetime')).toBe(
      '2026-10-19T11:55:40Z',
    );
    // Announced as they arrive.
    expect(wrapper.get('ol').attributes('aria-live')).toBe('polite');
  });

  it.each<[StreamStatus, string | undefined]>([
    ['open', 'live'],
    ['connecting', 'connecting'],
    ['reconnecting', 'reconnecting'],
    ['unsupported', undefined],
    ['closed', undefined],
  ])('shows a %s stream as %s while the run runs', (stream, label) => {
    const pill = mountProgress({ events, running: true, stream }).find(
      '[data-testid="stream-status"]',
    );
    expect(pill.exists() ? pill.text() : undefined).toBe(label);
  });

  it('shows no stream status for a finished run', () => {
    const wrapper = mountProgress({ events, running: false, stream: 'open' });
    expect(wrapper.find('[data-testid="stream-status"]').exists()).toBe(false);
  });

  it('renders a message as text', () => {
    const wrapper = mountProgress({
      events: [{ at: events[0]!.at, kind: 'tool', message: '<img src=x>' }],
      running: true,
    });
    expect(wrapper.find('img').exists()).toBe(false);
    expect(wrapper.text()).toContain('<img src=x>');
  });

  it('says when there is no progress', () => {
    const wrapper = mountProgress({ events: [], running: true });
    expect(wrapper.find('ol').exists()).toBe(false);
    expect(wrapper.text()).toContain('No progress yet.');
  });
});
