import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import PrimeVue from 'primevue/config';
import { createMemoryHistory } from 'vue-router';
import type { Schemas } from '@/api/client';
import { confirmDialog } from '@/__tests__/commands';
import { maintainer } from '@/mocks/session';
import { example, workItemDetails } from '@/mocks/workitems';
import { createAppRouter } from '@/router';
import { useSessionStore } from '@/stores/session';
import WorkItemTimeline from './WorkItemTimeline.vue';

const byName = (name: string) =>
  workItemDetails.find((d) => d.workItem.metadata.name === name)!;
const pipeline = byName('wi_01JAC0PIPELINE0000000000000');

let attached: VueWrapper | undefined;

afterEach(() => {
  attached?.unmount();
  attached = undefined;
});

/** Mounts the timeline; with `me`, that user is signed in (a maintainer can replay). */
function mountTimeline(
  detail: Schemas['WorkItemDetail'],
  focus?: string,
  me?: Schemas['Me'],
) {
  const pinia = createPinia();
  setActivePinia(pinia);
  if (me) {
    const session = useSessionStore();
    session.me = me;
    session.status = 'signed-in';
  }
  const wrapper = mount(WorkItemTimeline, {
    props: { detail, focus },
    ...(me ? { attachTo: document.body } : {}),
    global: {
      plugins: [
        pinia,
        createAppRouter(createMemoryHistory()),
        [PrimeVue, { unstyled: true }],
      ],
    },
  });
  if (me) attached = wrapper;
  return wrapper;
}

const rows = (w: ReturnType<typeof mountTimeline>) =>
  w.findAll('[data-testid="timeline-event"]');

