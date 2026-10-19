import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import PrimeVue from 'primevue/config';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import EvaluationView from './EvaluationView.vue';

const evaluation = examples.get('dryRunCatalog') as Schemas['Evaluation'];

let wrapper: VueWrapper | undefined;

function mountView(e: Schemas['Evaluation'] = evaluation) {
  wrapper = mount(EvaluationView, {
    props: { evaluation: e, catalogSha: 'abc123' },
    attachTo: document.body,
    global: { plugins: [[PrimeVue, { unstyled: true }]] },
  });
  return wrapper;
}

afterEach(() => {
  wrapper?.unmount();
  wrapper = undefined;
});

describe('EvaluationView', () => {
  it('shows every candidate rule, the actions with their verdicts, and the state after', () => {
    const view = mountView();
    expect(view.text()).toContain(evaluation.event);
    expect(view.text()).toContain(evaluation.workItem!);
    expect(
      view
        .findAll('[data-testid="match"]')
        .map((m) => m.findAll('td').map((c) => c.text())),
    ).toEqual([
      ['docs', 'triage-outside-issues', 'matched', ''],
      ['docs', 'triage-maintainer-issues', 'not-matched', ''],
    ]);
    const decisions = view.findAll('[data-testid="decision"]');
    expect(
      decisions.map((d) => [
        d.get('[data-testid="decision-action"]').text(),
        d.get('[data-testid="decision-verdict"]').text(),
      ]),
    ).toEqual([
      ['dispatch', 'Allowed'],
      ['set', 'Allowed'],
    ]);
    expect(
      JSON.parse(decisions[1]!.get('[aria-label="Action 2: set"]').text()),
    ).toEqual({ key: 'triage', value: 'running' });
    expect(JSON.parse(view.get('[aria-label="State after"]').text())).toEqual({
      triage: 'running',
    });
  });

  it("opens a decision's why drawer with its inputs and the catalog commit", async () => {
    const view = mountView();
    await view
      .get('button[aria-label="Why: docs / triage-outside-issues (action 1)"]')
      .trigger('click');
    await flushPromises();
    const drawer = document.body.querySelector<HTMLElement>(
      '[data-testid="decision-why"]',
    )!;
    expect(
      drawer.querySelector('[data-testid="why-catalog-sha"]')!.textContent,
    ).toContain('abc123');
    expect(
      JSON.parse(
        drawer.querySelector('[aria-label="where inputs"]')!.textContent!,
      ),
    ).toEqual({ 'actor.tier': 'contributor', 'actor.isBot': false });
    expect(drawer.textContent).toContain('fired');
  });

  it('says when nothing would happen, and shows errors as text', () => {
    const view = mountView({
      event: 'e1',
      matches: [
        {
          ruleSet: 'docs',
          rule: 'r',
          result: 'action-error',
          actionIndex: 0,
          error: '<img src=x>',
        },
      ],
      decisions: [],
    });
    expect(view.text()).toContain('against an empty work item');
    expect(view.text()).toContain('No action would be taken.');
    expect(view.get('[data-testid="match"]').text()).toContain(
      '<img src=x> (action 0)',
    );
    expect(view.find('img').exists()).toBe(false);
  });
});
