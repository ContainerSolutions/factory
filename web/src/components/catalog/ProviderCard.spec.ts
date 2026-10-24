import { afterEach, describe, expect, it } from 'vitest';
import type { VueWrapper } from '@vue/test-utils';
import { mountWithApp } from '@/__tests__/mount';
import { providerViews } from '@/mocks/catalog';
import { maintainer, reader } from '@/mocks/session';
import ProviderCard from './ProviderCard.vue';

let mounted: VueWrapper | undefined;

async function mountCard(i: number, me = maintainer) {
  const { wrapper } = await mountWithApp(ProviderCard, {
    me,
    props: { view: providerViews[i]! },
  });
  mounted = wrapper;
  return wrapper;
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

describe('ProviderCard', () => {
  it.each<[number, string[]]>([
    [
      0,
      [
        'github-public',
        'github',
        'family=github, network=public',
        'org compliance-framework',
        'ok checked 2026-10-16 09:29:00 UTC',
      ],
    ],
    [
      1,
      [
        'github-enterprise',
        'github',
        'family=github, network=enterprise',
        'org platform',
        expect.stringMatching(/^degraded\s*rate limited until 12:30 UTC/),
      ],
    ],
    [
      2,
      [
        'slack-ccf',
        'slack',
        'family=slack',
        'everything it can see',
        expect.stringMatching(/^ok/),
      ],
    ],
  ])(
    'shows provider %i with its type, labels, scope and health',
    async (i, want) => {
      const wrapper = await mountCard(i);
      const text = (id: string) =>
        wrapper.get(`[data-testid="${id}"]`).text().replace(/\s+/g, ' ');
      expect([
        wrapper.get('h3').text(),
        text('provider-type'),
        text('provider-labels'),
        text('provider-scope'),
        text('provider-health'),
      ]).toEqual(want);
    },
  );

  it('never renders secret refs', async () => {
    for (const i of [0, 1, 2]) {
      const text = (await mountCard(i)).text();
      for (const secret of [
        'github-public-webhook',
        'ccf-factory-app',
        'private-key',
        'app-token',
        'bot-token',
      ]) {
        expect(text).not.toContain(secret);
      }
      mounted!.unmount();
      mounted = undefined;
    }
  });

  it('offers pause and resume to maintainers only', async () => {
    const wrapper = await mountCard(0);
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
    expect(
      (await mountCard(0, reader))
        .find('[data-testid="pause-controls"]')
        .exists(),
    ).toBe(false);
  });
});
