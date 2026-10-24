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
    expect(
      wrapper.findAll('[data-testid="taskspec"] h3').map((h) => h.text()),
    ).toEqual(['implement', 'review', 'triage']);
    expect(wrapper.find('[data-testid="dry-run"]').exists()).toBe(true);
  });

  it.each<[string, string, string[]]>([
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
    ],
    ['RuleSets', '[data-testid="ruleset"] h3', ['api', 'docs']],
    ['Templates', '[data-testid="template"] h3', ['needs-human']],
    [
      'Providers',
      '[data-testid="provider"] th',
      ['github-public', 'github-enterprise', 'slack-ccf'],
    ],
    [
      'Queues and Pools',
      '[data-testid="catalog-pool"] th',
      ['ci-ephemeral', 'maintainer-agents'],
    ],
    ['Groups', '[data-testid="group"] h3', ['maintainers', 'readers']],
  ])('the %s tab shows its objects', async (label, selector, want) => {
    const wrapper = await mountCatalog();
    await openTab(wrapper, label);
    expect(wrapper.findAll(selector).map((e) => e.text())).toEqual(want);
    expect(wrapper.find('[data-testid="taskspec"]').exists()).toBe(false);
    expect(wrapper.router.currentRoute.value.query.tab).toBeTruthy();
  });

  it("opens a TaskSpec's prompt from its card, highlighted", async () => {
    const wrapper = await mountCatalog();
    const triage = wrapper
      .findAll('[data-testid="taskspec"]')
      .find((c) => c.get('h3').text() === 'triage')!;
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
    await wrapper.findAll('[data-testid="catalog-file"]')[1]!.trigger('click');
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
    expect(wrapper.findAll('[data-testid="ruleset"]')).toHaveLength(2);
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
