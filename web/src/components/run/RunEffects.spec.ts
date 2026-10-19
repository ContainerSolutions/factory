import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import RunEffects from './RunEffects.vue';

const effect = (examples.get('getRun') as Schemas['RunDetail']).effects[0]!;

describe('RunEffects', () => {
  it('lists each effect with a link to what it made', () => {
    const wrapper = mount(RunEffects, { props: { effects: [effect] } });
    const item = wrapper.get('[data-testid="effect"]');
    expect(item.text()).toContain('comment on compliance-framework/docs#42');
    expect(item.text()).toContain('as author · 2026-10-16 09:33:21 UTC');
    expect(item.get('a').attributes()).toMatchObject({
      href: 'https://github.com/compliance-framework/docs/issues/42#issuecomment-2401',
      target: '_blank',
      rel: 'noopener noreferrer',
    });
  });

  it('links https URLs only', () => {
    const wrapper = mount(RunEffects, {
      props: {
        effects: [
          { ...effect, status: { result: { url: 'javascript:alert(1)' } } },
        ],
      },
    });
    expect(wrapper.find('a').exists()).toBe(false);
    expect(wrapper.text()).toContain('javascript:alert(1)');
  });

  it('shows a failed effect', () => {
    const wrapper = mount(RunEffects, {
      props: {
        effects: [{ ...effect, status: { result: { error: 'HTTP 403' } } }],
      },
    });
    expect(wrapper.text()).toContain('failed');
    expect(wrapper.text()).toContain('HTTP 403');
    expect(wrapper.find('a').exists()).toBe(false);
  });

  it('says when there are none', () => {
    const wrapper = mount(RunEffects, { props: { effects: [] } });
    expect(wrapper.text()).toContain('No effects.');
  });
});