describe('WorkItemTimeline', () => {
  it('lists each event as a row, the newest open', () => {
    const wrapper = mountTimeline(pipeline);
    expect(
      rows(wrapper).map((r) =>
        [
          ...r.get('[data-testid="timeline-event-toggle"]').element.children,
        ].map((c) => c.textContent!.replace(/\s+/g, ' ').trim()),
      ),
    ).toEqual([
      [
        '▸',
        'issue.opened',
        'on github-public · compliance-framework/docs#77',
        '2 fired · 1 not fired',
        '2026-10-18 08:00:00 UTC',
      ],
      [
        '▸',
        'factory.run.completed',
        'on factory · run_01JAC0PIPELINETRIAGE000000',
        '1 fired · 1 not fired',
        '2026-10-18 08:06:00 UTC',
      ],
      [
        '▸',
        'issue.commented',
        'on github-public · compliance-framework/docs#77',
        '0 fired',
        '2026-10-18 08:06:03 UTC',
      ],
      [
        '▸',
        'pr.opened',
        'on github-public · compliance-framework/docs#78',
        '2 fired · 1 not fired',
        '2026-10-18 08:31:00 UTC',
      ],
      [
        '▸',
        'pr.labeled',
        'on github-public · compliance-framework/docs#78',
        '0 fired',
        '2026-10-18 08:31:03 UTC',
      ],
      [
        '▾',
        'factory.run.completed',
        'on factory · run_01JAC0PIPELINEIMPLEMENT02',
        '0 fired',
        '2026-10-18 08:31:30 UTC',
      ],
    ]);
    expect(
      rows(wrapper).map((r) =>
        r
          .get('[data-testid="timeline-event-toggle"]')
          .attributes('aria-expanded'),
      ),
    ).toEqual(['false', 'false', 'false', 'false', 'false', 'true']);
  });

  it('shows the fired decisions first, with their actions, runs and effects', async () => {
    const wrapper = mountTimeline(pipeline);
    const row = rows(wrapper)[1]!;
    await row.get('[data-testid="timeline-event-toggle"]').trigger('click');
    const body = row.get('[data-testid="timeline-event-body"]');
    const decision = body.get('[data-testid="timeline-decision"]');
    expect(decision.text()).toContain('docs / implement-promoted');
    const actions = decision.findAll('[data-testid="timeline-action"]');
    expect(actions.map((a) => a.find('span').text())).toEqual([
      'dispatch · implement',
      'notify · implement-started',
    ]);
    expect(actions[0]!.text()).toContain(
      'attempt 1 on agt_gusfcarvalho-mbp-01',
    );
    expect(actions[0]!.text()).toContain('outcome: pr-opened');
    expect(actions[0]!.text()).toContain(
      'pr.open on compliance-framework/docs#78',
    );
    expect(actions[0]!.find('a[href^="https://"]').attributes('href')).toBe(
      'https://github.com/compliance-framework/docs/pull/78',
    );
    // What didn't fire stays behind its toggle, with its reason and inputs.
    expect(body.find('[data-testid="timeline-not-fired"]').exists()).toBe(
      false,
    );
    const toggle = body.get('[data-testid="not-fired-toggle"]');
    expect(toggle.text()).toBe('Show 1 not fired');
    await toggle.trigger('click');
    const notFired = body.get('[data-testid="timeline-not-fired"]');
    expect(notFired.text()).toContain('docs / escalate-needs-human');
    expect(notFired.get('[data-testid="not-fired-why"]').text()).toBe(
      'where is false',
    );
    expect(notFired.get('[data-testid="not-fired-inputs"]').text()).toBe(
      'run.outcome = promote',
    );
    expect(toggle.attributes('aria-expanded')).toBe('true');
  });

  it('asks for the "why" of a decision by its index, fired or not', async () => {
    const wrapper = mountTimeline(
      pipeline,
      'event/github-public/gh-docs-77-opened',
    );
    const body = rows(wrapper)[0]!.get('[data-testid="timeline-event-body"]');
    await body.get('[data-testid="not-fired-toggle"]').trigger('click');
    const why = body.findAll('button[aria-haspopup="dialog"]');
    expect(why.map((b) => b.attributes('aria-label'))).toEqual([
      'Why: docs / triage-outside-issues',
      'Why: docs / docs/label-docs',
      'Why: docs / triage-maintainer-issues',
    ]);
    await why[2]!.trigger('click');
    await why[0]!.trigger('click');
    expect(wrapper.emitted('why')).toEqual([[2], [0]]);
  });

  it('opens the row the focus names, and shows a held rail on its decision', () => {
    const wrapper = mountTimeline(
      byName('wi_01JAC0ESCALATED0000000000000'),
      'event/github-public/gh-escalated-review-3',
    );
    const decisions = wrapper.findAll('[data-testid="timeline-decision"]');
    expect(decisions[0]!.text()).toContain('run-cap: Suppressed');
    expect(decisions[1]!.text()).not.toContain('effects-allowed');
  });

  it('renders untrusted text as text and links only https URLs', () => {
    const detail: Schemas['WorkItemDetail'] = {
      ...example,
      events: [
        {
          ...example.events[0]!,
          data: { ...example.events[0]!.data, facts: { title: '<b>bold</b>' } },
        },
      ],
      effects: [
        {
          ...example.effects[0]!,
          status: { result: { url: 'javascript:alert(1)' } },
        },
      ],
    };
    const wrapper = mountTimeline(detail);
    expect(wrapper.find('b').exists()).toBe(false);
    expect(wrapper.text()).toContain('<b>bold</b>');
    const action = wrapper.get('[data-testid="timeline-action"]');
    expect(action.find('a[href^="javascript"]').exists()).toBe(false);
    expect(action.text()).toContain('comment on compliance-framework/docs#42');
  });

  it('shows a failed effect', () => {
    const wrapper = mountTimeline({
      ...example,
      effects: [
        { ...example.effects[0]!, status: { result: { error: 'HTTP 403' } } },
      ],
    });
    expect(wrapper.get('[data-testid="timeline-action"]').text()).toContain(
      'failed: HTTP 403',
    );
  });

  it('lists what no decision accounts for apart', () => {
    const wrapper = mountTimeline({
      ...example,
      workItem: { ...example.workItem, status: {} },
    });
    expect(
      wrapper
        .findAll('[data-testid="timeline-other"]')
        .map((e) => e.attributes('data-kind')),
    ).toEqual(['task', 'run', 'effect']);
  });

  it('marks the decisions a replay recorded', async () => {
    const decisions = pipeline.workItem.status!.decisions!.map((d, i) =>
      i === 0 ? { ...d, replay: true } : d,
    );
    const wrapper = mountTimeline(
      {
        ...pipeline,
        workItem: {
          ...pipeline.workItem,
          status: { ...pipeline.workItem.status, decisions },
        },
      },
      'event/github-public/gh-docs-77-opened',
    );
    const body = rows(wrapper)[0]!.get('[data-testid="timeline-event-body"]');
    await body.get('[data-testid="not-fired-toggle"]').trigger('click');
    const marked = [
      ...body.findAll('[data-testid="timeline-decision"]'),
      ...body.findAll('[data-testid="timeline-not-fired"]'),
    ].filter((d) => d.find('[data-testid="decision-replay"]').exists());
    expect(marked.map((d) => d.find('span').text())).toEqual([
      `${decisions[0]!.ruleSet} / ${decisions[0]!.rule}`,
    ]);
  });

  it("replays an event for a maintainer and lists the replay's decisions", async () => {
    const wrapper = mountTimeline(example, undefined, maintainer);
    const event = example.events[0]!.id;
    await wrapper.get('[data-testid="replay"]').trigger('click');
    await flushPromises();
    expect(
      document.body.querySelector('[data-testid="confirm-request-line"]')!
        .textContent,
    ).toBe(`POST /api/v1/events/${encodeURIComponent(event)}/replay`);
    await confirmDialog('Replay');
    const result = wrapper.get('[data-testid="replay-result"]');
    expect(result.text()).toContain('The replay recorded 1 decision');
    expect(
      result
        .findAll('[data-testid="replay-decision"]')
        .map((d) => d.findAll(':scope > span').map((s) => s.text())),
    ).toEqual([['docs / label-docs-issues', 'fired', 'replay', 'effect']]);
    expect(wrapper.emitted('replayed')).toEqual([
      [expect.objectContaining({ event })],
    ]);
  });

  it('offers no replay to a reader', () => {
    const wrapper = mountTimeline(example);
    expect(wrapper.find('[data-testid="replay"]').exists()).toBe(false);
  });

  it('says when nothing has happened', () => {
    const wrapper = mountTimeline({
      ...example,
      workItem: { ...example.workItem, status: {} },
      events: [],
      tasks: [],
      runs: [],
      effects: [],
    });
    expect(wrapper.find('ol').exists()).toBe(false);
    expect(wrapper.text()).toContain('Nothing has happened yet.');
  });
});
