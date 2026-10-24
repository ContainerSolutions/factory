/**
 * Small tokenisers for the catalog's languages: CEL (`where`, `=` values), Go text/template
 * (prompts, Template bodies), JSON (result schemas), and a YAML writer for any object. They only
 * split text into spans for colour; CodeBlock renders each span as text, never as HTML, so
 * catalog and provider text can't inject markup.
 */

export type TokenKind =
  | 'plain'
  | 'key'
  | 'string'
  | 'number'
  | 'keyword'
  | 'operator'
  | 'punct'
  | 'comment'
  | 'variable'
  | 'field'
  | 'function'
  | 'delim'
  | 'heading';

export interface Token {
  text: string;
  kind: TokenKind;
}

/** Appends a token, merging it into the previous one when the kind is the same. */
function push(out: Token[], text: string, kind: TokenKind): void {
  if (!text) return;
  const last = out[out.length - 1];
  if (last && last.kind === kind) last.text += text;
  else out.push({ text, kind });
}

/** The text of tokens, joined: what CodeBlock shows. */
export function tokensText(tokens: readonly Token[]): string {
  return tokens.map((t) => t.text).join('');
}

const CEL_KEYWORDS = new Set(['true', 'false', 'null', 'in']);

const CEL_RULES: [RegExp, TokenKind | 'ident'][] = [
  [/\s+/y, 'plain'],
  [/[rRbB]{0,2}(?:'''[\s\S]*?'''|"""[\s\S]*?""")/y, 'string'],
  [/[rRbB]{0,2}(?:'(?:\\.|[^'\\\n])*'|"(?:\\.|[^"\\\n])*")/y, 'string'],
  [
    /0x[0-9a-fA-F]+u?|\d+\.\d*(?:[eE][+-]?\d+)?|\.\d+(?:[eE][+-]?\d+)?|\d+(?:[eE][+-]?\d+)?u?/y,
    'number',
  ],
  [/[A-Za-z_][A-Za-z0-9_]*/y, 'ident'],
  [/&&|\|\||==|!=|<=|>=|[<>!+\-*/%?:]/y, 'operator'],
  [/[()[\]{},.]/y, 'punct'],
];

/** Runs sticky regex rules over src from `start`; returns the kind and the match, or null. */
function matchAt(
  rules: readonly [RegExp, string][],
  src: string,
  at: number,
): [string, string] | null {
  for (const [re, kind] of rules) {
    re.lastIndex = at;
    const m = re.exec(src);
    if (m && m[0]) return [kind, m[0]];
  }
  return null;
}

/** A CEL expression's tokens: identifiers after a `.` are fields, before a `(` functions. */
export function tokenizeCEL(src: string): Token[] {
  const out: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const m = matchAt(CEL_RULES, src, i);
    if (!m) {
      push(out, src[i]!, 'plain');
      i++;
      continue;
    }
    const [kind, text] = m;
    if (kind === 'ident') {
      const prev = out[out.length - 1];
      if (CEL_KEYWORDS.has(text)) push(out, text, 'keyword');
      else if (/^\s*\(/.test(src.slice(i + text.length))) {
        push(out, text, 'function');
      } else if (prev?.kind === 'punct' && prev.text.endsWith('.')) {
        push(out, text, 'field');
      } else push(out, text, 'variable');
    } else push(out, text, kind as TokenKind);
    i += text.length;
  }
  return out;
}

/** A catalog value: `=CEL` highlighted as CEL after its `=`, anything else a string. */
export function tokenizeValue(value: string): Token[] {
  if (!value.startsWith('=')) return [{ text: value, kind: 'string' }];
  return [{ text: '=', kind: 'operator' }, ...tokenizeCEL(value.slice(1))];
}

const TEMPLATE_KEYWORDS = new Set([
  'if',
  'else',
  'end',
  'range',
  'with',
  'define',
  'template',
  'block',
  'break',
  'continue',
  'nil',
  'true',
  'false',
]);

const TEMPLATE_RULES: [RegExp, TokenKind | 'ident'][] = [
  [/\s+/y, 'plain'],
  [/"(?:\\.|[^"\\\n])*"|`[^`]*`|'(?:\\.|[^'\\\n])+'/y, 'string'],
  [/-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/y, 'number'],
  [/\$[A-Za-z0-9_]*/y, 'variable'],
  [/(?:\.[A-Za-z_][A-Za-z0-9_]*)+|\./y, 'field'],
  [/[A-Za-z_][A-Za-z0-9_]*/y, 'ident'],
  [/:=|[=|]/y, 'operator'],
  [/[(),]/y, 'punct'],
];

