import { describe, expect, it } from 'vitest';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import { workItemDetails } from '@/mocks/workitems';
import { llmTasksOf } from './agentTasks';

const pipeline = workItemDetails.find(
  (d) => d.workItem.metadata.name === 'wi_01JAC0PIPELINE0000000000000',
)!;
const example = examples.get('getWorkItem') as Schemas['WorkItemDetail'];

describe('llmTasksOf', () => {
  it('lists every task in order, with its runs, state and what it produced', () => {
    const tasks = llmTasksOf(pipeline);
    expect(
      tasks.map((t) => [
        t.task.spec.taskSpec.name,
        t.inFlight,
        t.runs.map((r) => r.spec.attempt),
        t.outcome,
        t.inputs.map(([k, v]) => `${k}=${v}`),
      ]),
    ).toEqual([
      ['triage', false, [1], 'promote', ['issue=compliance-framework/docs#77']],
      [
        'implement',
        false,
        [1, 2],
        'pr-opened',
        ['mode=new', 'target=pr', 'issue=compliance-framework/docs#77'],
      ],
      ['docs-review', true, [], undefined, ['mode=review', 'target=pr']],
    ]);
    expect(tasks[1]!.produced).toEqual([
      { label: 'compliance-framework/docs#78' },
      {
        label: 'https://github.com/compliance-framework/docs/pull/78',
        url: 'https://github.com/compliance-framework/docs/pull/78',
      },
    ]);
  });

  it.each<[string, Schemas['Task']['spec']['harness'], boolean, string]>([
    [
      'the resolved harness',
      {
        resolved: {
          name: 'codex',
          version: '1',
          auth: 'subscription',
          model: 'gpt-5-codex',
          reasoningEffort: 'high',
        },
      },
      true,
      'codex (gpt-5-codex, effort high)',
    ],
    ["the run's harness", { resolved: null }, true, 'codex (gpt-5-codex)'],
    [
      'the request',
      {
        resolved: null,
        request: { name: 'claude-code', config: { model: 'claude-*' } },
      },
      false,
      'claude-code (claude-*)',
    ],
    ['nothing', undefined, false, 'the agent picks'],
  ])('names %s', (_, harness, withRuns, want) => {
    const task = {
      ...example.tasks[0]!,
      spec: { ...example.tasks[0]!.spec, harness },
    };
    const detail = {
      ...example,
      tasks: [task],
      runs: withRuns ? example.runs : [],
    };
    expect(llmTasksOf(detail)[0]!.harness).toBe(want);
  });

  it('links only https URLs', () => {
    const run = example.runs[0]!;
    const detail: Schemas['WorkItemDetail'] = {
      ...example,
      runs: [
        {
          ...run,
          status: {
            ...run.status!,
            result: {
              ...run.status!.result!,
              artifacts: [{ kind: 'x', url: 'javascript:alert(1)' }],
            },
          },
        },
      ],
      effects: [],
    };
    expect(llmTasksOf(detail)[0]!.produced).toEqual([]);
  });
});
