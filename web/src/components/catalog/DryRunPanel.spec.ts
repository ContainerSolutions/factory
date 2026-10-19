import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { http } from 'msw';
import Select from 'primevue/select';
import type { Schemas } from '@/api/client';
import { mountWithApp } from '@/__tests__/mount';
import { server } from '@/mocks/node';
import { workItemDetails } from '@/mocks/workitems';
import DryRunPanel from './DryRunPanel.vue';

let mounted: VueWrapper | undefined;

async function mountPanel() {
  const { wrapper } = await mountWithApp(DryRunPanel, {
    props: { catalogSha: 'abc123' },
    attachTo: document.body,
  });
  mounted = wrapper;
  return wrapper;
}

function recordDryRuns(): Schemas['DryRunRequest'][] {
  const bodies: Schemas['DryRunRequest'][] = [];
  server.use(
    http.post('/api/v1/catalog/dry-run', async ({ request }) => {
      bodies.push((await request.clone().json()) as Schemas['DryRunRequest']);
      return undefined;
    }),
  );
  return bodies;
}

// The PrimeVue Selects inside the Volt wrappers.
const selects = (wrapper: VueWrapper) =>
  wrapper
    .findAllComponents(Select)
    .filter((c) => c.props('options') !== undefined);

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

const escalated = workItemDetails.find(
  (d) => d.workItem.metadata.name === 'wi_01JAC0ESCALATED0000000000000',
)!;

describe('DryRunPanel', () => {
  it('evaluates a pasted event against an empty work item', async () => {
    const bodies = recordDryRuns();
    const wrapper = await mountPanel();
    const event = escalated.events[0]!;
    await wrapper
      .get('[data-testid="dry-run-event"]')
      .setValue(JSON.stringify(event));
    await wrapper.get('form').trigger('submit');
    await flushPromises();
    expect(bodies).toEqual([{ event }]);
    expect(wrapper.get('[data-testid="evaluation"]').text()).toContain(
      event.id,
    );
    expect(wrapper.text()).toContain('against an empty work item');
  });

  it('replays a recorded event against its work item', async () => {
    const bodies = recordDryRuns();
    const wrapper = await mountPanel();
    const [items, events] = selects(wrapper);
    expect(items!.props('options')).toContainEqual({
      label: 'compliance-framework/api#310',
      value: escalated.workItem.metadata.name,
    });
    expect(events!.props('disabled')).toBe(true);

    await items!.vm.$emit(
      'update:modelValue',
      escalated.workItem.metadata.name,
    );
    await flushPromises();
    const options = selects(wrapper)[1]!.props('options') as {
      label: string;
      value: string;
    }[];
    expect(options.length).toBe(escalated.events.length);
    const newest = [...escalated.events].sort(
      (a, b) => Date.parse(b.time) - Date.parse(a.time),
    )[0]!;
    expect(options[0]!.value).toBe(newest.id);
    expect(options[0]!.label).toContain(newest.type);

    await selects(wrapper)[1]!.vm.$emit('update:modelValue', newest.id);
    await flushPromises();
    const textarea = wrapper.get<HTMLTextAreaElement>(
      '[data-testid="dry-run-event"]',
    );
    expect(JSON.parse(textarea.element.value)).toEqual(newest);
    expect(
      wrapper.get<HTMLInputElement>('[data-testid="dry-run-workitem"]').element
        .value,
    ).toBe(escalated.workItem.metadata.name);

    await wrapper.get('form').trigger('submit');
    await flushPromises();
    expect(bodies).toEqual([
      { event: newest, workItem: escalated.workItem.metadata.name },
    ]);
    expect(wrapper.get('[data-testid="evaluation"]').text()).toContain(
      escalated.workItem.metadata.name,
    );
  });

  it.each([
    ['', 'Paste an event, or pick a recorded one.'],
    ['{nope', 'Not JSON'],
    ['[1]', 'The event must be a JSON object.'],
  ])('refuses %j before sending it', async (text, want) => {
    const bodies = recordDryRuns();
    const wrapper = await mountPanel();
    await wrapper.get('[data-testid="dry-run-event"]').setValue(text);
    await wrapper.get('form').trigger('submit');
    await flushPromises();
    expect(bodies).toEqual([]);
    expect(wrapper.get('[data-testid="dry-run-error"]').text()).toContain(want);
  });

  it("shows the server's field errors", async () => {
    const wrapper = await mountPanel();
    await wrapper.get('[data-testid="dry-run-event"]').setValue('{}');
    await wrapper.get('form').trigger('submit');
    await flushPromises();
    const error = wrapper.get('[data-testid="dry-run-error"]');
    expect(error.text()).toContain("Couldn't evaluate the event");
    expect(error.get('li').text()).toBe('event.id: event.id: required');
    expect(wrapper.find('[data-testid="evaluation"]').exists()).toBe(false);
  });
});
