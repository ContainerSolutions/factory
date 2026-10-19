import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import { NOW } from '@/__tests__/mount';
import { agents, poolViews } from '@/mocks/pools';
import PoolCard from './PoolCard.vue';
import { poolGroups } from './pools';

const groups = poolGroups(poolViews, agents);

function rows(wrapper: ReturnType<typeof mount>) {
  const dl = wrapper.get('dl');
  const dts = dl.findAll(':scope > dt').map((d) => d.text());
  const dds = dl.findAll(':scope > dd').map((d) => d.text());
  return Object.fromEntries(dts.map((t, i) => [t, dds[i]]));
}

describe('PoolCard', () => {
  it('shows a registered pool: owners Group, lease policy, requirements and its agents', () => {
    const wrapper = mount(PoolCard, { props: { group: groups[0]!, now: NOW } });
    expect(wrapper.get('h3').text()).toBe('maintainer-agents');
    expect(wrapper.get('[data-testid="pool-mode"]').text()).toBe('registered');
    expect(rows(wrapper)).toEqual({
      Owners: 'members of the Group maintainers',
      Lease: 'ttl 2m · heartbeat every 30s · 1 per agent',
      Requires: 'any of claude-code, codex',
    });
    expect(
      wrapper.findAll('[data-testid="agent"] h4').map((h) => h.text()),
    ).toEqual([
      'agt_gusfcarvalho-mbp-01',
      'agt_octocat-linux-02',
      'agt_ccf-bot-builder-03',
    ]);
    expect(wrapper.find('[data-testid="pool-missing"]').exists()).toBe(false);
  });

  it('shows an ephemeral pool: backend, image and scaling, and no agents', () => {
    const wrapper = mount(PoolCard, { props: { group: groups[1]!, now: NOW } });
    expect(wrapper.get('h3').text()).toBe('ci-ephemeral');
    expect(wrapper.get('[data-testid="pool-mode"]').text()).toBe('ephemeral');
    expect(rows(wrapper)).toEqual({
      Lease: 'ttl 5m · heartbeat every 1m · 1 per agent',
      Backend: 'github-actions: compliance-framework/factory-runners agent.yml',
      Image: 'ghcr.io/compliance-framework/factory-agent:v0.1.0',
      Scaling: '0 to 4 agents',
    });
    expect(wrapper.text()).toContain('No agents.');
  });

  it('never shows the secret refs: the join token or the LLM API key', () => {
    for (const group of groups) {
      const html = mount(PoolCard, { props: { group, now: NOW } }).html();
      expect(html).not.toContain('join-token');
      expect(html).not.toContain('tokenRef');
      expect(html).not.toContain('ci-llm');
      expect(html).not.toContain('api-key');
    }
  });

  it("lists agent names the agents list didn't return, and a pool that isn't listed", () => {
    const missing = mount(PoolCard, {
      props: {
        group: { ...groups[0]!, agents: [], missing: ['agt_gone'] },
        now: NOW,
      },
    });
    expect(missing.get('[data-testid="pool-missing"]').text()).toContain(
      'agt_gone',
    );

    const unlisted = mount(PoolCard, {
      props: {
        group: { name: 'gone', agents: [agents[0]!], missing: [] },
        now: NOW,
      },
    });
    expect(unlisted.get('h3').text()).toBe('gone');
    expect(unlisted.get('[data-testid="pool-mode"]').text()).toBe('not listed');
    expect(unlisted.text()).toContain("it isn't listed");
    expect(unlisted.findAll('[data-testid="agent"]')).toHaveLength(1);
  });
});
