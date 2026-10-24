import { describe, expect, it } from 'vitest';
import { mount, RouterLinkStub } from '@vue/test-utils';
import { workItemDetails } from '@/mocks/workitems';
import LLMTasks from './LLMTasks.vue';

const pipeline = workItemDetails.find(
  (d) => d.workItem.metadata.name === 'wi_01JAC0PIPELINE0000000000000',
)!;

describe('LLMTasks', () => {
  it('lists the tasks in order and highlights the one in flight', () => {
    const wrapper = mount(LLMTasks, {
      props: { detail: pipeline },
      global: { stubs: { RouterLink: RouterLinkStub } },
    });
    const tasks = wrapper.findAll('[data-testid="llm-task"]');
    expect(
      tasks.map((t) => [t.get('h3').text(), t.attributes('data-in-flight')]),
    ).toEqual([
      ['triage', 'false'],
      ['implement', 'false'],
      ['docs-review', 'true'],
    ]);
    expect(tasks[2]!.text()).toContain('in flight');
    expect(tasks[2]!.get('[data-testid="llm-task-reason"]').text()).toBe(
      'Waiting: anti-affinity: waiting for an agent other than the author',
    );
    const implement = tasks[1]!;
    expect(implement.get('[data-testid="llm-task-attempts"]').text()).toBe('2');
    expect(implement.get('[data-testid="llm-task-harness"]').text()).toBe(
      'codex (gpt-5-codex, effort medium)',
    );
    expect(implement.get('[data-testid="llm-task-summary"]').text()).toBe(
      'Opened compliance-framework/docs#78 documenting --profile.',
    );
    const links = wrapper
      .findAllComponents(RouterLinkStub)
      .filter((l) => implement.element.contains(l.element));
    expect(links.map((l) => [l.text(), l.props('to')])).toEqual([
      [
        'attempt 1',
        { name: 'run', params: { id: 'run_01JAC0PIPELINEIMPLEMENT01' } },
      ],
      [
        'transcript',
        {
          name: 'run',
          params: { id: 'run_01JAC0PIPELINEIMPLEMENT01' },
          hash: '#transcript-heading',
        },
      ],
      [
        'attempt 2',
        { name: 'run', params: { id: 'run_01JAC0PIPELINEIMPLEMENT02' } },
      ],
      [
        'transcript',
        {
          name: 'run',
          params: { id: 'run_01JAC0PIPELINEIMPLEMENT02' },
          hash: '#transcript-heading',
        },
      ],
    ]);
    expect(
      implement.get('[data-testid="llm-task-produced"] a').attributes(),
    ).toMatchObject({
      href: 'https://github.com/compliance-framework/docs/pull/78',
      rel: 'noopener noreferrer',
    });
  });

  it('says when there are none', () => {
    const wrapper = mount(LLMTasks, {
      props: { detail: { ...pipeline, tasks: [] } },
    });
    expect(wrapper.text()).toContain('No agent tasks yet.');
  });
});
