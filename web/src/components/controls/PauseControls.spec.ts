import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { http, HttpResponse } from 'msw';
import { recordCommands } from '@/__tests__/commands';
import { mountWithApp } from '@/__tests__/mount';
import { server } from '@/mocks/node';
import { maintainer, reader } from '@/mocks/session';
import PauseControls from './PauseControls.vue';
import type { PauseScope } from './commands';

let mounted: VueWrapper | undefined;

async function mountControls(
  scope: PauseScope,
  name?: string,
  me = maintainer,
) {
  const { wrapper } = await mountWithApp(PauseControls, {
    me,
    props: { scope, name },
    attachTo: document.body,
  });
  mounted = wrapper;
  return wrapper;
}

const dialog = () =>
  document.body.querySelector<HTMLElement>('[role="dialog"]');

async function press(wrapper: VueWrapper, label: string, confirm: string) {
  await wrapper.get(`button[aria-label="${label}"]`).trigger('click');
  await flushPromises();
  [...dialog()!.querySelectorAll('button')]
    .find((b) => b.textContent?.trim() === confirm)!
    .click();
  await flushPromises();
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

describe('PauseControls', () => {
  it.each([
    ['global', undefined, 'the whole factory', { scope: 'global' }],
    [
      'provider',
      'github-public',
      'provider github-public',
      { scope: 'provider', name: 'github-public' },
    ],
    ['queue', 'triage', 'queue triage', { scope: 'queue', name: 'triage' }],
    ['workitem', 'wi_1', 'work item wi_1', { scope: 'workitem', name: 'wi_1' }],
  ] as const)('pauses and resumes %s %s', async (scope, name, what, target) => {
    const bodies = recordCommands();
    const wrapper = await mountControls(scope, name);
    expect(wrapper.find('[data-testid="pause-state"]').exists()).toBe(false);

    await press(wrapper, `Pause ${what}`, 'Pause');
    expect(bodies).toEqual([{ type: 'pause', target }]);
    expect(wrapper.get('[data-testid="pause-state"]').text()).toBe(
      'pause sent',
    );

    await press(wrapper, `Resume ${what}`, 'Resume');
    expect(bodies[1]).toEqual({ type: 'resume', target });
    expect(wrapper.get('[data-testid="pause-state"]').text()).toBe(
      'resume sent',
    );
  });

  it('shows nothing optimistic when the server refuses', async () => {
    server.use(
      http.post('/api/v1/commands', () =>
        HttpResponse.json(
          { title: 'Not Found', status: 404, detail: 'no queue "gone"' },
          {
            status: 404,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        ),
      ),
    );
    const wrapper = await mountControls('queue', 'gone');
    await press(wrapper, 'Pause queue gone', 'Pause');
    expect(wrapper.find('[data-testid="pause-state"]').exists()).toBe(false);
    expect(
      dialog()!.querySelector('[data-testid="command-error"]')!.textContent,
    ).toContain('Not Found: no queue "gone"');
  });

  it('is hidden from readers', async () => {
    const wrapper = await mountControls('global', undefined, reader);
    expect(wrapper.find('[data-testid="pause-controls"]').exists()).toBe(false);
  });
});
