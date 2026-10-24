import { afterEach, beforeAll, describe, expect, it } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { http, HttpResponse } from 'msw';
import { confirmDialog, recordCommands } from '@/__tests__/commands';
import { mountWithApp } from '@/__tests__/mount';
import { server } from '@/mocks/node';
import { maintainer, reader } from '@/mocks/session';
import { renderMarkdown } from '@/components/shared/markdown';
import CatalogView from './CatalogView.vue';

beforeAll(async () => {
  await renderMarkdown('');
});

let mounted: VueWrapper | undefined;

async function mountCatalog(me = maintainer, path = '/catalog') {
  const { wrapper, router } = await mountWithApp(CatalogView, {
    path,
    me,
    attachTo: document.body,
  });
  mounted = wrapper;
  return Object.assign(wrapper, { router });
}

// The tab's own list, not a RuleSet's list of rules inside its detail.
const listed = (wrapper: VueWrapper) =>
  wrapper
    .get('[data-testid="list-detail"]')
    .findAll(':scope > nav [data-testid="list-item"]')
    .map(
      (b) => (b.attributes('aria-current') ? '*' : '') + b.find('span').text(),
    );

async function openTab(wrapper: VueWrapper, label: string): Promise<void> {
  const tab = wrapper
    .findAll('[role="tab"]')
    .find((t) => t.text().startsWith(label));
  if (!tab) throw new Error(`no tab ${label}`);
  await tab.trigger('click');
  await flushPromises();
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

describe('CatalogView', () => {
  it('shows the commit, a tab per kind with its count, the TaskSpecs and the dry run', async () => {
    const wrapper = await mountCatalog();
    expect(wrapper.get('h1').text()).toBe('Catalog');
    expect(wrapper.get('[data-testid="catalog-sha"]').text()).toBe(
      '9f1e2d3c4b5a',
    );
    const tabs = wrapper.findAll('[role="tab"]');
    expect(tabs.map((t) => t.text())).toEqual([
      'Task Specs3',
      'Prompts2',
      'Rules5',
      'RuleSets2',
      'Templates1',
      'Providers3',
      'Queues and Pools6',
      'Groups2',
      'Result Schemas2',
    ]);
    expect(tabs[0]!.attributes('aria-selected')).toBe('true');
    expect(wrapper.get('[role="tabpanel"]').attributes('aria-labelledby')).toBe(
      'catalog-tab-taskspecs',
    );
    // A list of TaskSpecs, the first one open beside it.
    expect(listed(wrapper)).toEqual(['*implement', 'review', 'triage']);
    expect(
      wrapper.findAll('[data-testid="taskspec"] h3').map((h) => h.text()),
    ).toEqual(['implement']);
    expect(wrapper.find('[data-testid="dry-run"]').exists()).toBe(true);
  });

  it.each<[string, string, string[], string]>([
    [
      'Rules',
      '[data-testid="rule"] h3',
      [
        'implement-review-feedback',
        'merge-approved',
        'review-factory-prs',
        'triage-maintainer-issues',
        'triage-outside-issues',
      ],
      'implement-review-feedback',
    ],
    ['RuleSets', '[data-testid="ruleset"] > h3', ['api', 'docs'], 'api'],
    [
      'Templates',
      '[data-testid="template"] h3',
      ['needs-human'],
      'needs-human',
    ],
    [
      'Providers',
      '[data-testid="provider"] h3',
      ['github-public', 'github-enterprise', 'slack-ccf'],
      'github-public',
    ],
    [
      'Queues and Pools',
      '[data-testid="catalog-queue"] h3',
      [
        'default',
        'nightly',
        'review',
        'triage',
        'ci-ephemeral',
        'maintainer-agents',
      ],
      'Queue default',
    ],
    [
      'Groups',
      '[data-testid="group"] h3',
      ['maintainers', 'readers'],
      'maintainers',
    ],
  ])(
    'the %s tab lists its objects and shows the first',
    async (label, selector, names, first) => {
      const wrapper = await mountCatalog();
      await openTab(wrapper, label);
      expect(listed(wrapper)).toEqual(
        names.map((n, i) => (i === 0 ? `*${n}` : n)),
      );
      expect(wrapper.findAll(selector).map((e) => e.text())).toEqual([first]);
      expect(wrapper.find('[data-testid="taskspec"]').exists()).toBe(false);
      expect(wrapper.router.currentRoute.value.query.tab).toBeTruthy();
    },
  );

  it('keeps the picked object in the URL, and drops it with the tab', async () => {
    const wrapper = await mountCatalog(maintainer, '/catalog?tab=rules');
    await wrapper.findAll('[data-testid="list-item"]')[1]!.trigger('click');
    await flushPromises();
    expect(wrapper.router.currentRoute.value.query).toEqual({
      tab: 'rules',
      item: 'merge-approved',
    });
    expect(wrapper.get('[data-testid="rule"] h3').text()).toBe(
      'merge-approved',
    );
    expect(listed(wrapper)[1]).toBe('*merge-approved');
    await openTab(wrapper, 'Groups');
    expect(wrapper.router.currentRoute.value.query).toEqual({ tab: 'groups' });
    mounted!.unmount();
    // An item the tab doesn't list shows the first.
    const again = await mountCatalog(
      maintainer,
      '/catalog?tab=rules&item=gone',
    );
    expect(again.get('[data-testid="rule"] h3').text()).toBe(
      'implement-review-feedback',
    );
  });

  it('opens a pool by its id', async () => {
    const wrapper = await mountCatalog(
      maintainer,
      '/catalog?tab=queues&item=Pool/ci-ephemeral',
    );
    expect(wrapper.get('[data-testid="catalog-pool"] h3').text()).toBe(
      'Pool ci-ephemeral',
    );
  });

  it("opens a TaskSpec's prompt from its card, highlighted", async () => {
    const wrapper = await mountCatalog(maintainer, '/catalog?item=triage');
    const triage = wrapper.get('[data-testid="taskspec"]');
    expect(triage.get('h3').text()).toBe('triage');
    await triage.get('[data-testid="taskspec-prompt"] a').trigger('click');
    await flushPromises();
    expect(wrapper.router.currentRoute.value.query).toEqual({
      tab: 'prompts',
      file: 'prompts/triage.md.tmpl',
    });
    expect(wrapper.get('[role="tab"][aria-selected="true"]').text()).toBe(
      'Prompts2',
    );
    const view = wrapper.get('[data-testid="catalog-file-view"]');
    expect(view.get('h3').text()).toBe('prompts/triage.md.tmpl');
    expect(view.text()).toContain('Used by triage');
    await flushPromises();
    expect(view.find('[data-testid="markdown"] h1').exists()).toBe(true);
  });

  it('shows the first result schema, and another one picked', async () => {
    const wrapper = await mountCatalog(maintainer, '/catalog?tab=schemas');
    const view = () => wrapper.get('[data-testid="catalog-file-view"]');
    expect(view().get('h3').text()).toBe('schemas/results/implement.json');
    expect(view().find('pre [data-kind="key"]').text()).toBe('"$schema"');
    await wrapper.findAll('[data-testid="list-item"]')[1]!.trigger('click');
    await flushPromises();
    expect(view().get('h3').text()).toBe('schemas/results/triage.json');
    expect(view().text()).toContain('Used by triage');
  });

  it('resyncs the catalog and offers to reload it', async () => {
    const bodies = recordCommands();
    const wrapper = await mountCatalog();
    await wrapper
      .findAll('button')
      .find((b) => b.text() === 'Resync catalog')!
      .trigger('click');
    await flushPromises();
    await confirmDialog('Resync');
    expect(bodies).toEqual([{ type: 'resync-catalog' }]);
    const notice = wrapper.get('[data-testid="resync-requested"]');
    expect(notice.text()).toContain('Resync requested');
    await notice.get('button').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="resync-requested"]').exists()).toBe(
      false,
    );
  });

  it('shows the open prompt again after a resync reload', async () => {
    const wrapper = await mountCatalog(maintainer, '/catalog?tab=prompts');
    await wrapper
      .findAll('button')
      .find((b) => b.text() === 'Resync catalog')!
      .trigger('click');
    await flushPromises();
    await confirmDialog('Resync');
    await wrapper
      .get('[data-testid="resync-requested"] button')
      .trigger('click');
    await flushPromises();
    const view = wrapper.get('[data-testid="catalog-file-view"]');
    expect(view.get('h3').text()).toBe('prompts/implement.md.tmpl');
    await flushPromises();
    expect(view.find('[data-testid="markdown"] h1').exists()).toBe(true);
    expect(view.find('[role="status"]').exists()).toBe(false);
  });

  it('gives readers everything but the controls', async () => {
    const wrapper = await mountCatalog(reader, '/catalog?tab=rulesets');
    expect(listed(wrapper)).toEqual(['*api', 'docs']);
    expect(wrapper.findAll('[data-testid="ruleset"]')).toHaveLength(1);
    expect(wrapper.find('[data-testid="pause-controls"]').exists()).toBe(false);
    expect(
      wrapper.findAll('button').some((b) => b.text() === 'Resync catalog'),
    ).toBe(false);
    // The dry run changes nothing, so readers have it.
    expect(wrapper.find('[data-testid="dry-run-submit"]').exists()).toBe(true);
  });

  it('says what failed to load', async () => {
    server.use(
      http.get('/api/v1/catalog', () =>
        HttpResponse.json(
          {
            title: 'Service Unavailable',
            status: 503,
            detail: 'no catalog yet',
          },
          { status: 503 },
        ),
      ),
      http.get('/api/v1/providers', () => HttpResponse.json({ items: [] })),
    );
    const wrapper = await mountCatalog(maintainer, '/catalog?tab=providers');
    expect(wrapper.get('[role="alert"]').text()).toBe(
      "Couldn't load the catalog: Service Unavailable: no catalog yet",
    );
    expect(wrapper.text()).toContain('No providers.');
    await openTab(wrapper, 'RuleSets');
    expect(wrapper.find('[data-testid="ruleset"]').exists()).toBe(false);
  });
});
