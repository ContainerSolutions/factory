import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { NOW } from '@/__tests__/mount';
import { agents, poolViews } from '@/mocks/pools';
import { liveRun } from '@/mocks/workitems';
import AgentCard from './AgentCard.vue';

const [laptop, throttled, offline] = agents as [
  Schemas['Agent'],
  Schemas['Agent'],
  Schemas['Agent'],
];
const pool = poolViews[0]!.pool;

function mountAgent(agent: Schemas['Agent'], p = pool) {
  return mount(AgentCard, { props: { agent, pool: p, now: NOW } });
}

/** The element's text, its children's joined with spaces as they are laid out. */
const text = (wrapper: ReturnType<typeof mount>, id: string) =>
  [...wrapper.get(`[data-testid="${id}"]`).element.childNodes]
    .filter((n) => n.nodeType !== Node.COMMENT_NODE)
    .map((n) => n.textContent ?? '')
    .join(' ')
    .replace(/\s+/g, ' ')
    .trim();

function lending(wrapper: ReturnType<typeof mount>) {
  const box = wrapper.get('[data-testid="agent-lending"]');
  const dts = box.findAll('dt').map((d) => d.text());
  const dds = box.findAll('dd').map((d) => d.text());
  return Object.fromEntries(dts.map((t, i) => [t, dds[i]]));
}

describe('AgentCard', () => {
  it('shows a busy agent: owner, labels, capabilities, heartbeat, leases, usage and lending', () => {
    const wrapper = mountAgent(laptop);
    expect(wrapper.get('h4').text()).toBe('agt_gusfcarvalho-mbp-01');
    expect(text(wrapper, 'agent-phase')).toBe('Busy');
    expect(text(wrapper, 'agent-owner')).toBe('github-public:gusfcarvalho');
    expect(text(wrapper, 'agent-labels')).toBe('owner=gusfcarvalho');
    expect(text(wrapper, 'agent-capabilities')).toBe('has-local-stack macos');
    expect(text(wrapper, 'agent-heartbeat')).toBe('on time 20 s ago');
    expect(text(wrapper, 'agent-running')).toBe(`1 of 1 ${liveRun.spec.task}`);
    expect(text(wrapper, 'agent-today')).toBe(
      '4 of 30 runs · 430,200 of 3,000,000 tokens',
    );
    expect(wrapper.find('[data-testid="agent-throttle"]').exists()).toBe(false);
    expect(lending(wrapper)).toEqual({
      Queues: 'default, triage, review',
      RuleSets: 'docs',
      Harnesses: 'codex, claude-code',
      'Max concurrent': '1',
      'Runs a day': '30',
      'Tokens a day': '3,000,000',
      Hours: 'Mon-Fri 08:00-20:00 Europe/Lisbon',
    });
    expect(wrapper.findAll('[data-testid="harness-offer"]')).toHaveLength(2);
  });

  it('shows the throttle window of a throttled agent and its daily caps', () => {
    const wrapper = mountAgent(throttled);
    expect(text(wrapper, 'agent-phase')).toBe('Throttled');
    expect(text(wrapper, 'agent-throttle')).toBe(
      'until 2026-10-19 13:30:00 UTC (in 1 h 30 min)',
    );
    expect(text(wrapper, 'agent-running')).toBe('0 of 1');
    expect(text(wrapper, 'agent-today')).toBe(
      '9 of 10 runs · 980,000 of 1,000,000 tokens',
    );
    expect(lending(wrapper)).toEqual({
      Queues: 'default',
      RuleSets: 'any',
      Harnesses: 'codex',
      'Max concurrent': '1',
      'Runs a day': '10',
      'Tokens a day': '1,000,000',
      Hours: 'any time',
    });
  });

  it('says when a throttle window has passed', () => {
    const wrapper = mountAgent({
      ...throttled,
      status: {
        ...throttled.status!,
        throttledUntil: '2026-10-19T11:00:00Z',
      },
    });
    expect(text(wrapper, 'agent-throttle')).toBe(
      'until 2026-10-19 11:00:00 UTC (passed)',
    );
  });

  it('shows an offline agent with a stale heartbeat and unlimited lending, with no caps', () => {
    const wrapper = mountAgent(offline);
    expect(text(wrapper, 'agent-phase')).toBe('Offline');
    expect(text(wrapper, 'agent-heartbeat')).toBe('stale 13 h 50 min ago');
    expect(
      wrapper
        .get('[data-testid="agent-heartbeat"] [data-tone]')
        .attributes('data-tone'),
    ).toBe('danger');
    expect(text(wrapper, 'agent-labels')).toBe('owner=ccf-bot site=lisbon');
    expect(text(wrapper, 'agent-today')).toBe('0 runs · 0 tokens');
    expect(text(wrapper, 'agent-lending')).toMatch(/^Unlimited/);
  });

  it('shows an agent with no lending, labels or status', () => {
    const bare: Schemas['Agent'] = {
      kind: 'Agent',
      metadata: { name: 'agt_bare' },
      spec: { pool: 'p', owner: 'github-public:someone', capabilities: {} },
    };
    const wrapper = mountAgent(bare, undefined);
    expect(text(wrapper, 'agent-phase')).toBe('no status');
    expect(text(wrapper, 'agent-heartbeat')).toBe('never');
    expect(text(wrapper, 'agent-running')).toBe('0 of 1');
    expect(text(wrapper, 'agent-today')).toBe('0 runs · 0 tokens');
    expect(text(wrapper, 'agent-lending')).toBe(
      'Lends nothing: no lending is set.',
    );
    expect(wrapper.find('[data-testid="agent-labels"]').exists()).toBe(false);
    expect(wrapper.text()).toContain('No harness offers.');
  });

  it('renders what the agent reported as text, never HTML', () => {
    const wrapper = mountAgent({
      ...laptop,
      metadata: { name: 'agt_x', labels: { note: '<img src=x>' } },
      spec: {
        ...laptop.spec,
        owner: '<b>owner</b>',
        lending: { queues: ['default'], hours: '<i>always</i>' },
      },
    });
    expect(text(wrapper, 'agent-owner')).toBe('<b>owner</b>');
    expect(text(wrapper, 'agent-labels')).toBe('note=<img src=x>');
    expect(lending(wrapper).Hours).toBe('<i>always</i>');
    expect(wrapper.find('b, i, img').exists()).toBe(false);
  });
});
