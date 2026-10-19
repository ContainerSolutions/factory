import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import PrimeVue from 'primevue/config';
import Select from 'primevue/select';
import BoardFilters from './BoardFilters.vue';
import { NO_FILTERS, type BoardFilters as Filters } from './board';

const facets = {
  ruleSet: ['api', 'docs'],
  provider: ['github-public'],
  repo: ['compliance-framework/docs'],
  taskSpec: ['triage'],
};

// The PrimeVue Selects inside the Volt wrappers, which share their name.
function selectsOf(wrapper: ReturnType<typeof mountFilters>) {
  return wrapper
    .findAllComponents(Select)
    .filter((c) => c.props('options') !== undefined);
}

function mountFilters(modelValue: Filters = { ...NO_FILTERS }) {
  return mount(BoardFilters, {
    props: { modelValue, facets },
    global: { plugins: [[PrimeVue, { unstyled: true }]] },
  });
}

describe('BoardFilters', () => {
  it('labels a filter per facet, each with an "all" option first', () => {
    const wrapper = mountFilters();
    expect(wrapper.get('form').attributes('role')).toBe('search');
    const selects = selectsOf(wrapper);
    const labels = selects.map((s) => {
      const id = s.props('ariaLabelledby');
      return wrapper.get(`#${id}`).text();
    });
    expect(labels).toEqual(['RuleSet', 'Provider', 'Repository', 'TaskSpec']);
    expect(selects[0]!.props('options')).toEqual([
      { label: 'All RuleSets', value: '' },
      { label: 'api', value: 'api' },
      { label: 'docs', value: 'docs' },
    ]);
  });

  it('keeps a set value that is no longer a facet', () => {
    const wrapper = mountFilters({ ...NO_FILTERS, provider: 'gone' });
    const provider = selectsOf(wrapper)[1]!;
    expect(provider.props('options')).toContainEqual({
      label: 'gone',
      value: 'gone',
    });
  });

  it('updates one filter, keeping the others', async () => {
    const wrapper = mountFilters({ ...NO_FILTERS, repo: 'r' });
    await selectsOf(wrapper)[0]!.vm.$emit('update:modelValue', 'docs');
    expect(wrapper.emitted('update:modelValue')).toEqual([
      [{ ...NO_FILTERS, repo: 'r', ruleSet: 'docs' }],
    ]);
  });

  it('clears every filter, and offers to only when one is set', async () => {
    expect(mountFilters().find('button').exists()).toBe(false);
    const wrapper = mountFilters({ ...NO_FILTERS, ruleSet: 'docs' });
    const clear = wrapper.get('button');
    expect(clear.text()).toBe('Clear filters');
    await clear.trigger('click');
    expect(wrapper.emitted('update:modelValue')).toEqual([[NO_FILTERS]]);
  });
});
