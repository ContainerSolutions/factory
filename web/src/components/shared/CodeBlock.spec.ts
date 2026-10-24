import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import CodeBlock from './CodeBlock.vue';
import { tokenizeCEL } from './highlight';

describe('CodeBlock', () => {
  it('renders each token as a text span with its kind', () => {
    const wrapper = mount(CodeBlock, {
      props: { tokens: tokenizeCEL("a == '<b>x</b>'"), label: 'Where' },
    });
    const pre = wrapper.get('pre');
    expect(pre.attributes('aria-label')).toBe('Where');
    expect(pre.text()).toBe("a == '<b>x</b>'");
    expect(pre.find('b').exists()).toBe(false);
    expect(
      wrapper.findAll('span').map((s) => [s.attributes('data-kind'), s.text()]),
    ).toEqual([
      ['variable', 'a'],
      ['plain', ''],
      ['operator', '=='],
      ['plain', ''],
      ['string', "'<b>x</b>'"],
    ]);
  });

  it('renders inline as code', () => {
    const wrapper = mount(CodeBlock, {
      props: {
        tokens: [{ text: 'x', kind: 'plain' }],
        label: 'x',
        inline: true,
      },
    });
    expect(wrapper.find('pre').exists()).toBe(false);
    expect(wrapper.get('code').text()).toBe('x');
  });
});
