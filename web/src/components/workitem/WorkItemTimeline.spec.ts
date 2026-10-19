import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import PrimeVue from 'primevue/config';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import { workItemDetails } from '@/mocks/workitems';
import WorkItemTimeline from './WorkItemTimeline.vue';

const example = examples.get('getWorkItem') as Schemas['WorkItemDetail'];

function mountTimeline(detail: Schemas['WorkItemDetail']) {
  return mount(WorkItemTimeline, {
    props: { detail },
    global: { plugins: [[PrimeVue, { unstyled: true }]] },
  });
}

describe('WorkItemTimeline', () => {
  it('lists the example timeline in order, as an ordered list', () => {
    const wrapper = mountTimeline(example);
    const entries = wrapper.findAll('ol > li');
    expect(entries.map((e) => e.attributes('data-kind'))).toEqual([
      'event',
      'decision',
      'task',
      'run',
      'effect',
    ]);
    const [event, decision, task, run, effect] = entries.map((e) => e.text());
    expect(event).toContain('issue.opened on github-public');
    expect(event).toContain('OSCAL import page misses --profile');
    expect(event).toContain('by github-public:octocat');
    expect(decision).toContain('docs / triage-outside-issues');
    expect(decision).toContain('fired');
    expect(decision).toContain('actions: dispatch, set');
    expect(task).toContain('triage');
    expect(task).toContain('queue triage');
    expect(task).toContain('Running');
    expect(run).toContain('attempt 1 on agt_gusfcarvalho-mbp-01');
    expect(run).toContain('outcome: triaged');
    expect(effect).toContain('comment on compliance-framework/docs#42');
    expect(entries[4]!.get('a').attributes()).toMatchObject({
      href: 'https://github.com/compliance-framework/docs/issues/42#issuecomment-2401',
      rel: 'noopener noreferrer',
    });
  });

  it('asks for the "why" of a decision by its index', async () => {
    const detail = workItemDetails.find(
      (d) => d.workItem.metadata.name === 'wi_01JAC0ESCALATED0000000000000',
    )!;
    const wrapper = mountTimeline(detail);
    const buttons = wrapper.findAll('button');
    expect(buttons.map((b) => b.attributes('aria-label'))).toEqual([
      'Why: api / implement-review-feedback',
      'Why: api / escalate-needs-human',
    ]);
    expect(buttons[0]!.attributes('aria-haspopup')).toBe('dialog');
    await buttons[1]!.trigger('click');
    expect(wrapper.emitted('why')).toEqual([[1]]);
    // A rail that held the action back shows on the entry itself.
    const decisions = wrapper.findAll('li[data-kind="decision"]');
    expect(decisions[0]!.text()).toContain('run-cap: Suppressed');
    expect(decisions[1]!.text()).not.toContain('effects-allowed');
  });

  it('renders untrusted text as text and links only https URLs', () => {
    const detail: Schemas['WorkItemDetail'] = {
      ...example,
      events: [
        {
          ...example.events[0]!,
          data: {
            ...example.events[0]!.data,
            facts: { title: '<b>bold</b>' },
          },
        },
      ],
      effects: [
        {
          ...example.effects[0]!,
          status: { result: { url: 'javascript:alert(1)' } },
        },
      ],
    };
    const wrapper = mountTimeline(detail);
    expect(wrapper.find('b').exists()).toBe(false);
    expect(wrapper.text()).toContain('<b>bold</b>');
    expect(wrapper.find('a').exists()).toBe(false);
    expect(wrapper.text()).toContain('javascript:alert(1)');
  });

  it('shows a failed effect', () => {
    const wrapper = mountTimeline({
      ...example,
      effects: [
        { ...example.effects[0]!, status: { result: { error: 'HTTP 403' } } },
      ],
    });
    const effect = wrapper.get('li[data-kind="effect"]');
    expect(effect.text()).toContain('failed');
    expect(effect.text()).toContain('HTTP 403');
  });

  it('says when nothing has happened', () => {
    const wrapper = mountTimeline({
      ...example,
      workItem: { ...example.workItem, status: {} },
      events: [],
      tasks: [],
      runs: [],
      effects: [],
    });
    expect(wrapper.find('ol').exists()).toBe(false);
    expect(wrapper.text()).toContain('Nothing has happened yet.');
  });
});
