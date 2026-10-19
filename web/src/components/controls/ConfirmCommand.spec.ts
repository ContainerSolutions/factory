import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import { createPinia } from 'pinia';
import PrimeVue from 'primevue/config';
import { http, HttpResponse } from 'msw';
import { recordCommands } from '@/__tests__/commands';
import { server } from '@/mocks/node';
import { useControlsStore } from '@/stores/controls';
import ConfirmCommand from './ConfirmCommand.vue';
import { pauseCommand, type Command } from './commands';

let wrapper: VueWrapper | undefined;

async function open(command: Command, props: { invalid?: boolean } = {}) {
  wrapper = mount(ConfirmCommand, {
    props: {
      visible: true,
      title: 'Pause queue triage?',
      confirmLabel: 'Pause',
      command,
      ...props,
      'onUpdate:visible': (v: boolean) => wrapper!.setProps({ visible: v }),
    },
    slots: { default: '<p data-testid="explain">What it does.</p>' },
    attachTo: document.body,
    global: { plugins: [createPinia(), [PrimeVue, { unstyled: true }]] },
  });
  await flushPromises();
  return document.body.querySelector<HTMLElement>('[role="dialog"]');
}

const button = (dialog: HTMLElement, label: string) =>
  [...dialog.querySelectorAll('button')].find(
    (b) => b.textContent?.trim() === label,
  )!;

afterEach(() => {
  wrapper?.unmount();
  wrapper = undefined;
});

describe('ConfirmCommand', () => {
  it('shows what the command does and its exact JSON', async () => {
    const dialog = await open(pauseCommand('pause', 'queue', 'triage'));
    expect(dialog).not.toBeNull();
    expect(dialog!.textContent).toContain('Pause queue triage?');
    expect(dialog!.querySelector('[data-testid="explain"]')).not.toBeNull();
    const json = dialog!.querySelector('[aria-label="Command JSON"]');
    expect(JSON.parse(json!.textContent!)).toEqual({
      type: 'pause',
      target: { scope: 'queue', name: 'triage' },
    });
  });

  it('sends the command only when confirmed, then closes and emits the 202', async () => {
    const bodies = recordCommands();
    const dialog = await open(pauseCommand('pause', 'workitem', 'wi_1'));
    expect(bodies).toEqual([]);
    button(dialog!, 'Pause').click();
    await flushPromises();
    expect(bodies).toEqual([
      { type: 'pause', target: { scope: 'workitem', name: 'wi_1' } },
    ]);
    expect(wrapper!.emitted('accepted')).toEqual([
      [{ type: 'pause', event: 'factory/cmd-01JAB3R5S6T7V8W9X0Y1Z2A3BC' }],
    ]);
    expect(wrapper!.emitted('update:visible')).toEqual([[false]]);
    expect(useControlsStore().pauses).toEqual({ 'workitem:wi_1': 'paused' });
  });

  it('keeps the dialog open with the reason when the server refuses', async () => {
    server.use(
      http.post('/api/v1/commands', () =>
        HttpResponse.json(
          { title: 'Forbidden', status: 403, detail: 'not a maintainer' },
          {
            status: 403,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        ),
      ),
    );
    const dialog = await open(pauseCommand('pause', 'queue', 'triage'));
    button(dialog!, 'Pause').click();
    await flushPromises();
    expect(
      dialog!.querySelector('[data-testid="command-error"]')?.textContent,
    ).toContain('Only members of the maintainers Group can send commands');
    expect(wrapper!.emitted('accepted')).toBeUndefined();
    expect(wrapper!.emitted('update:visible')).toBeUndefined();
    // Nothing optimistic before a 2xx.
    expect(useControlsStore().pauses).toEqual({});
  });

  it('sends nothing when dismissed', async () => {
    const bodies = recordCommands();
    const dialog = await open(pauseCommand('resume', 'global'));
    button(dialog!, 'Go back').click();
    await flushPromises();
    expect(bodies).toEqual([]);
    expect(wrapper!.emitted('update:visible')).toEqual([[false]]);
  });

  it("can't confirm an invalid command", async () => {
    const bodies = recordCommands();
    const dialog = await open(pauseCommand('pause', 'global'), {
      invalid: true,
    });
    const confirm = button(dialog!, 'Pause');
    expect(confirm.disabled).toBe(true);
    confirm.click();
    await flushPromises();
    expect(bodies).toEqual([]);
  });
});
