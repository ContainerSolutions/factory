import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { recordCommands } from '@/__tests__/commands';
import { mountWithApp } from '@/__tests__/mount';
import { maintainer, reader } from '@/mocks/session';
import CommandButton from './CommandButton.vue';

let mounted: VueWrapper | undefined;

async function mountButton(me?: typeof maintainer) {
  const { wrapper } = await mountWithApp(CommandButton, {
    me,
    props: {
      label: 'Resync catalog',
      title: 'Resync the catalog?',
      confirmLabel: 'Resync',
      command: { type: 'resync-catalog' },
    },
    slots: { default: 'Re-reads it now.' },
    attachTo: document.body,
  });
  mounted = wrapper;
  return wrapper;
}

const dialog = () =>
  document.body.querySelector<HTMLElement>('[role="dialog"]');

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

describe('CommandButton', () => {
  it('is hidden from readers and without a session', async () => {
    expect((await mountButton(reader)).find('button').exists()).toBe(false);
    mounted!.unmount();
    expect((await mountButton()).find('button').exists()).toBe(false);
  });

  it('opens its confirmation dialog for a maintainer, and sends on confirm', async () => {
    const bodies = recordCommands();
    const wrapper = await mountButton(maintainer);
    const button = wrapper.get('button');
    expect(button.text()).toBe('Resync catalog');
    expect(button.attributes('aria-haspopup')).toBe('dialog');
    expect(dialog()).toBeNull();

    await button.trigger('click');
    await flushPromises();
    expect(dialog()!.textContent).toContain('Resync the catalog?');
    expect(dialog()!.textContent).toContain('Re-reads it now.');
    [...dialog()!.querySelectorAll('button')]
      .find((b) => b.textContent?.trim() === 'Resync')!
      .click();
    await flushPromises();
    expect(bodies).toEqual([{ type: 'resync-catalog' }]);
    expect(wrapper.emitted('accepted')).toEqual([[{ type: 'resync-catalog' }]]);
  });
});
