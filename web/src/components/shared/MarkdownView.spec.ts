import { beforeAll, describe, expect, it } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import MarkdownView from './MarkdownView.vue';
import { renderMarkdown } from './markdown';

// The renderer loads with a dynamic import: load it once, so each mount renders in a tick.
beforeAll(async () => {
  await renderMarkdown('');
});

async function rendered(source: string) {
  const wrapper = mount(MarkdownView, { props: { source, label: 'Prompt' } });
  await flushPromises();
  return wrapper.get('[data-testid="markdown"]');
}

describe('MarkdownView', () => {
  it('renders headings, lists and code, and keeps template actions as text', async () => {
    const md = await rendered(
      '# Task: {{ .task.spec }}\n\n- one\n- two\n\nRun `make test`:\n\n```sh\nmake {{ .target }}\n```\n',
    );
    expect(md.get('h1').text()).toBe('Task: {{ .task.spec }}');
    expect(md.findAll('ul > li').map((l) => l.text())).toEqual(['one', 'two']);
    expect(md.get('p code').text()).toBe('make test');
    expect(md.get('pre code').text()).toBe('make {{ .target }}');
    expect(md.attributes('aria-label')).toBe('Prompt');
  });

  it.each<[string, string]>([
    ['a script tag', '<script>window.pwned = 1</script>'],
    ['an onerror attribute', '<img src=x onerror="window.pwned = 1">'],
    [
      'an inline handler in HTML',
      '<a href="https://x.dev" onclick="window.pwned=1">x</a>',
    ],
  ])('escapes %s', async (_, source) => {
    const md = await rendered(source);
    expect(md.find('script').exists()).toBe(false);
    expect(md.find('img').exists()).toBe(false);
    expect(md.find('[onerror], [onclick]').exists()).toBe(false);
    // Raw HTML is shown as text, not run.
    expect(md.text()).toContain(source.slice(0, 6));
  });

  it.each<[string, string | undefined]>([
    ['[x](javascript:alert(1))', undefined],
    ['[x](data:text/html,hi)', undefined],
    ['[x](/relative)', undefined],
    ['[x](https://example.com/a)', 'https://example.com/a'],
    ['[x](mailto:a@b.dev)', 'mailto:a@b.dev'],
  ])('%s links to %s', async (source, href) => {
    const md = await rendered(source);
    const a = md.find('a');
    if (href === undefined) {
      expect(a.exists()).toBe(false);
      return;
    }
    expect(a.attributes()).toMatchObject({
      href,
      target: '_blank',
      rel: 'noopener noreferrer',
    });
  });

  it('drops images in Markdown', async () => {
    expect(
      await renderMarkdown('![alt](https://example.com/x.png)'),
    ).not.toContain('<img');
  });
});
