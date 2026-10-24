/**
 * Markdown → sanitised HTML, for catalog prompts (web LLD §Non-functional: Markdown only through
 * a sanitising renderer). markdown-it runs with `html: false`, so raw HTML in the source is
 * escaped, and links other than https:, http: and mailto: are not made; DOMPurify then cleans
 * its output again: no images, only those URL schemes, and every link opens in a new tab with
 * `rel="noopener noreferrer"`. Both libraries load on first use (a dynamic import), so they stay
 * out of the initial bundle.
 */

const SAFE_URL = /^(?:https?|mailto):/i;

type Render = (source: string) => string;

let renderer: Promise<Render> | undefined;

async function load(): Promise<Render> {
  const [{ default: MarkdownIt }, { default: createDOMPurify }] =
    await Promise.all([import('markdown-it'), import('dompurify')]);
  const md = new MarkdownIt({
    html: false,
    linkify: false,
    typographer: false,
  });
  md.validateLink = (url) => SAFE_URL.test(url.trim());
  const purify = createDOMPurify(window);
  purify.addHook('afterSanitizeAttributes', (node) => {
    if (node.tagName === 'A') {
      node.setAttribute('target', '_blank');
      node.setAttribute('rel', 'noopener noreferrer');
    }
  });
  return (source) =>
    purify.sanitize(md.render(source), {
      FORBID_TAGS: ['img', 'style', 'form', 'input', 'button'],
      ALLOWED_URI_REGEXP: SAFE_URL,
      ADD_ATTR: ['target'],
    });
}

/** The source as sanitised HTML: safe to give `v-html`. */
export async function renderMarkdown(source: string): Promise<string> {
  renderer ??= load();
  return (await renderer)(source);
}
