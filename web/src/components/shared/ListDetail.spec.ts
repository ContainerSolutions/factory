import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import ListDetail from './ListDetail.vue';

const items = [
  { id: 'a', label: 'alpha', secondary: 'first' },
  { id: 'b', label: 'beta' },
];

describe('ListDetail', () => {
  it('lists buttons, marks the picked one current, and shows its details', async () => {
    const wrapper = mount(ListDetail, {
      props: { items, selected: 'b', label: 'Things' },
      slots: { default: '<p data-testid="detail">beta details</p>' },
    });
    expect(wrapper.get('nav').attributes('aria-label')).toBe('Things');
    const buttons = wrapper.findAll('button');
    expect(
      buttons.map((b) => [b.text(), b.attributes('aria-current')]),
    ).toEqual([
      ['alphafirst', undefined],
      ['beta', 'true'],
    ]);
    expect(wrapper.get('[data-testid="detail"]').text()).toBe('beta details');
    await buttons[0]!.trigger('click');
    expect(wrapper.emitted('select')).toEqual([['a']]);
  });

  it('shows no details with nothing picked', () => {
    const wrapper = mount(ListDetail, {
      props: { items: [], selected: '', label: 'Things' },
      slots: { default: '<p data-testid="detail">x</p>' },
    });
    expect(wrapper.find('[data-testid="detail"]').exists()).toBe(false);
  });
});
