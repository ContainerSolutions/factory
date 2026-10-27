import { describe, expect, it } from 'vitest';
import { mount, RouterLinkStub } from '@vue/test-utils';
import { taskSpecs } from '@/mocks/catalog';
import TaskSpecCard from './TaskSpecCard.vue';

function mountCard(name: string) {
  const taskSpec = taskSpecs.find((t) => t.metadata.name === name)!;
  return mount(TaskSpecCard, {
    props: { taskSpec },
    global: { stubs: { RouterLink: RouterLinkStub } },
  });
}

function text(wrapper: ReturnType<typeof mountCard>, id: string): string {
  return wrapper.get(`[data-testid="${id}"]`).text().replace(/\s+/g, ' ');
}

describe('TaskSpecCard', () => {
  it('shows the triage TaskSpec readably, with links to its prompt and schema', () => {
    const wrapper = mountCard('triage');
    expect(wrapper.get('h3').text()).toBe('triage');
    expect(text(wrapper, 'taskspec-harness')).toBe(
      'codex (gpt-5*, effort medium) allowed codex: gpt-5* · effort low | medium | high allowed claude-code: claude-{sonnet,haiku}-5-* · effort *',
    );
    expect(text(wrapper, 'taskspec-skills')).toBe(
      'ccf-skills@v0.7.0: bugreport',
    );
    expect(text(wrapper, 'taskspec-permissions')).toBe(
      'contents: read, issues: write',
    );
    expect(text(wrapper, 'taskspec-effects')).toBe(
      'comment, label.add, label.remove, link-duplicate, create-issue labels type/*, area/*, needs-info',
    );
    expect(text(wrapper, 'taskspec-limits')).toBe('40 turns, 15m, $1.50');
    const links = wrapper.findAllComponents(RouterLinkStub);
    expect(links.map((l) => [l.text(), l.props('to')])).toEqual([
      [
        'prompts/triage.md.tmpl',
        {
          name: 'catalog',
          query: { tab: 'prompts', file: 'prompts/triage.md.tmpl' },
        },
      ],
      [
        'schemas/results/triage.json',
        {
          name: 'catalog',
          query: { tab: 'schemas', file: 'schemas/results/triage.json' },
        },
      ],
    ]);
    expect(text(wrapper, 'taskspec-output')).toContain('promote');
  });

  it('shows inputs, a CEL repo ref and the environment', () => {
    const wrapper = mountCard('implement');
    const rows = wrapper
      .findAll('[data-testid="taskspec-inputs"] tbody tr')
      .map((r) => r.findAll('th, td').map((c) => c.text()));
    expect(rows).toEqual([
      ['base', 'string', 'any', 'main'],
      ['issue', 'string', 'any', '—'],
      ['mode', 'string', 'new, address-review', 'new'],
    ]);
    const workspace = wrapper.get('[data-testid="taskspec-workspace"]');
    expect(workspace.get('code[aria-label="Repo"]').text()).toBe(
      "=task.target.repo + '@' + inputs.base",
    );
    expect(
      workspace
        .findAll('code[aria-label="Repo"] [data-kind="field"]')
        .map((s) => s.text()),
    ).toEqual(['target', 'repo', 'base']);
    expect(workspace.text()).toContain('rw');
    expect(workspace.get('pre').text()).toBe(
      'setup: |\n  corepack enable\n  yarn install --immutable\nexposes:\n  FACTORY_STACK_URL: http://localhost:8000',
    );
  });

  it('says what a bare TaskSpec lacks', () => {
    const wrapper = mountCard('review');
    expect(text(wrapper, 'taskspec-prompt')).toBe('none');
    expect(text(wrapper, 'taskspec-harness')).toBe('the agent picks');
    expect(text(wrapper, 'taskspec-limits')).toBe('none');
    expect(text(wrapper, 'taskspec-output')).toBe('the base Result');
    expect(wrapper.find('[data-testid="taskspec-workspace"]').exists()).toBe(
      false,
    );
  });
});
