import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import { liveRun } from '@/mocks/workitems';
import RunOverview from './RunOverview.vue';

const example = (examples.get('getRun') as Schemas['RunDetail']).run;

function rows(wrapper: ReturnType<typeof mount>) {
  const dts = wrapper.findAll('dt').map((d) => d.text());
  const dds = wrapper.findAll('dd').map((d) => d.text());
  return Object.fromEntries(dts.map((t, i) => [t, dds[i]]));
}

describe('RunOverview', () => {
  it('shows the agent, owner, task, attempt and times of a finished run with its Result', () => {
    const wrapper = mount(RunOverview, { props: { run: example } });
    expect(rows(wrapper)).toEqual({
      State: 'ended',
      Agent: 'agt_gusfcarvalho-mbp-01',
      Owner: 'github-public:gusfcarvalho',
      Task: 'tsk_01JAB3K7Q2M8V9W4XYZ5N6P7RS',
      Attempt: '1',
      Started: '2026-10-16 09:30:31 UTC',
      Ended: '2026-10-16 09:33:43 UTC',
    });
    expect(wrapper.text()).toContain('done');
    expect(wrapper.text()).toContain('outcome: triaged');
    expect(wrapper.get('[data-testid="result-summary"]').text()).toBe(
      example.status!.result!.summary,
    );
    expect(wrapper.find('[data-testid="result-question"]').exists()).toBe(
      false,
    );
  });

  it('shows a running run without a Result', () => {
    const wrapper = mount(RunOverview, { props: { run: liveRun } });
    expect(rows(wrapper)).toMatchObject({ State: 'running' });
    expect(rows(wrapper)).not.toHaveProperty('Ended');
    expect(wrapper.text()).not.toContain('Result');
  });

  it('shows the question of a run that needs input, as text', () => {
    const run: Schemas['Run'] = {
      ...example,
      spec: { ...example.spec, owner: undefined },
      status: {
        ...example.status,
        end: 'completed',
        result: {
          ...example.status!.result!,
          status: 'needs_input',
          question: 'Is <b>--profile</b> public?',
        },
      },
    };
    const wrapper = mount(RunOverview, { props: { run } });
    expect(rows(wrapper)).toMatchObject({
      State: 'completed',
      Owner: 'not recorded',
    });
    expect(wrapper.get('[data-testid="result-question"]').text()).toBe(
      'Question: Is <b>--profile</b> public?',
    );
    expect(wrapper.find('b').exists()).toBe(false);
  });
});
