// `@`-command catalog and heading styles for the Markdown editor toolbar
// (Req 11.10). The backend's markdown package (internal/markdown/atcommands.go)
// is the source of truth for the four commands and their stable icon
// identifiers; this module mirrors that catalog for the frontend toolbar so the
// typed form and its on-screen icon stay in lockstep. Keeping it a plain module
// (not fetched at runtime) keeps the editor self-contained and testable.

/** The four recognized `@`-commands (Req 11.2, 11.4, 11.5, 11.7). */
export type AtCommand = '@link' | '@heading' | '@toc' | '@children';

/** When a command resolves: edit-time authoring aid vs render-time directive. */
export type CommandKind = 'edit-time' | 'render-time';

/** One toolbar command: the typed form, its kind, icon id, and label. */
export interface CommandInfo {
	command: AtCommand;
	kind: CommandKind;
	/** Stable icon identifier mirrored from the backend catalog (Req 11.10). */
	icon: string;
	label: string;
	/** A short glyph the toolbar renders for the icon id. */
	glyph: string;
}

/**
 * The command catalog, mirroring `markdown.Commands()` including the backend's
 * icon identifiers (`at-link`, `at-heading`, `at-toc`, `at-children`). Each
 * entry carries a non-empty icon so no command is ever shown without a glyph
 * (Req 11.10).
 */
export const COMMANDS: readonly CommandInfo[] = [
	{ command: '@link', kind: 'edit-time', icon: 'at-link', label: 'Link', glyph: '🔗' },
	{ command: '@heading', kind: 'edit-time', icon: 'at-heading', label: 'Heading', glyph: 'H' },
	{
		command: '@toc',
		kind: 'render-time',
		icon: 'at-toc',
		label: 'Table of contents',
		glyph: '☰'
	},
	{ command: '@children', kind: 'render-time', icon: 'at-children', label: 'Children', glyph: '▤' }
];

/** One selectable markdown heading style offered by `@heading` (Req 11.4). */
export interface HeadingStyle {
	level: number;
	label: string;
	/** The markdown that opens a heading of this level, e.g. "# ". */
	prefix: string;
}

/**
 * The fixed heading-style choices for `@heading` (Req 11.4), levels 1–6
 * shallow-to-deep, mirroring `markdown.HeadingStyles()`.
 */
export function headingStyles(): HeadingStyle[] {
	const styles: HeadingStyle[] = [];
	for (let level = 1; level <= 6; level++) {
		styles.push({ level, label: `Heading ${level}`, prefix: '#'.repeat(level) + ' ' });
	}
	return styles;
}

/** Render the markdown a heading choice inserts, clamped to a valid ATX level. */
export function headingMarkdown(level: number, text: string): string {
	const l = Math.min(6, Math.max(1, level));
	return '#'.repeat(l) + ' ' + text.trim();
}

/** Render the markdown an `@link` selection inserts (internal link by id). */
export function linkMarkdown(name: string, recordId: string): string {
	return `[${name}](influence://polygon/${recordId})`;
}
