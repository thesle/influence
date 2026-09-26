import { describe, it, expect } from 'vitest';
import fc from 'fast-check';
import { splitSections, replaceSection } from './sections';

describe('splitSections', () => {
	it('returns a single empty section for empty content', () => {
		const secs = splitSections('');
		expect(secs).toHaveLength(1);
		expect(secs[0].source).toBe('');
	});

	it('splits a leading span and heading-delimited sections', () => {
		const md = 'intro text\n\n# First\nbody\n\n## Second\nmore';
		const secs = splitSections(md);
		expect(secs.map((s) => s.level)).toEqual([0, 1, 2]);
		expect(secs[1].title).toBe('First');
		expect(secs[2].title).toBe('Second');
	});

	it('has no leading section when content starts with a heading', () => {
		const secs = splitSections('# Only\nbody');
		expect(secs).toHaveLength(1);
		expect(secs[0].level).toBe(1);
	});

	it('concatenating section sources reproduces the input', () => {
		const md = 'lead\n# A\nx\n### B\ny\n';
		const secs = splitSections(md);
		expect(secs.map((s) => s.source).join('')).toBe(md);
	});
});

describe('replaceSection round-trip (Req 12.3)', () => {
	it('re-inserting an unchanged section reproduces the content byte-for-byte', () => {
		const md = '# A\nbody a\n## B\nbody b\n';
		const secs = splitSections(md);
		for (const s of secs) {
			expect(replaceSection(md, s, s.source)).toBe(md);
		}
	});

	// Property: for any content, opening any section and confirming without
	// changes yields exactly the original content (Req 12.3).
	it('property: unchanged confirm is a byte-for-byte round-trip', () => {
		fc.assert(
			fc.property(
				fc.array(
					fc.oneof(
						fc.constantFrom('# H1', '## H2', '### H3'),
						fc.string({ maxLength: 20 })
					),
					{ maxLength: 12 }
				),
				(lines) => {
					const content = lines.join('\n');
					const secs = splitSections(content);
					// Reconstruction invariant.
					expect(secs.map((s) => s.source).join('')).toBe(content);
					// Every unchanged re-insert reproduces the original.
					for (const s of secs) {
						expect(replaceSection(content, s, s.source)).toBe(content);
					}
				}
			),
			{ numRuns: 200 }
		);
	});

	it('editing a section splices only that span', () => {
		const md = '# A\nold\n# B\nkeep\n';
		const secs = splitSections(md);
		const first = secs.find((s) => s.title === 'A')!;
		const edited = replaceSection(md, first, '# A\nnew\n');
		expect(edited).toBe('# A\nnew\n# B\nkeep\n');
	});
});
