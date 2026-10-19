import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { http, HttpResponse } from 'msw';
import type { Schemas } from '@/api/client';
import { recordCommands } from '@/__tests__/commands';
import { mountWithApp } from '@/__tests__/mount';
import { server } from '@/mocks/node';
import { maintainer, reader } from '@/mocks/session';
import { liveRun } from '@/mocks/workitems';
import RunControls from './RunControls.vue';

let mounted: VueWrapper | undefined;

async function mountControls(
  props: { run?: Schemas['Run']; running: boolean },
  me = maintainer,
) {
  const { wrapper } = await mountWithApp(RunControls, {
    me,
    props: { run: liveRun, ...props },
    attachTo: document.body,
  });
  mounted = wrapper;
  return wrapper;
}

const dialog = () =>
  document.body.querySelector<HTMLElement>('[role="dialog"]');
const dialogButton = (label: string) =>
  [...dialog()!.querySelectorAll('button')].find(
    (b) => b.textContent?.trim() === label,
  )!;

async function type(testid: string, value: string) {
  const input = dialog()!.querySelector<HTMLInputElement>(
    `[data-testid="${testid}"]`,
  )!;
  input.value = value;
  input.dispatchEvent(new Event('input'));
  await flushPromises();
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

describe('RunControls', () => {
  it('cancels a running run, then says so and hides the cancel', async () => {
    const bodies = recordCommands();
    const wrapper = await mountControls({ running: true });
    expect(wrapper.findAll('button').map((b) => b.text())).toEqual([
      'Cancel run',
    ]);
    await wrapper.get('button').trigger('click');
    await flushPromises();
    dialogButton('Cancel run').click();
    await flushPromises();
    expect(bodies).toEqual([
      { type: 'cancel', target: { task: liveRun.spec.task } },
    ]);
    expect(wrapper.get('[data-testid="run-control-notice"]').text()).toContain(
      'Cancel accepted',
    );
    expect(wrapper.find('button').exists()).toBe(false);
  });

  it('re-dispatches an ended run, on another harness and model when given', async () => {
    const bodies = recordCommands();
    const wrapper = await mountControls({ running: false });
    expect(wrapper.findAll('button').map((b) => b.text())).toEqual([
      'Re-dispatch',
    ]);
    await wrapper.get('button').trigger('click');
    await flushPromises();
    await type('redispatch-model', 'claude-sonnet-5-0');
    expect(dialog()!.textContent).toContain('A model needs a harness.');
    expect(dialogButton('Re-dispatch').disabled).toBe(true);

    await type('redispatch-harness', 'claude-code');
    const json = dialog()!.querySelector('[aria-label="Command JSON"]');
    const want = {
      type: 'redispatch',
      target: { task: liveRun.spec.task },
      args: {
        harness: {
          name: 'claude-code',
          config: { model: 'claude-sonnet-5-0' },
        },
      },
    };
    expect(JSON.parse(json!.textContent!)).toEqual(want);
    dialogButton('Re-dispatch').click();
    await flushPromises();
    expect(bodies).toEqual([want]);
    expect(wrapper.get('[data-testid="run-control-notice"]').text()).toContain(
      'Re-dispatch accepted',
    );
  });

  it("re-dispatches on the task's own harness by default", async () => {
    const bodies = recordCommands();
    const wrapper = await mountControls({ running: false });
    await wrapper.get('button').trigger('click');
    await flushPromises();
    dialogButton('Re-dispatch').click();
    await flushPromises();
    expect(bodies).toEqual([
      { type: 'redispatch', target: { task: liveRun.spec.task } },
    ]);
  });

  it('shows a conflict from the server in the dialog', async () => {
    server.use(
      http.post('/api/v1/commands', () =>
        HttpResponse.json(
          {
            title: 'Conflict',
            status: 409,
            code: 'task-active',
            detail: 'task is Running: cancel it first',
          },
          {
            status: 409,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        ),
      ),
    );
    const wrapper = await mountControls({ running: false });
    await wrapper.get('button').trigger('click');
    await flushPromises();
    dialogButton('Re-dispatch').click();
    await flushPromises();
    expect(dialog()!.textContent).toContain(
      'Conflict: task is Running: cancel it first',
    );
    expect(wrapper.find('[data-testid="run-control-notice"]').exists()).toBe(
      false,
    );
  });

  it('is hidden from readers', async () => {
    const wrapper = await mountControls({ running: true }, reader);
    expect(wrapper.find('[data-testid="run-controls"]').exists()).toBe(false);
  });
});
