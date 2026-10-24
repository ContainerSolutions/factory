import { describe, expect, it } from 'vitest';
import yaml from 'js-yaml';
import {
  tokenizeCEL,
  tokenizeJSON,
  tokenizeTemplate,
  tokenizeValue,
  tokensText,
  yamlTokens,
  type Token,
  type TokenKind,
} from './highlight';

/** The tokens of one kind, as text. */
function ofKind(tokens: Token[], kind: TokenKind): string[] {
  return tokens.filter((t) => t.kind === kind).map((t) => t.text);
}

describe('tokenizeCEL', () => {
  it.each<[string, TokenKind, string[]]>([
    ["actor.tier != 'maintainer' && !actor.isBot", 'string', ["'maintainer'"]],
    [
      "actor.tier != 'maintainer' && !actor.isBot",
      'variable',
      ['actor', 'actor'],
    ],
    ["actor.tier != 'maintainer' && !actor.isBot", 'field', ['tier', 'isBot']],
    [
      "actor.tier != 'maintainer' && !actor.isBot",
      'operator',
      [' != '.trim(), '&&', '!'],
    ],
    ["item.count('implement') < params.maxRounds", 'function', ['count']],
    ['x in [1, 2.5, 0x1F] || y == null', 'number', ['1', '2.5', '0x1F']],
    ['x in [1, 2.5, 0x1F] || y == null', 'keyword', ['in', 'null']],
    [
      'r"raw \\d" + """multi\nline"""',
      'string',
      ['r"raw \\d"', '"""multi\nline"""'],
    ],
  ])('%s: %s', (src, kind, want) => {
    const tokens = tokenizeCEL(src);
    expect(tokensText(tokens)).toBe(src);
    expect(ofKind(tokens, kind)).toEqual(want);
  });
});

describe('tokenizeValue', () => {
  it('highlights a =CEL value and leaves anything else a string', () => {
    expect(tokenizeValue('main')).toEqual([{ text: 'main', kind: 'string' }]);
    const cel = tokenizeValue("=task.target.repo + '@' + inputs.base");
    expect(cel[0]).toEqual({ text: '=', kind: 'operator' });
    expect(ofKind(cel, 'field')).toEqual(['target', 'repo', 'base']);
    expect(tokensText(cel)).toBe("=task.target.repo + '@' + inputs.base");
  });
});

describe('tokenizeTemplate', () => {
  const src = [
    '{{- /* a comment */ -}}',
    '{{- $n := index .item.state "stackNext" -}}',
    '# Task: {{ .task.spec }} for {{ .task.target.repo }}',
    '',
    '{{ if eq .inputs.mode "new" -}}',
    'Implement {{ printf "%q" .inputs.issue }}.',
    '{{- end }}',
    'Unclosed {{ .x',
  ].join('\n');
  const tokens = tokenizeTemplate(src);

  it.each<[TokenKind, string[]]>([
    ['comment', ['{{- /* a comment */ -}}']],
    ['variable', ['$n']],
    [
      'field',
      [
        '.item.state',
        '.task.spec',
        '.task.target.repo',
        '.inputs.mode',
        '.inputs.issue',
      ],
    ],
    ['keyword', ['if', 'end']],
    ['function', ['index', 'eq', 'printf']],
    ['string', ['"stackNext"', '"new"', '"%q"']],
    ['operator', [':=']],
    ['heading', ['# Task: ', ' for ', '\n']],
  ])('%s', (kind, want) => {
    expect(ofKind(tokens, kind)).toEqual(want);
  });

  it('keeps every character and the delimiters', () => {
    expect(tokensText(tokens)).toBe(src);
    expect(ofKind(tokens, 'delim')).toEqual([
      '{{-',
      '-}}',
      '{{',
      '}}',
      '{{',
      '}}',
      '{{',
      '-}}',
      '{{',
      '}}',
      '{{-',
      '}}',
    ]);
    expect(tokens[tokens.length - 1]).toEqual({
      text: '\nUnclosed {{ .x',
      kind: 'plain',
    });
  });

  it('treats a whole heading line as the heading', () => {
    expect(ofKind(tokenizeTemplate('## Goal\nText'), 'heading')).toEqual([
      '## Goal\n',
    ]);
    expect(ofKind(tokenizeTemplate('Not # a heading'), 'heading')).toEqual([]);
  });
});

