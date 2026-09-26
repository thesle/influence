import { describe, expect, test } from 'vitest';
import fc from 'fast-check';
import { renderMarkdown } from './markdown';

describe('renderMarkdown (Req 27.2)', () => {
	test('renders headings at the right level', () => {
		expect(renderMarkdown('# Title')).toContain('<h1>Title</h1>');
		expect(renderMarkdown('### Sub')).toContain('<h3>Sub</h3>');
	});

	test('renders paragraphs joining wrapped lines', () => {
		expect(renderMarkdown('one\ntwo')).toBe('<p>one two</p>');
	});

	test('renders unordered and ordered lists', () => {
		expect(renderMarkdown('- a\n- b')).toBe('<ul>\n<li>a</li>\n<li>b</li>\n</ul>');
		expect(renderMarkdown('1. a\n2. b')).toBe('<ol>\n<li>a</li>\n<li>b</li>\n</ol>');
	});

	test('renders fenced code blocks without inline formatting', () => {
		const md = '```\nconst x = **not bold**;\n```';
		const html = renderMarkdown(md);
		expect(html).toContain('<pre><code>');
		expect(html).toContain('const x = **not bold**;');
		expect(html).not.toContain('<strong>');
	});

	test('renders inline code, bold, italics, and safe links', () => {
		expect(renderMarkdown('use `code` here')).toContain('<code>code</code>');
		expect(renderMarkdown('**bold**')).toContain('<strong>bold</strong>');
		expect(renderMarkdown('a *word*')).toContain('<em>word</em>');
		expect(renderMarkdown('[docs](/docs/x)')).toContain('<a href="/docs/x">docs</a>');
	});

	test('renders unsafe link schemes as plain text (no anchor)', () => {
		const html = renderMarkdown('[x](javascript:alert(1))');
		expect(html).not.toContain('<a ');
		expect(html).toContain('javascript:alert(1)');
	});

	// Safety: no matter the input, the renderer never emits a raw <script> tag
	// or an unescaped user-supplied angle bracket that could inject markup.
	test('never emits raw script tags or unescaped user angle brackets', () => {
		fc.assert(
			fc.property(fc.string(), (input) => {
				const html = renderMarkdown(input);
				// The renderer never introduces <script>; any in the input is escaped.
				expect(html.toLowerCase()).not.toContain('<script');
				// A literal "<" from the input must be escaped to &lt; — the only
				// "<" characters in the output are from the renderer's own tags,
				// which are always immediately followed by an ASCII letter or "/".
				for (let i = 0; i < html.length; i++) {
					if (html[i] === '<') {
						const next = html[i + 1] ?? '';
						expect(/[a-zA-Z/]/.test(next)).toBe(true);
					}
				}
			}),
			{ numRuns: 300 }
		);
	});

	test('escapes HTML in headings and paragraphs', () => {
		expect(renderMarkdown('# <b>hi</b>')).toContain('&lt;b&gt;hi&lt;/b&gt;');
		expect(renderMarkdown('a < b & c')).toContain('a &lt; b &amp; c');
	});
});
