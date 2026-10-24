import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { recordCommands } from '@/__tests__/commands';
import { mountWithApp } from '@/__tests__/mount';
import { catalogSummary } from '@/mocks/catalog';
import { maintainer, reader } from '@/mocks/session';
import CatalogStatus from './CatalogStatus.vue';

let mounted: VueWrapper | undefined;

async function mountStatus(
  summary: Schemas['CatalogSummary'] = catalogSummary,
  me = maintainer,
) {
  const { wrapper } = await mountWithApp(CatalogStatus, {
    me,
    props: { summary },
    attachTo: document.body,
  });
  mounted = wrapper;
  return wrapper;
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

describe('CatalogStatus', () => {
  it('shows the commit, status, source and what the catalog holds', async () => {
    const wrapper = await mountStatus();
    const sha = wrapper.get('[data-testid="catalog-sha"]');
    expect(sha.text()).toBe('9f1e2d3c4b5a');
    expect(sha.attributes('title')).toBe(catalogSummary.sha);
    expect(wrapper.text()).toContain('on main');
    expect(wrapper.get('[data-testid="catalog-status"]').text()).toBe('loaded');
    expect(wrapper.get('a').attributes('href')).toBe(
      'https://github.com/compliance-framework/factory',
    );
    expect(wrapper.text()).toContain('2026-10-16 08:00:05 UTC');
    expect(wrapper.get('[data-testid="catalog-counts"]').text()).toBe(
      '3 Provider, 2 Group, 1 Template, 2 RuleSet, 5 Rule, 3 TaskSpec, 4 Queue, 2 Pool',
    );
    expect(wrapper.find('[data-testid="catalog-errors"]').exists()).toBe(false);
  });

  it('shows load errors, and that the last good catalog still serves', async () => {
    const wrapper = await mountStatus({
      ...catalogSummary,
      repo: '',
      status: 'error',
      errors: [
        {
          code: 'CAT101',
          file: 'rules/x.yaml',
          line: 3,
          message: 'unknown field',
        },
        { message: '<b>not html</b>' },
      ],
    });
    expect(wrapper.text()).toContain('serving the last good catalog');
    expect(wrapper.text()).toContain('local directory');
    const errors = wrapper.findAll('[data-testid="catalog-errors"] li');
    expect(
      errors.map((e) => e.findAll('code, span').map((part) => part.text())),
    ).toEqual([
      ['CAT101', 'rules/x.yaml:3', 'unknown field'],
      ['<b>not html</b>'],
    ]);
    expect(wrapper.find('b').exists()).toBe(false);
  });

  it('resyncs the catalog when a maintainer confirms', async () => {
    const bodies = recordCommands();
    const wrapper = await mountStatus();
    await wrapper.get('button').trigger('click');
    await flushPromises();
    [
      ...document.body.querySelectorAll<HTMLButtonElement>(
        '[role="dialog"] button',
      ),
    ]
      .find((b) => b.textContent?.trim() === 'Resync')!
      .click();
    await flushPromises();
    expect(bodies).toEqual([{ type: 'resync-catalog' }]);
    expect(wrapper.emitted('resynced')).toEqual([[{ type: 'resync-catalog' }]]);
  });

  it('offers readers no resync', async () => {
    const wrapper = await mountStatus(catalogSummary, reader);
    expect(wrapper.find('button').exists()).toBe(false);
  });
});