/** The text outside actions: Markdown heading lines are headings, the rest plain. */
function templateText(
  out: Token[],
  text: string,
  state: { lineStart: boolean; heading: boolean },
): void {
  for (const piece of text.match(/[^\n]*\n?/g) ?? []) {
    if (!piece) continue;
    if (state.lineStart && /^#{1,6}\s/.test(piece)) state.heading = true;
    push(out, piece, state.heading ? 'heading' : 'plain');
    if (piece.endsWith('\n')) {
      state.lineStart = true;
      state.heading = false;
    } else state.lineStart = false;
  }
}

/**
 * A Go text/template's tokens: the text (with Markdown headings), each action's delimiters
 * (`{{`, `{{-`, `-}}`, `}}`), keywords, fields, variables, functions and literals, and comments.
 * An action left open runs to the end as plain text.
 */
export function tokenizeTemplate(src: string): Token[] {
  const out: Token[] = [];
  const state = { lineStart: true, heading: false };
  let i = 0;
  while (i < src.length) {
    const open = src.indexOf('{{', i);
    if (open < 0) {
      templateText(out, src.slice(i), state);
      break;
    }
    templateText(out, src.slice(i, open), state);
    i = open;
    const comment = /\{\{-?\s*\/\*[\s\S]*?\*\/\s*-?\}\}/y;
    comment.lastIndex = i;
    const c = comment.exec(src);
    if (c) {
      push(out, c[0], 'comment');
      i += c[0].length;
      state.lineStart = false;
      continue;
    }
    const delim = src.startsWith('{{-', i) ? '{{-' : '{{';
    let j = i + delim.length;
    const action: Token[] = [{ text: delim, kind: 'delim' }];
    let closed = false;
    while (j < src.length) {
      const close = src.startsWith('-}}', j)
        ? '-}}'
        : src.startsWith('}}', j)
          ? '}}'
          : '';
      if (close) {
        action.push({ text: close, kind: 'delim' });
        j += close.length;
        closed = true;
        break;
      }
      const m = matchAt(TEMPLATE_RULES, src, j);
      if (!m) {
        push(action, src[j]!, 'plain');
        j++;
        continue;
      }
      const [kind, text] = m;
      if (kind === 'ident') {
        push(
          action,
          text,
          TEMPLATE_KEYWORDS.has(text) ? 'keyword' : 'function',
        );
      } else push(action, text, kind as TokenKind);
      j += text.length;
    }
    if (!closed) {
      templateText(out, src.slice(i), state);
      break;
    }
    for (const t of action) push(out, t.text, t.kind);
    i = j;
    state.lineStart = false;
  }
  return out;
}

const JSON_RULES: [RegExp, TokenKind | 'str'][] = [
  [/\s+/y, 'plain'],
  [/"(?:\\.|[^"\\])*"/y, 'str'],
  [/-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/y, 'number'],
  [/true|false|null/y, 'keyword'],
  [/[{}[\],:]/y, 'punct'],
];

/** JSON text's tokens: object keys apart from string values. Invalid JSON still tokenises. */
export function tokenizeJSON(src: string): Token[] {
  const out: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const m = matchAt(JSON_RULES, src, i);
    if (!m) {
      push(out, src[i]!, 'plain');
      i++;
      continue;
    }
    const [kind, text] = m;
    if (kind === 'str') {
      const isKey = /^\s*:/.test(src.slice(i + text.length));
      push(out, text, isKey ? 'key' : 'string');
    } else push(out, text, kind as TokenKind);
    i += text.length;
  }
  return out;
}

