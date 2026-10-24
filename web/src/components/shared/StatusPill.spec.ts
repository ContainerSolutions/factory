import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import StatusPill from './StatusPill.vue';

describe('StatusPill', () => {
  it.each([
    [{ label: 'running' }, 'info'],
    [{ label: 'needs-human' }, 'danger'],
    [{ label: 'something else' }, 'neutral'],
    [{ label: 'running', tone: 'warn' as const }, 'warn'],
  ])('shows %o with the %s tone', (props, tone) => {
    const wrapper = mount(StatusPill, { props });
    // The text is always there: colour is never the only signal.
    expect(wrapper.text()).toBe(props.label);
    expect(wrapper.attributes('data-tone')).toBe(tone);
  });

  it('truncates a long label to its box, with the whole label as its title', () => {
    const label = `lastReview: ${'x'.repeat(300)}`;
    const wrapper = mount(StatusPill, { props: { label } });
    expect(wrapper.classes()).toEqual(
      expect.arrayContaining(['max-w-full', 'truncate']),
    );
    expect(wrapper.attributes('title')).toBe(label);
    expect(wrapper.text()).toBe(label);
  });
});
