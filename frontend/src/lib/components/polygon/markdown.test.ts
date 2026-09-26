import { describe, it, expect } from 'vitest';
import { renderMarkdownPreview, escapeHtml } from './markdown';

describe('renderMarkdownPreview', () => {
	it('escapes raw HTML so the preview never injects markup', () => {
		const html = renderMarkdownPreview('<script>alert(1)</script>');
		expect(html).not.toContain('<script>');
		expect(html).toContain('&lt;script&gt;');
	});

	it('renders headings and emphasis', () => {
		expect(renderMarkdownPreview('# Title')).toContain('<h1>Title</h1>');
		expect(renderMarkdownPreview('**bold**')).toContain('<strong>bold</strong>');
	});

	it('resolves image references to the served blob endpoint', () => {
		const html = renderMarkdownPreview('![alt](influence://image/img-1)');
		expect(html).toContain('src="/api/images/img-1"');
	});

	it('renders internal links to the polygon route', () => {
		const html = renderMarkdownPreview('[Page](influence://polygon/rec-9)');
		expect(html).toContain('href="/p/rec-9"');
		expect(html).toContain('>Page</a>');
	});

	it('masks obfuscation tokens', () => {
		const html = renderMarkdownPreview('secret: |encrypt|tok-1|');
		expect(html).toContain('class="masked"');
		expect(html).toContain('data-token="tok-1"');
		expect(html).not.toContain('|encrypt|');
	});
});

describe('escapeHtml', () => {
	it('escapes the five significant characters', () => {
		expect(escapeHtml(`<>&"'`)).toBe('&lt;&gt;&amp;&quot;&#39;');
	});
});
