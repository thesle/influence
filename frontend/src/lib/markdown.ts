// Minimal, dependency-free markdown renderer for the in-app docs viewer
// (task 23.3, Req 27.2). It supports the subset the platform docs use:
// headings, paragraphs, unordered/ordered lists, fenced code blocks, inline
// code, bold, italics, and links. It is deliberately small — the goal is to
// render platform documentation in-app, not to be a full CommonMark engine.
//
// Security: all text is HTML-escaped before any inline formatting is applied,
// so untrusted document content cannot inject markup. Links are restricted to
// http(s), mailto, and in-app relative paths; anything else renders as plain
// text. The output is a trusted HTML string the viewer injects via {@html}.

/** Escape the five HTML-significant characters. */
function escapeHtml(text: string): string {
	return text
		.replace(/&/g, '&amp;')
		.replace(/</g, '&lt;')
		.replace(/>/g, '&gt;')
		.replace(/"/g, '&quot;')
		.replace(/'/g, '&#39;');
}

/** Whether a link target is allowed to render as an anchor. */
function isSafeHref(href: string): boolean {
	const trimmed = href.trim();
	if (trimmed === '') return false;
	// Relative / in-app links (no scheme) and fragment links are fine.
	if (/^(\/|#|\.{1,2}\/)/.test(trimmed)) return true;
	if (!/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(trimmed)) return true; // scheme-less
	return /^(https?:|mailto:)/i.test(trimmed);
}

/**
 * Render inline markdown (code, bold, italics, links) within already-escaped
 * text. Inline code is extracted first so its contents are not re-formatted.
 */
function renderInline(text: string): string {
	const escaped = escapeHtml(text);

	// Inline code — protect its contents from further inline rules.
	const codeSlots: string[] = [];
	let out = escaped.replace(/`([^`]+)`/g, (_m, code: string) => {
		codeSlots.push(`<code>${code}</code>`);
		return `\u0000${codeSlots.length - 1}\u0000`;
	});

	// Links: [text](href) — href was HTML-escaped, so unescape &amp; for the check.
	out = out.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (whole, label: string, href: string) => {
		const rawHref = href.replace(/&amp;/g, '&');
		if (!isSafeHref(rawHref)) return whole;
		return `<a href="${href}">${label}</a>`;
	});

	// Bold then italics (order matters so ** is consumed before *).
	out = out.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
	out = out.replace(/(^|[^*])\*([^*]+)\*/g, '$1<em>$2</em>');

	// Restore inline-code slots.
	out = out.replace(/\u0000(\d+)\u0000/g, (_m, i: string) => codeSlots[Number(i)]);
	return out;
}

type ListKind = 'ul' | 'ol';

/**
 * Render a markdown string to a trusted HTML string. Unknown constructs degrade
 * to escaped paragraph text rather than being dropped.
 */
export function renderMarkdown(markdown: string): string {
	const lines = markdown.replace(/\r\n/g, '\n').split('\n');
	const html: string[] = [];

	let paragraph: string[] = [];
	let listKind: ListKind | null = null;
	let inCode = false;
	let codeLines: string[] = [];

	const flushParagraph = () => {
		if (paragraph.length > 0) {
			html.push(`<p>${renderInline(paragraph.join(' '))}</p>`);
			paragraph = [];
		}
	};
	const flushList = () => {
		if (listKind) {
			html.push(`</${listKind}>`);
			listKind = null;
		}
	};

	for (const line of lines) {
		const fence = line.trimStart().startsWith('```');
		if (fence) {
			if (inCode) {
				html.push(`<pre><code>${escapeHtml(codeLines.join('\n'))}</code></pre>`);
				codeLines = [];
				inCode = false;
			} else {
				flushParagraph();
				flushList();
				inCode = true;
			}
			continue;
		}
		if (inCode) {
			codeLines.push(line);
			continue;
		}

		if (line.trim() === '') {
			flushParagraph();
			flushList();
			continue;
		}

		const heading = /^(#{1,6})\s+(.*)$/.exec(line);
		if (heading) {
			flushParagraph();
			flushList();
			const level = heading[1].length;
			html.push(`<h${level}>${renderInline(heading[2].trim())}</h${level}>`);
			continue;
		}

		const ulItem = /^\s*[-*+]\s+(.*)$/.exec(line);
		const olItem = /^\s*\d+\.\s+(.*)$/.exec(line);
		if (ulItem || olItem) {
			flushParagraph();
			const kind: ListKind = ulItem ? 'ul' : 'ol';
			if (listKind !== kind) {
				flushList();
				html.push(`<${kind}>`);
				listKind = kind;
			}
			const item = (ulItem ? ulItem[1] : (olItem as RegExpExecArray)[1]).trim();
			html.push(`<li>${renderInline(item)}</li>`);
			continue;
		}

		flushList();
		paragraph.push(line.trim());
	}

	// Close anything left open at EOF.
	if (inCode) {
		html.push(`<pre><code>${escapeHtml(codeLines.join('\n'))}</code></pre>`);
	}
	flushParagraph();
	flushList();

	return html.join('\n');
}
