import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import { liveRun } from '@/mocks/workitems';
import RunHarness from './RunHarness.vue';

const example = (examples.get('getRun') as Schemas['RunDetail']).run;

function rows(wrapper: ReturnType<typeof mount>, testid: string) {
  const dl = wrapper.get(`[data-testid="${testid}"]`);
  const dts = dl.findAll('dt').map((d) => d.text());
  const dds = dl.findAll('dd').map((d) => d.text());
  return Object.fromEntries(dts.map((t, i) => [t, dds[i]]));
}

describe('RunHarness', () => {
  it('shows the effective config and usage of the example', () => {
    const wrapper = mount(RunHarness, { props: { run: example } });
    expect(rows(wrapper, 'harness')).toEqual({
      Name: 'codex 0.46.0',
      Auth: 'subscription',
      Model: 'gpt-5-codex',
      'Reasoning effort': 'medium',
    });
    expect(rows(wrapper, 'usage')).toEqual({
      'Input tokens': '120,400',
      'Output tokens': '6,100',
      Turns: '17',
      'Wall clock': '3m12s',
      Cost: 'not reported',
    });
  });

  it('shows the usage of a running run, and says the config comes with the Result', () => {
    const wrapper = mount(RunHarness, { props: { run: liveRun } });
    expect(wrapper.get('[data-testid="harness"]').text()).toBe(
      'The effective config is reported with the Result.',
    );
    expect(rows(wrapper, 'usage')).toEqual({
      'Input tokens': '18,200',
      'Output tokens': '900',
      Turns: '4',
      Cost: 'not reported',
    });
  });

  it("falls back to the Result's usage, and shows a cost", () => {
    const harness = example.status!.result!.harness!;
    const run: Schemas['Run'] = {
      ...example,
      status: {
        ...example.status,
        usage: undefined,
        result: {
          ...example.status!.result!,
          harness: {
            name: 'claude-code',
            effectiveConfig: { model: 'claude-opus-5-5' },
            usage: { ...harness.usage, costUSD: 0.42 },
          },
        },
      },
    };
    const wrapper = mount(RunHarness, { props: { run } });
    expect(rows(wrapper, 'harness')).toEqual({
      Name: 'claude-code',
      Model: 'claude-opus-5-5',
    });
    expect(rows(wrapper, 'usage')).toMatchObject({ Cost: '$0.42' });
  });

  it('says when no usage was reported', () => {
    const wrapper = mount(RunHarness, {
      props: { run: { ...liveRun, status: {} } },
    });
    expect(wrapper.get('[data-testid="usage"]').text()).toBe(
      'No usage reported yet.',
    );
  });
});
