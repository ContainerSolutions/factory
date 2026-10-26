import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { http } from 'msw';
import type { Schemas } from '@/api/client';
import { confirmDialog } from '@/__tests__/commands';
import { mountWithApp } from '@/__tests__/mount';
import { server } from '@/mocks/node';
import { maintainer, reader } from '@/mocks/session';
import { example, workItemDetails } from '@/mocks/workitems';
import WorkItemAdmin from './WorkItemAdmin.vue';

const RUNNING = example.workItem; // its triage task is Running
const IDLE = workItemDetails.find(
  (d) => d.workItem.metadata.name === 'wi_01JAC0ESCALATED0000000000000',
)!.workItem;

let mounted: VueWrapper | undefined;

async function mountAdmin(item = RUNNING, me = maintainer) {
  const { wrapper } = await mountWithApp(WorkItemAdmin, {
    me,
    props: { item },
    attachTo: document.body,
  });
  mounted = wrapper;
  return wrapper;
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

/** Records each request to `path` (method, URL and JSON body) while the mocks answer it. */
function record(method: 'post' | 'delete', path: string) {
  const seen: { url: string; body?: unknown }[] = [];
  server.use(
    http[method](path, async ({ request }) => {
      const text = await request.clone().text();
      seen.push({
        url: request.url,
        ...(text ? { body: JSON.parse(text) } : {}),
      });
      return undefined;
    }),
  );
  return seen;
}

const inDialog = (testid: string) =>
  document.body.querySelector<HTMLElement>(
    `[role="dialog"] [data-testid="${testid}"]`,
  );

describe('WorkItemAdmin', () => {
  it('closes the work item with a reason and a note once confirmed', async () => {
    const seen = record('post', '/api/v1/workitems/:id/close');
    const wrapper = await mountAdmin();
    await wrapper.get('[data-testid="close-workitem"]').trigger('click');
    await flushPromises();
    inDialog('close-reason-duplicate')!.click();
    const note = inDialog('close-note') as HTMLTextAreaElement;
    note.value = '  same as docs#40 ';
    note.dispatchEvent(new Event('input'));
    await flushPromises();
    expect(inDialog('confirm-request-line')!.textContent).toBe(
      `POST /api/v1/workitems/${RUNNING.metadata.name}/close`,
    );
    await confirmDialog('Close');
    expect(seen.map((s) => s.body)).toEqual([
      { reason: 'duplicate', note: 'same as docs#40' },
    ]);
    expect(document.body.querySelector('[role="dialog"]')).toBeNull();
    const closed = wrapper.emitted('closed') as [Schemas['WorkItem']][];
    expect(closed[0]![0].status?.closed).toEqual(
      expect.objectContaining({ reason: 'duplicate', note: 'same as docs#40' }),
    );
  });

  it('offers to force a delete a leased task refused, and then deletes', async () => {
    const seen = record('delete', '/api/v1/workitems/:id');
    const wrapper = await mountAdmin();
    await wrapper.get('[data-testid="delete-workitem"]').trigger('click');
    await flushPromises();
    expect(inDialog('delete-force')).toBeNull();
    await confirmDialog('Delete');
    expect(inDialog('request-error')!.textContent).toContain(
      'Conflict: task tsk_',
    );
    expect(inDialog('delete-force')).not.toBeNull();
    expect(inDialog('confirm-request-line')!.textContent).toBe(
      `DELETE /api/v1/workitems/${RUNNING.metadata.name}?force=true`,
    );
    await confirmDialog('Cancel tasks and delete');
    expect(seen.map((s) => new URL(s.url).search)).toEqual(['', '?force=true']);
    expect(document.body.querySelector('[role="dialog"]')).toBeNull();
    expect(wrapper.emitted('deleted')).toEqual([
      [expect.objectContaining({ workItem: RUNNING.metadata.name })],
    ]);
  });

  it('deletes an item with no leased task at once', async () => {
    const wrapper = await mountAdmin(IDLE);
    await wrapper.get('[data-testid="delete-workitem"]').trigger('click');
    await flushPromises();
    await confirmDialog('Delete');
    expect(wrapper.emitted('deleted')).toHaveLength(1);
  });

  it('shows a reader no controls', async () => {
    const wrapper = await mountAdmin(RUNNING, reader);
    expect(wrapper.find('[data-testid="workitem-admin"]').exists()).toBe(false);
  });
});
