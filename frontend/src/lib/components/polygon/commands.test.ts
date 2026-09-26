import { describe, it, expect } from 'vitest';
import { COMMANDS, headingStyles, headingMarkdown, linkMarkdown } from './commands';

describe('@-command catalog (Req 11.10)', () => {
	it('covers the four commands, each with a non-empty icon and glyph', () => {
		const cmds = COMMANDS.map((c) => c.command).sort();
		expect(cmds).toEqual(['@children', '@heading', '@link', '@toc']);
		for (const c of COMMANDS) {
			expect(c.icon.length).toBeGreaterThan(0);
			expect(c.glyph.length).toBeGreaterThan(0);
			expect(c.label.length).toBeGreaterThan(0);
		}
	});

	it('mirrors the backend icon identifiers', () => {
		const byCmd = Object.fromEntries(COMMANDS.map((c) => [c.command, c.icon]));
		expect(byCmd['@link']).toBe('at-link');
		expect(byCmd['@heading']).toBe('at-heading');
		expect(byCmd['@toc']).toBe('at-toc');
		expect(byCmd['@children']).toBe('at-children');
	});

	it('marks edit-time vs render-time correctly', () => {
		const kind = Object.fromEntries(COMMANDS.map((c) => [c.command, c.kind]));
		expect(kind['@link']).toBe('edit-time');
		expect(kind['@heading']).toBe('edit-time');
		expect(kind['@toc']).toBe('render-time');
		expect(kind['@children']).toBe('render-time');
	});
});

describe('heading helpers (Req 11.4)', () => {
	it('offers levels 1..6', () => {
		expect(headingStyles().map((h) => h.level)).toEqual([1, 2, 3, 4, 5, 6]);
	});

	it('clamps out-of-range levels', () => {
		expect(headingMarkdown(0, 'x')).toBe('# x');
		expect(headingMarkdown(9, 'x')).toBe('###### x');
		expect(headingMarkdown(3, '  spaced  ')).toBe('### spaced');
	});
});

describe('linkMarkdown (Req 11.2)', () => {
	it('renders an internal link by record id', () => {
		expect(linkMarkdown('My Page', 'abc123')).toBe('[My Page](influence://polygon/abc123)');
	});
});
