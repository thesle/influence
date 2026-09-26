// Minimal client-side markdown renderer for per-section edit previews.
//
// The authoritative rendered view comes from the backend render route (masked,
// with resolved Cross-Refraction links and the metadata footer). This tiny
// renderer exists only for the live *edit preview* of a single section, where
// contacting the server on every keystroke would be wasteful. It handles the
// common CommonMark subset plus the platform's inline encodings so the preview
// resembles the final render:
//   - headings, bold/italic/code, paragraphs, unordered lists
//   - internal links `[label](influence://polygon/{id})` → activatable-looking
//   - external links `[label](https://…)`
//   - image refs `![alt](influence://image/{id})` → served blob URL
//   - obfuscation tokens `|encrypt|{id}|` → a masked placeholder span
//
// It escapes all text first, so it never emits raw user HTML (no XSS via the
// preview path).

const IMAGE_RE = /!\[([^\]]*)\]\(influence:\/\/image\/([^)]+)\)/g;
const INTERNAL_LINK_RE = /\[([^\]]+)\]\(influence:\/\/polygon\/([^)]+)\)/g;
const EXTERNAL_LINK_RE = /\[([^\]]+)\]\((https?:\/\/[^)]+)\)/g;
const TOKEN_RE = /\|encrypt\|([^|]+)\|/g;

export function escapeHtml(s: string): string {
	return s
		.replace(/&/g, '&amp;')
		.replace(/</g, '&lt;')
		.replace(/>/g, '&gt;')
		.replace(/"/g, '&quot;')
		.replace(/'/g, '&#39;');
}

/** Render inline spans (links, images, tokens, emphasis) within escaped text. */
function renderInline(escaped: string): string {
	let out = escaped;
	// Images resolve to the served blob endpoint (Req 17.4).
	out = out.replace(IMAGE_RE, (_m, alt, id) => {
		const safeId = encodeURIComponent(String(id));
		return `<img alt="${alt}" src="/api/images/${safeId}" />`;
	});
	// Internal links render as activatable links to the polygon route; name
	// resolution/accessibility happens on the server render path (Req 14).
	out = out.replace(INTERNAL_LINK_RE, (_m, label, id) => {
		const safeId = encodeURIComponent(String(id));
		return `<a class="xref" href="/p/${safeId}">${label}</a>`;
	});
	// External links (Req 13.3).
	out = out.replace(EXTERNAL_LINK_RE, (_m, label, url) => {
		return `<a class="external" href="${url}" rel="noreferrer noopener" target="_blank">${label}</a>`;
	});
	// Obfuscation tokens render masked in the preview; real reveal happens on the
	// server-rendered view via the reveal control (Req 16.3).
	out = out.replace(TOKEN_RE, (_m, id) => {
		return `<span class="masked" data-token="${escapeHtml(String(id))}">🔒 masked</span>`;
	});
	// Inline emphasis/code (order matters: code first to avoid nested parsing).
	out = out.replace(/`([^`]+)`/g, '<code>$1</code>');
	out = out.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
	out = out.replace(/\*([^*]+)\*/g, '<em>$1</em>');
	return out;
}

/** Render a markdown string to a safe HTML fragment for the edit preview. */
export function renderMarkdownPreview(md: string): string {
	const lines = md.split('\n');
	const html: string[] = [];
	let inList = false;
	let paragraph: string[] = [];

	const flushParagraph = () => {
		if (paragraph.length > 0) {
			html.push(`<p>${renderInline(escapeHtml(paragraph.join(' ')))}</p>`);
			paragraph = [];
		}
	};
	const closeList = () => {
		if (inList) {
			html.push('</ul>');
			inList = false;
		}
	};

	for (const raw of lines) {
		const line = raw.trimEnd();
		const heading = /^(#{1,6})\s+(.*)$/.exec(line);
		const listItem = /^[-*]\s+(.*)$/.exec(line);

		if (heading) {
			flushParagraph();
			closeList();
			const level = heading[1].length;
			html.push(`<h${level}>${renderInline(escapeHtml(heading[2]))}</h${level}>`);
		} else if (listItem) {
			flushParagraph();
			if (!inList) {
				html.push('<ul>');
				inList = true;
			}
			html.push(`<li>${renderInline(escapeHtml(listItem[1]))}</li>`);
		} else if (line.trim() === '') {
			flushParagraph();
			closeList();
		} else {
			closeList();
			paragraph.push(line);
		}
	}
	flushParagraph();
	closeList();
	return html.join('\n');
}
