import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import TabList from './TabList.vue';
import { pickTab, tabPanelAttrs, type Tab } from './tabs';

const tabs: Tab[] = [
  { id: 'a', label: 'Alpha', count: 2 },
  { id: 'b', label: 'Beta' },
  { id: 'c', label: 'Gamma' },
];

function mountTabs(modelValue = 'a') {
  return mount(TabList, {
    props: { tabs, modelValue, label: 'Kinds', idPrefix: 'k' },
    attachTo: document.body,
  });
}

describe('TabList', () => {
  it('marks the selected tab and keeps only it in the tab order', () => {
    const wrapper = mountTabs('b');
    const buttons = wrapper.findAll('[role="tab"]');
    expect(buttons.map((b) => b.text())).toEqual(['Alpha2', 'Beta', 'Gamma']);
    expect(buttons.map((b) => b.attributes('aria-selected'))).toEqual([
      'false',
      'true',
      'false',
    ]);
    expect(buttons.map((b) => b.attributes('tabindex'))).toEqual([
      '-1',
      '0',
      '-1',
    ]);
    expect(buttons[1]!.attributes('aria-controls')).toBe('k-panel-b');
    wrapper.unmount();
  });

  it.each<[string, string, string]>([
    ['a', 'ArrowRight', 'b'],
    ['c', 'ArrowRight', 'a'],
    ['a', 'ArrowLeft', 'c'],
    ['b', 'Home', 'a'],
    ['a', 'End', 'c'],
  ])('from %s, %s selects %s', async (from, key, want) => {
    const wrapper = mountTabs(from);
    await wrapper.get(`[data-tab="${from}"]`).trigger('keydown', { key });
    expect(wrapper.emitted('update:modelValue')).toEqual([[want]]);
    expect(document.activeElement?.getAttribute('data-tab')).toBe(want);
    wrapper.unmount();
  });

  it('selects on click and ignores other keys', async () => {
    const wrapper = mountTabs();
    await wrapper.get('[data-tab="c"]').trigger('click');
    await wrapper.get('[data-tab="a"]').trigger('keydown', { key: 'x' });
    expect(wrapper.emitted('update:modelValue')).toEqual([['c']]);
    wrapper.unmount();
  });
});

describe('tabs', () => {
  it('links a panel to its tab', () => {
    expect(tabPanelAttrs('k', 'a')).toEqual({
      id: 'k-panel-a',
      role: 'tabpanel',
      'aria-labelledby': 'k-tab-a',
      tabindex: 0,
    });
  });

  it.each<[unknown, string]>([
    ['b', 'b'],
    ['nope', 'a'],
    [undefined, 'a'],
  ])('pickTab(%s) is %s', (wanted, want) => {
    expect(pickTab(tabs, wanted)).toBe(want);
  });
});
