import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import CatalogFiles from './CatalogFiles.vue';

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
    const buttons = wrapper.findAll('[data-testid="catalog-file"]');
    expect(buttons.map((b) => b.text())).toEqual(files.map((f) => f.path));
    expect(wrapper.find('[data-testid="catalog-file-view"]').exists()).toBe(
      false,
    );
    await buttons[1]!.trigger('click');
    expect(wrapper.emitted('select')).toEqual([['prompts/triage.md.tmpl']]);
  });

  it('shows a prompt as its highlighted template, with its users and digest', () => {
    const wrapper = mountFiles({ selected: prompt.path, file: prompt });
    expect(wrapper.find('[aria-current="true"]').text()).toBe(
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
