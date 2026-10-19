import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import { taskSpecs } from '@/mocks/catalog';
import TaskSpecTable from './TaskSpecTable.vue';

describe('TaskSpecTable', () => {
  it('shows each TaskSpec with its harness, effects, limits and outcomes', () => {
    const wrapper = mount(TaskSpecTable, {
      props: { taskSpecs: [...taskSpecs] },
    });
    const rows = wrapper
      .findAll('[data-testid="taskspec"]')
      .map((r) =>
        r
          .findAll('th > span, th > p, td')
          .map((c) => c.text().replace(/\s+/g, ' ')),
      );
    expect(rows[0]).toEqual([
      'triage',
      'Classify, de-duplicate and enrich an incoming issue or thread.',
      'codex (gpt-5*) allowed: codex, claude-code',
      'comment, label.add, label.remove, link-duplicate, create-issue',
      '40 turns, 15m, $1.50',
      'triaged, needs-info, duplicate, not-actionable, promote',
    ]);
    expect(rows[1]).toEqual([
      'implement',
      'Address review feedback on a pull request.',
      'claude-code (claude-sonnet-5-*)',
      'push-branch, pr.comment',
      '120 turns, 1h',
      '',
    ]);
    expect(rows[2]![2]).toBe('the agent picks');
    expect(rows[2]![4]).toBe('none');
  });
});
