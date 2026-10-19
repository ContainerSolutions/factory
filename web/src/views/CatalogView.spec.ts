import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { http, HttpResponse } from 'msw';
import { confirmDialog, recordCommands } from '@/__tests__/commands';
import { mountWithApp } from '@/__tests__/mount';
import { server } from '@/mocks/node';
import { maintainer, reader } from '@/mocks/session';
import CatalogView from './CatalogView.vue';

let mounted: VueWrapper | undefined;

async function mountCatalog(me = maintainer) {
  const { wrapper } = await mountWithApp(CatalogView, {
    path: '/catalog',
    me,
    attachTo: document.body,
  });
  mounted = wrapper;
  return wrapper;
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

describe('CatalogView', () => {
  it('shows the commit, providers, RuleSets, TaskSpecs, queues, pools and the dry run', async () => {
    const wrapper = await mountCatalog();
    expect(wrapper.get('h1').text()).toBe('Catalog');
    expect(wrapper.get('[data-testid="catalog-sha"]').text()).toBe(
      '9f1e2d3c4b5a',
    );
    expect(
      wrapper.findAll('[data-testid="provider"] th').map((t) => t.text()),
    ).toEqual(['github-public', 'github-enterprise', 'slack-ccf']);
    expect(
      wrapper.findAll('[data-testid="ruleset"] h3').map((h) => h.text()),
    ).toEqual(['api', 'docs']);
    expect(wrapper.findAll('[data-testid="taskspec"]')).toHaveLength(3);
    expect(wrapper.findAll('[data-testid="catalog-queue"]')).toHaveLength(4);
    expect(wrapper.findAll('[data-testid="catalog-pool"]')).toHaveLength(2);
    expect(wrapper.get('[data-testid="groups"]').text()).toBe(
      'maintainers, readers',
    );
    expect(wrapper.find('[data-testid="dry-run"]').exists()).toBe(true);
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

  it('gives readers everything but the controls', async () => {
    const wrapper = await mountCatalog(reader);
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
    const wrapper = await mountCatalog();
    expect(wrapper.get('[role="alert"]').text()).toBe(
      "Couldn't load the catalog: Service Unavailable: no catalog yet",
    );
    expect(wrapper.text()).toContain('No providers.');
    expect(wrapper.find('[data-testid="ruleset"]').exists()).toBe(false);
  });
});