describe('tokenizeJSON', () => {
  it('tells keys from string values', () => {
    const src =
      '{ "type": "object", "required": ["data"], "n": -1.5e3, "x": true, "y": null }';
    const tokens = tokenizeJSON(src);
    expect(tokensText(tokens)).toBe(src);
    expect(ofKind(tokens, 'key')).toEqual([
      '"type"',
      '"required"',
      '"n"',
      '"x"',
      '"y"',
    ]);
    expect(ofKind(tokens, 'string')).toEqual(['"object"', '"data"']);
    expect(ofKind(tokens, 'number')).toEqual(['-1.5e3']);
    expect(ofKind(tokens, 'keyword')).toEqual(['true', 'null']);
  });
});

describe('yamlTokens', () => {
  it.each<[string, unknown, string]>([
    ['a scalar', 'text', 'text\n'],
    ['an empty map', {}, '{}\n'],
    ['an empty list', [], '[]\n'],
    [
      'a nested object',
      {
        kind: 'TaskSpec',
        metadata: { name: 'implement', labels: {} },
        spec: {
          inputs: { mode: { type: 'string', enum: ['new', 'address-review'] } },
          skills: [{ source: 'ccf-skills', use: ['lisa', 'resolve'] }],
          limits: { maxTurns: 40, maxCostUSD: 1.5 },
          headless: true,
          none: null,
          skipped: undefined,
        },
      },
      [
        'kind: TaskSpec',
        'metadata:',
        '  name: implement',
        '  labels: {}',
        'spec:',
        '  inputs:',
        '    mode:',
        '      type: string',
        '      enum:',
        '        - new',
        '        - address-review',
        '  skills:',
        '    - source: ccf-skills',
        '      use:',
        '        - lisa',
        '        - resolve',
        '  limits:',
        '    maxTurns: 40',
        '    maxCostUSD: 1.5',
        '  headless: true',
        '  none: null',
        '',
      ].join('\n'),
    ],
    [
      'strings that need quotes',
      { a: '', b: 'true', c: '42', d: '- x', e: 'k: v', f: '=x', g: 'a\tb' },
      'a: ""\nb: "true"\nc: "42"\nd: "- x"\ne: "k: v"\nf: =x\ng: "a\\tb"\n',
    ],
    [
      'multi-line strings as literal blocks',
      { keep: 'one\ntwo\n', strip: 'one\ntwo', many: 'x\n\n' },
      'keep: |\n  one\n  two\nstrip: |-\n  one\n  two\nmany: |+\n  x\n\n',
    ],
    ['nested lists', [[1, 2], []], '-\n  - 1\n  - 2\n- []\n'],
  ])('%s', (_, value, want) => {
    expect(tokensText(yamlTokens(value))).toBe(want);
  });

  it.each<[string, unknown]>([
    [
      'an object with every shape',
      {
        s: 'plain',
        q: 'yes',
        n: 0,
        f: 2.5,
        b: false,
        z: null,
        list: [{ a: 1, b: [1, { c: 'x\ny\n' }] }, 'two', [], {}],
        map: { 'key with: colon': 1, '#hash': 2, '': 3 },
        text: 'line one\n\nline three\n',
        lead: ' leading space\nsecond',
      },
    ],
  ])('round-trips %s through a YAML parser', (_, value) => {
    expect(yaml.load(tokensText(yamlTokens(value)))).toEqual(value);
  });

  it('colours keys and scalars by type', () => {
    const tokens = yamlTokens({ a: 'x', n: 1, b: true });
    expect(ofKind(tokens, 'key')).toEqual(['a', 'n', 'b']);
    expect(ofKind(tokens, 'string')).toEqual(['x']);
    expect(ofKind(tokens, 'number')).toEqual(['1']);
    expect(ofKind(tokens, 'keyword')).toEqual(['true']);
  });
});
