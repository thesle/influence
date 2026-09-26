// Influence — a self-hostable documentation platform.
// Copyright (C) 2026  Conrad Smith
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

// Grid (de)serialization for Tabular_Polygon (Req 10.1).
//
// A Tabular_Polygon stores its grid as content. We use a GitHub-flavored
// markdown pipe table as the on-disk form so the content stays human-readable
// markdown (consistent with the platform's markdown-first storage) and round
// trips through parse → edit → serialize. An empty/blank content yields a
// single empty cell so a new tabular polygon is immediately editable.

export type Grid = string[][];

/** Parse a markdown pipe table into a rectangular grid of trimmed cells. */
export function parseGrid(content: string): Grid {
	const lines = content
		.split('\n')
		.map((l) => l.trim())
		.filter((l) => l.length > 0);

	// Drop a GFM alignment separator row (e.g. `| --- | :--: |`).
	const rows = lines.filter((l) => !/^\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?$/.test(l));

	const grid: Grid = rows.map((line) => {
		let cells = line.split('|');
		// Trim leading/trailing empties from surrounding pipes.
		if (cells.length && cells[0].trim() === '') cells = cells.slice(1);
		if (cells.length && cells[cells.length - 1].trim() === '') cells = cells.slice(0, -1);
		return cells.map((c) => c.trim());
	});

	if (grid.length === 0) return [['']];

	// Normalize to a rectangle so the editor renders a stable grid.
	const width = grid.reduce((w, r) => Math.max(w, r.length), 1);
	return grid.map((r) => {
		const row = r.slice();
		while (row.length < width) row.push('');
		return row;
	});
}

/** Serialize a grid back to a GFM pipe table (header row + separator). */
export function serializeGrid(grid: Grid): string {
	if (grid.length === 0 || (grid.length === 1 && grid[0].every((c) => c === ''))) {
		return '';
	}
	const width = grid.reduce((w, r) => Math.max(w, r.length), 1);
	const esc = (c: string) => c.replace(/\|/g, '\\|');
	const line = (cells: string[]) => {
		const padded = cells.slice();
		while (padded.length < width) padded.push('');
		return `| ${padded.map(esc).join(' | ')} |`;
	};
	const out: string[] = [];
	out.push(line(grid[0]));
	out.push(`| ${Array(width).fill('---').join(' | ')} |`);
	for (let i = 1; i < grid.length; i++) out.push(line(grid[i]));
	return out.join('\n');
}

/** Return a new grid with an appended empty row matching the current width. */
export function addRow(grid: Grid): Grid {
	const width = grid.reduce((w, r) => Math.max(w, r.length), 1);
	return [...grid.map((r) => r.slice()), Array(width).fill('')];
}

/** Return a new grid with an appended empty column. */
export function addColumn(grid: Grid): Grid {
	return grid.map((r) => [...r, '']);
}
