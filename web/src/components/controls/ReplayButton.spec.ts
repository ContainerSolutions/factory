import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { http, HttpResponse } from 'msw';
import type { Schemas } from '@/api/client';
import { confirmDialog } from '@/__tests__/commands';
import { mountWithApp } from '@/__tests__/mount';
import { server } from '@/mocks/node';
import { maintainer, problem } from '@/mocks/session';
import ReplayButton from './ReplayButton.vue';

const EVENT = 'github-public/gh-docs-77-opened';

let mounted: VueWrapper | undefined;

async function replay() {
  const { wrapper } = await mountWithApp(ReplayButton, {
    me: maintainer,
    props: { event: EVENT },
    attachTo: document.body,
  });
  mounted = wrapper;
  await wrapper.get('[data-testid="replay"]').trigger('click');
  await flushPromises();
  await confirmDialog('Replay');
  return wrapper;
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

describe('ReplayButton', () => {
  it('says when the replay changed nothing', async () => {
    server.use(
      http.post('/api/v1/events/:id/replay', () =>
        HttpResponse.json<Schemas['ReplayResult']>({
          event: EVENT,
          decisions: [],
        }),
      ),
    );
    const wrapper = await replay();
    expect(wrapper.get('[data-testid="replay-result"]').text()).toBe(
      "The replay changed nothing: no rule acts on it now that didn't then.",
    );
    expect(wrapper.emitted('replayed')).toHaveLength(1);
  });

  it('keeps a refusal in the dialog, with who may replay', async () => {
    server.use(
      http.post('/api/v1/events/:id/replay', () =>
        problem(403, 'Forbidden', 'forbidden', 'needs the maintainers Group'),
      ),
    );
    const wrapper = await replay();
    expect(
      document.body.querySelector('[data-testid="request-error"]')!.textContent,
    ).toContain(
      'Only members of the maintainers Group can replay events, close or delete work items.',
    );
    expect(wrapper.find('[data-testid="replay-result"]').exists()).toBe(false);
    expect(wrapper.emitted('replayed')).toBeUndefined();
  });
});
