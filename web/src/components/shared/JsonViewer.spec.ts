import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import JsonViewer from './JsonViewer.vue';

describe('JsonViewer', () => {
  it('shows the value as indented JSON in a labelled, focusable region', () => {
    const wrapper = mount(JsonViewer, {
      props: { value: { 'actor.tier': 'contributor', n: 1 }, label: 'inputs' },
    });
    const pre = wrapper.get('pre');
    expect(pre.attributes('role')).toBe('region');
    expect(pre.attributes('aria-label')).toBe('inputs');
    expect(pre.attributes('tabindex')).toBe('0');
    expect(pre.text()).toBe('{\n  "actor.tier": "contributor",\n  "n": 1\n}');
  });

  it('renders markup as text', () => {
    const wrapper = mount(JsonViewer, {
      props: { value: { title: '<img src=x onerror=alert(1)>' }, label: 'x' },
    });
    expect(wrapper.find('img').exists()).toBe(false);
    expect(wrapper.text()).toContain('<img src=x onerror=alert(1)>');
  });

  it.each([
    [undefined, 'null'],
    [[], '[]'],
  ])('shows %o as %s', (value, want) => {
    const wrapper = mount(JsonViewer, { props: { value, label: 'x' } });
    expect(wrapper.text()).toBe(want);
  });
});