/** Whether a string must be quoted to read back as the same string in YAML. */
function needsQuotes(s: string): boolean {
  return (
    s === '' ||
    s !== s.trim() ||
    /^[-?:,[\]{}#&*!|>'"%@`]/.test(s) ||
    /: |:$| #|\t/.test(s) ||
    /^(?:true|false|null|yes|no|on|off|~)$/i.test(s) ||
    /^[-+.]?\d|^\.(?:inf|nan)$/i.test(s) ||
    /[\u0000-\u001f\u007f]/.test(s)
  );
}

function yamlKey(out: Token[], key: string): void {
  push(out, needsQuotes(key) ? JSON.stringify(key) : key, 'key');
  push(out, ':', 'punct');
}

function isBlock(v: unknown): v is object {
  if (Array.isArray(v)) return v.length > 0;
  return (
    !!v &&
    typeof v === 'object' &&
    Object.values(v).some((x) => x !== undefined)
  );
}

/** A multi-line string as a literal block (`|`, `|-` or `|+`), when it can be one. */
function literalBlock(s: string): boolean {
  return (
    s.includes('\n') &&
    !/^[ \t]/.test(s) &&
    !/[ \t]\n|[\u0000-\u0009\u000b-\u001f\u007f]/.test(s) &&
    !/^\n/.test(s)
  );
}

function yamlScalar(out: Token[], v: unknown, indent: string): void {
  if (v === null || v === undefined) push(out, 'null', 'keyword');
  else if (typeof v === 'boolean') push(out, String(v), 'keyword');
  else if (typeof v === 'number') push(out, String(v), 'number');
  else if (typeof v === 'string') {
    if (literalBlock(v)) {
      const trailing = /\n+$/.exec(v)?.[0].length ?? 0;
      push(out, trailing === 0 ? '|-' : trailing === 1 ? '|' : '|+', 'punct');
      const body = trailing ? v.slice(0, -1) : v;
      for (const line of body.split('\n')) {
        push(out, '\n', 'plain');
        if (line) push(out, `${indent}  ${line}`, 'string');
      }
    } else push(out, needsQuotes(v) ? JSON.stringify(v) : v, 'string');
  } else push(out, JSON.stringify(v) ?? 'null', 'string');
}

/** Writes v at `indent`, the caller having written what precedes it on the current line. */
function yamlValue(out: Token[], v: unknown, indent: string): void {
  if (Array.isArray(v)) {
    if (!v.length) {
      push(out, ' []', 'punct');
      return;
    }
    for (const item of v) {
      push(out, `\n${indent}`, 'plain');
      push(out, '-', 'punct');
      yamlItem(out, item, `${indent}  `);
    }
    return;
  }
  if (v && typeof v === 'object') {
    const entries = Object.entries(v).filter(([, x]) => x !== undefined);
    if (!entries.length) {
      push(out, ' {}', 'punct');
      return;
    }
    for (const [k, x] of entries) {
      push(out, `\n${indent}`, 'plain');
      yamlKey(out, k);
      yamlChild(out, x, indent);
    }
    return;
  }
  push(out, ' ', 'plain');
  yamlScalar(out, v, indent);
}

/** A map value: a block on the next lines (indented), or a scalar after `key: `. */
function yamlChild(out: Token[], x: unknown, indent: string): void {
  if (isBlock(x)) yamlValue(out, x, `${indent}  `);
  else if (Array.isArray(x)) push(out, ' []', 'punct');
  else if (x && typeof x === 'object') push(out, ' {}', 'punct');
  else {
    push(out, ' ', 'plain');
    yamlScalar(out, x, indent);
  }
}

/** A sequence item after its `-`: a map starts on the dash's line, a nested list below it. */
function yamlItem(out: Token[], item: unknown, indent: string): void {
  if (Array.isArray(item) && item.length) {
    yamlValue(out, item, indent);
    return;
  }
  if (isBlock(item)) {
    const entries = Object.entries(item).filter(([, x]) => x !== undefined);
    entries.forEach(([k, x], n) => {
      push(out, n === 0 ? ' ' : `\n${indent}`, 'plain');
      yamlKey(out, k);
      yamlChild(out, x, indent);
    });
    return;
  }
  yamlValue(out, item, indent);
}

/** Any JSON value as block YAML, tokenised: keys, scalars by type, literal blocks for text. */
export function yamlTokens(value: unknown): Token[] {
  const out: Token[] = [];
  if (isBlock(value)) {
    yamlValue(out, value, '');
    out[0]!.text = out[0]!.text.replace(/^\n/, '');
    if (!out[0]!.text) out.shift();
  } else if (Array.isArray(value)) push(out, '[]', 'punct');
  else if (value && typeof value === 'object') push(out, '{}', 'punct');
  else yamlScalar(out, value, '');
  push(out, '\n', 'plain');
  return out;
}
