import { describe, expect, it } from 'vitest';

import { inlineText, parseInline, parseMarkdown, safeHref } from './markdown';

describe('safeHref', () => {
  it('allows http, https, mailto and site-relative targets', () => {
    expect(safeHref('https://example.com/a')).toBe('https://example.com/a');
    expect(safeHref('http://example.com')).toBe('http://example.com');
    expect(safeHref('mailto:ops@example.com')).toBe('mailto:ops@example.com');
    expect(safeHref('/providers')).toBe('/providers');
    expect(safeHref('#top')).toBe('#top');
  });

  it('refuses script-bearing and opaque schemes', () => {
    expect(safeHref('javascript:alert(1)')).toBeNull();
    expect(safeHref('JavaScript:alert(1)')).toBeNull();
    expect(safeHref('data:text/html,<script>')).toBeNull();
    expect(safeHref('vbscript:msgbox')).toBeNull();
    expect(safeHref('file:///etc/passwd')).toBeNull();
    expect(safeHref('')).toBeNull();
  });
});

describe('parseInline', () => {
  it('reads inline code', () => {
    expect(parseInline('use `max_tokens` here')).toEqual([
      { type: 'text', value: 'use ' },
      { type: 'code', value: 'max_tokens' },
      { type: 'text', value: ' here' },
    ]);
  });

  it('reads strong and emphasis, preferring strong', () => {
    expect(parseInline('**bold**')).toEqual([
      { type: 'strong', children: [{ type: 'text', value: 'bold' }] },
    ]);
    expect(parseInline('*italic*')).toEqual([
      { type: 'em', children: [{ type: 'text', value: 'italic' }] },
    ]);
  });

  it('does not turn an identifier into emphasis', () => {
    const nodes = parseInline('the max_output_tokens field');
    expect(nodes).toEqual([{ type: 'text', value: 'the max_output_tokens field' }]);
  });

  it('keeps emphasis inside a code span from applying', () => {
    expect(parseInline('`a * b`')).toEqual([{ type: 'code', value: 'a * b' }]);
  });

  it('reads a link and drops an unsafe one to its label', () => {
    expect(parseInline('[docs](https://example.com)')).toEqual([
      { type: 'link', href: 'https://example.com', children: [{ type: 'text', value: 'docs' }] },
    ]);
    expect(parseInline('[click](javascript:alert(1))')).toEqual([
      { type: 'text', value: 'click' },
    ]);
  });

  it('leaves an unterminated marker as text', () => {
    expect(parseInline('2 * 3 = 6')).toEqual([{ type: 'text', value: '2 * 3 = 6' }]);
  });
});

describe('parseMarkdown', () => {
  it('parses headings at each level', () => {
    expect(parseMarkdown('## Summary')).toEqual([
      { type: 'heading', level: 2, children: [{ type: 'text', value: 'Summary' }] },
    ]);
  });

  it('parses a fenced code block with its language', () => {
    const blocks = parseMarkdown('```json\n{"a":1}\n```');
    expect(blocks).toEqual([{ type: 'code', language: 'json', value: '{"a":1}' }]);
  });

  it('keeps the body of an unterminated fence', () => {
    const blocks = parseMarkdown('here:\n\n```go\nfunc main() {');
    expect(blocks[1]).toEqual({ type: 'code', language: 'go', value: 'func main() {' });
  });

  it('renders every streaming prefix of a fenced block without throwing', () => {
    const full = 'Answer:\n\n```python\nprint("hello")\n```\n\nDone.';
    for (let end = 1; end <= full.length; end += 7) {
      const prefix = full.slice(0, end);
      let blocks;
      expect(() => {
        blocks = parseMarkdown(prefix);
      }).not.toThrow();
      expect(Array.isArray(blocks)).toBe(true);
    }
    expect(parseMarkdown(full)).toEqual([
      {
        type: 'paragraph',
        children: [{ type: 'text', value: 'Answer:' }],
      },
      { type: 'code', language: 'python', value: 'print("hello")' },
      {
        type: 'paragraph',
        children: [{ type: 'text', value: 'Done.' }],
      },
    ]);
  });

  it('keeps code readable while the fence is still open', () => {
    const blocks = parseMarkdown('```js\nconst x = 1;\nconst y = 2;');
    expect(blocks).toHaveLength(1);
    expect(blocks[0]).toMatchObject({ type: 'code', language: 'js' });
    if (blocks[0]?.type === 'code') {
      expect(blocks[0].value).toContain('const x = 1;');
      expect(blocks[0].value).toContain('const y = 2;');
    }
  });

  it('parses ordered and unordered lists', () => {
    const unordered = parseMarkdown('- one\n- two');
    expect(unordered).toHaveLength(1);
    const first = unordered[0];
    expect(first?.type).toBe('list');
    if (first?.type === 'list') {
      expect(first.ordered).toBe(false);
      expect(first.items).toHaveLength(2);
    }

    const ordered = parseMarkdown('1. first\n2. second');
    const second = ordered[0];
    expect(second?.type).toBe('list');
    if (second?.type === 'list') expect(second.ordered).toBe(true);
  });

  it('parses quotes and rules', () => {
    expect(parseMarkdown('> note this')).toEqual([
      { type: 'quote', children: [{ type: 'text', value: 'note this' }] },
    ]);
    expect(parseMarkdown('---')).toEqual([{ type: 'rule' }]);
  });

  it('joins wrapped paragraph lines into one paragraph', () => {
    const blocks = parseMarkdown('a long line\nthat wrapped');
    expect(blocks).toEqual([
      { type: 'paragraph', children: [{ type: 'text', value: 'a long line that wrapped' }] },
    ]);
  });

  it('separates a paragraph from a following list', () => {
    const blocks = parseMarkdown('Steps:\n- one\n- two');
    expect(blocks.map((block) => block.type)).toEqual(['paragraph', 'list']);
  });

  it('returns nothing for empty input', () => {
    expect(parseMarkdown('')).toEqual([]);
    expect(parseMarkdown('\n\n  \n')).toEqual([]);
  });

  it('never emits HTML, carrying markup through as text', () => {
    const blocks = parseMarkdown('<script>alert(1)</script>');
    expect(blocks[0]).toEqual({
      type: 'paragraph',
      children: [{ type: 'text', value: '<script>alert(1)</script>' }],
    });
  });
});

describe('inlineText', () => {
  it('flattens an inline tree to plain text', () => {
    expect(inlineText(parseInline('a **b** `c`'))).toBe('a b c');
  });
});
