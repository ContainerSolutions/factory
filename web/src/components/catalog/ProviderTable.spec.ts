import { afterEach, describe, expect, it } from 'vitest';
import type { VueWrapper } from '@vue/test-utils';
import { mountWithApp } from '@/__tests__/mount';
import { providerViews } from '@/mocks/catalog';
import { maintainer, reader } from '@/mocks/session';
import ProviderTable from './ProviderTable.vue';

let mounted: VueWrapper | undefined;

async function mountTable(me = maintainer) {
  const { wrapper } = await mountWithApp(ProviderTable, {
    me,
    props: { providers: [...providerViews] },
  });
  mounted = wrapper;
  return wrapper;
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

describe('ProviderTable', () => {
  it('shows each provider with its type, labels, scope and health', async () => {
    const wrapper = await mountTable();
    const rows = wrapper.findAll('[data-testid="provider"]').map((r) =>
      r
        .findAll('th, td')
        .slice(0, 5)
        .map((c) => c.text()),
    );
    expect(rows).toEqual([
      [
        'github-public',
        'github',
        'family=github, network=public',
        'org compliance-framework',
        expect.stringMatching(/^ok\s*checked 2026-10-16 09:29:00 UTC$/),
      ],
      [
        'github-enterprise',
        'github',
        'family=github, network=enterprise',
        'org platform',
        expect.stringMatching(/^degraded\s*rate limited until 12:30 UTC/),
      ],
      [
        'slack-ccf',
        'slack',
        'family=slack',
        'everything it can see',
        expect.stringMatching(/^ok/),
      ],
    ]);
  });

  it('never renders secret refs', async () => {
    const text = (await mountTable()).text();
    for (const secret of [
      'github-public-webhook',
      'ccf-factory-app',
      'private-key',
      'app-token',
      'bot-token',
    ]) {
      expect(text).not.toContain(secret);
    }
  });

  it('offers pause and resume per provider to maintainers only', async () => {
    const wrapper = await mountTable();
    expect(
      wrapper
        .findAll('button')
        .map((b) => b.attributes('aria-label'))
        .slice(0, 2),
    ).toEqual([
      'Pause provider github-public',
      'Resume provider github-public',
    ]);
    mounted!.unmount();
    expect((await mountTable(reader)).find('button').exists()).toBe(false);
  });
});
