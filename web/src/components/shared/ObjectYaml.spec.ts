import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import ObjectYaml from './ObjectYaml.vue';

describe('ObjectYaml', () => {
  it('shows the object as YAML once opened', async () => {
    const wrapper = mount(ObjectYaml, {
      props: {
        value: { kind: 'Queue', spec: { pool: 'local' } },
        label: 'YAML of q',
      },
    });
    const toggle = wrapper.get('[data-testid="yaml-toggle"]');
    expect(toggle.attributes('aria-expanded')).toBe('false');
    expect(wrapper.find('pre').exists()).toBe(false);
    await toggle.trigger('click');
    expect(toggle.attributes('aria-expanded')).toBe('true');
    expect(toggle.text()).toBe('Hide YAML');
    expect(wrapper.get('pre').text()).toBe('kind: Queue\nspec:\n  pool: local');
    expect(wrapper.get('pre').attributes('aria-label')).toBe('YAML of q');
  });
});
