import { beforeAll, describe, expect, it } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import { renderMarkdown } from '@/components/shared/markdown';
import CatalogFiles from './CatalogFiles.vue';

beforeAll(async () => {
  await renderMarkdown('');
});

const prompt = examples.get('getCatalogFile') as Schemas['CatalogFile'];
const files: Schemas['CatalogFileRef'][] = [
  { path: 'prompts/implement.md.tmpl', kind: 'prompt' },
  { path: 'prompts/triage.md.tmpl', kind: 'prompt' },
];

function mountFiles(
  props: Partial<InstanceType<typeof CatalogFiles>['$props']>,
) {
  return mount(CatalogFiles, {
    props: {
      files,
      selected: '',
      file: undefined,
      error: undefined,
      usedBy: { 'prompts/triage.md.tmpl': ['triage'] },
      label: 'Prompts',
      ...props,
    },
  });
}

describe('CatalogFiles', () => {
  it('lists the files and picks one', async () => {
    const wrapper = mountFiles({});
    const buttons = wrapper.findAll('[data-testid="list-item"]');
    expect(buttons.map((b) => b.find('span').text())).toEqual(
      files.map((f) => f.path),
    );
    // The secondary line says which TaskSpecs use it.
    expect(
      buttons.map((b) => b.get('[data-testid="list-item-secondary"]').text()),
    ).toEqual(['unused', 'used by triage']);
    expect(wrapper.find('[data-testid="catalog-file-view"]').exists()).toBe(
      false,
    );
    await buttons[1]!.trigger('click');
    expect(wrapper.emitted('select')).toEqual([['prompts/triage.md.tmpl']]);
  });

  it('shows a prompt rendered by default, its source on the toggle', async () => {
    const wrapper = mountFiles({ selected: prompt.path, file: prompt });
    await flushPromises();
    const modes = wrapper.findAll('[data-mode]');
    expect(modes.map((m) => [m.text(), m.attributes('aria-pressed')])).toEqual([
      ['Rendered', 'true'],
      ['Source', 'false'],
    ]);
    expect(wrapper.get('[data-testid="markdown"] h1').text()).toBe(
      'Task: {{ .task.spec }} for {{ .task.target.repo }}',
    );
    expect(wrapper.find('pre').exists()).toBe(false);
    await modes[1]!.trigger('click');
    expect(modes[1]!.attributes('aria-pressed')).toBe('true');
    expect(wrapper.find('[data-testid="markdown"]').exists()).toBe(false);
    expect(wrapper.find('[aria-current="true"] span').text()).toBe(
      'prompts/triage.md.tmpl',
    );
    expect(
      wrapper
        .get('[data-testid="catalog-file-users"]')
        .text()
        .replace(/\s+/g, ' '),
    ).toBe(`Used by triage · ${prompt.digest}`);
    const pre = wrapper.get('pre');
    expect(pre.text()).toBe(prompt.content.trimEnd());
    // A schema has no toggle.
    expect(pre.findAll('[data-kind="delim"]').length).toBeGreaterThan(0);
    expect(pre.find('[data-kind="comment"]').text()).toBe(
      '{{- /* triage: classify an incoming issue. */ -}}',
    );
  });

  it('shows a schema as highlighted JSON', () => {
    const schema: Schemas['CatalogFile'] = {
      path: 'schemas/results/x.json',
      kind: 'schema',
      digest: 'sha256:00',
      content: '{ "type": "object" }',
    };
    const wrapper = mountFiles({
      files: [{ path: schema.path, kind: 'schema' }],
      selected: schema.path,
      file: schema,
    });
    expect(wrapper.find('[data-mode]').exists()).toBe(false);
    expect(
      wrapper.findAll('pre [data-kind="key"]').map((s) => s.text()),
    ).toEqual(['"type"']);
    expect(wrapper.get('[data-testid="catalog-file-users"]').text()).toContain(
      'no TaskSpec',
    );
  });

  it('says why a file did not load, and that one is loading', () => {
    expect(
      mountFiles({ selected: 'prompts/implement.md.tmpl', error: 'Not Found' })
        .get('[role="alert"]')
        .text(),
    ).toBe("Couldn't load prompts/implement.md.tmpl: Not Found");
    expect(
      mountFiles({ selected: 'prompts/implement.md.tmpl' })
        .get('[role="status"]')
        .text(),
    ).toBe('Loading…');
  });
});
