import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import PrimeVue from 'primevue/config';
import type { Schemas } from '@/api/client';
import DecisionWhy from './DecisionWhy.vue';

const decision: Schemas['Decision'] = {
  at: '2026-10-17T14:02:01Z',
  event: 'github-public/gh-1',
  ruleSet: 'api',
  rule: 'implement-review-feedback',
  fired: true,
  inputs: { "item.count('implement')": 3 },
  rails: [
    { rail: 'run-cap', verdict: 'suppressed', reason: 'ran 3 of 3 times' },
    { rail: 'budget', verdict: 'allowed' },
  ],
  actions: ['dispatch triage → triage'],
};

let wrapper: VueWrapper | undefined;

async function open(props: {
  decision: Schemas['Decision'] | null;
  catalogSha?: string;
  visible?: boolean;
}) {
  wrapper = mount(DecisionWhy, {
    props: { visible: true, ...props },
    attachTo: document.body,
    global: { plugins: [[PrimeVue, { unstyled: true }]] },
  });
  await flushPromises();
  return document.body.querySelector<HTMLElement>('[role="dialog"]');
}

afterEach(() => {
  wrapper?.unmount();
  wrapper = undefined;
});

describe('DecisionWhy', () => {
  it('shows the RuleSet, rule, inputs, rail verdicts and catalog SHA', async () => {
    const dialog = await open({ decision, catalogSha: 'abc123' });
    expect(dialog).not.toBeNull();
    expect(dialog!.getAttribute('aria-label')).toBe(
      'Why: api / implement-review-feedback',
    );
    const text = (id: string) =>
      dialog!.querySelector(`[data-testid="${id}"]`)?.textContent?.trim();
    expect(text('why-ruleset')).toBe('api');
    expect(text('why-rule')).toBe('implement-review-feedback');
    expect(text('why-catalog-sha')).toBe('abc123');
    expect(dialog!.textContent).toContain('fired');
    expect(dialog!.textContent).toContain('2026-10-17 14:02:01 UTC');
    expect(dialog!.textContent).toContain('dispatch');

    const inputs = dialog!.querySelector('[aria-label="where inputs"]');
    expect(JSON.parse(inputs!.textContent!)).toEqual({
      "item.count('implement')": 3,
    });

    const rows = [...dialog!.querySelectorAll('tbody tr')].map((tr) =>
      [...tr.querySelectorAll('td')].map((td) => td.textContent?.trim()),
    );
    expect(rows).toEqual([
      ['run-cap', 'Suppressed', 'ran 3 of 3 times'],
      ['budget', 'Allowed', ''],
    ]);
  });

  it('says what the decision lacks', async () => {
    const dialog = await open({
      decision: { ruleSet: 'docs', rule: 'r', fired: false },
    });
    expect(
      dialog!.querySelector('[data-testid="why-catalog-sha"]')?.textContent,
    ).toContain('not recorded');
    expect(dialog!.textContent).toContain('did not fire');
    expect(dialog!.textContent).toContain('No rail verdicts.');
    expect(dialog!.textContent).toContain('none');
  });

  it('renders nothing while closed', async () => {
    const dialog = await open({ decision, visible: false });
    expect(dialog).toBeNull();
  });

  it('closes from its close button', async () => {
    const dialog = await open({ decision });
    dialog!.querySelector<HTMLButtonElement>('button')!.click();
    await flushPromises();
    expect(wrapper!.emitted('update:visible')).toEqual([[false]]);
  });
});
