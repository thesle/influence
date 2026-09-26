// Section splitting for the section-based editor (Req 12.1, 12.2, 12.3).
//
// A section is a contiguous span of the stored markdown delimited by heading
// boundaries. The editor renders each section on its own and, when a user opens
// one for editing, hands back the exact original text for that span — never a
// re-serialized copy — so an unchanged edit round-trips byte-for-byte (Req
// 12.3). To make that guarantee, we retain the exact character range of each
// section's source and slice the original string when editing.
//
// This mirrors the backend's heading-delimited `Sections()` in
// internal/markdown: a leading span before the first heading (if any) is its
// own section, and each ATX heading (`#`..`######`) opens a new section that
// runs until the next heading.

/** One heading-delimited section of the stored markdown. */
export interface Section {
	/** 0-based index in document order. */
	index: number;
	/** Heading level 1–6, or 0 for the leading pre-heading section. */
	level: number;
	/** The heading text (without the `#` prefix), or "" for the lead section. */
	title: string;
	/** The exact source text for this span, sliced verbatim from the input. */
	source: string;
	/** Inclusive start offset of `source` in the original string. */
	start: number;
	/** Exclusive end offset of `source` in the original string. */
	end: number;
}

const HEADING_RE = /^(#{1,6})\s+(.*)$/;

/**
 * Split stored markdown into heading-delimited sections, retaining the exact
 * source range of each. Concatenating every section's `source` in order
 * reproduces the input exactly (the invariant the round-trip test asserts).
 */
export function splitSections(content: string): Section[] {
	if (content.length === 0) {
		return [{ index: 0, level: 0, title: '', source: '', start: 0, end: 0 }];
	}

	// Record the char offset and heading info of every line that opens a
	// section (a heading line), plus track line starts to compute ranges.
	type Boundary = { offset: number; level: number; title: string };
	const boundaries: Boundary[] = [];

	let lineStart = 0;
	for (let i = 0; i <= content.length; i++) {
		const atEnd = i === content.length;
		if (atEnd || content[i] === '\n') {
			const line = content.slice(lineStart, i);
			const m = HEADING_RE.exec(line);
			if (m) {
				boundaries.push({ offset: lineStart, level: m[1].length, title: m[2].trim() });
			}
			lineStart = i + 1;
			if (atEnd) break;
		}
	}

	const sections: Section[] = [];
	let idx = 0;

	// Leading section: everything before the first heading. Emitted only when it
	// contains text, so a document that starts with a heading has no empty lead.
	const firstBoundary = boundaries.length > 0 ? boundaries[0].offset : content.length;
	if (firstBoundary > 0) {
		sections.push({
			index: idx++,
			level: 0,
			title: '',
			source: content.slice(0, firstBoundary),
			start: 0,
			end: firstBoundary
		});
	}

	for (let b = 0; b < boundaries.length; b++) {
		const start = boundaries[b].offset;
		const end = b + 1 < boundaries.length ? boundaries[b + 1].offset : content.length;
		sections.push({
			index: idx++,
			level: boundaries[b].level,
			title: boundaries[b].title,
			source: content.slice(start, end),
			start,
			end
		});
	}

	// A document with no headings and no leading text (e.g. only whitespace that
	// still has length) still yields one section covering the whole input.
	if (sections.length === 0) {
		sections.push({ index: 0, level: 0, title: '', source: content, start: 0, end: content.length });
	}

	return sections;
}

/**
 * Replace one section's source with a new buffer, returning the full rebuilt
 * document. Confirming an unchanged buffer reproduces the original content
 * exactly (Req 12.3); cancelling never calls this, leaving stored bytes
 * untouched (Req 12.4).
 */
export function replaceSection(content: string, section: Section, edited: string): string {
	return content.slice(0, section.start) + edited + content.slice(section.end);
}
