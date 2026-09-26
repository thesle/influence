import { describe, it, expect } from 'vitest';
import fc from 'fast-check';
import { parseGrid, serializeGrid, addRow, addColumn } from './tabular';

describe('tabular grid', () => {
	it('parses a GFM pipe table dropping the separator row', () => {
		const md = '| a | b |\n| --- | --- |\n| 1 | 2 |';
		expect(parseGrid(md)).toEqual([
			['a', 'b'],
			['1', '2']
		]);
	});

	it('yields a single empty cell for blank content', () => {
		expect(parseGrid('')).toEqual([['']]);
	});

	it('normalizes ragged rows to a rectangle', () => {
		const md = '| a | b | c |\n| 1 |';
		const grid = parseGrid(md);
		expect(grid.every((r) => r.length === 3)).toBe(true);
	});

	it('addRow / addColumn keep the grid rectangular', () => {
		let g = [['a', 'b']];
		g = addRow(g);
		expect(g).toEqual([
			['a', 'b'],
			['', '']
		]);
		g = addColumn(g);
		expect(g.every((r) => r.length === 3)).toBe(true);
	});

	it('serialize→parse round-trips grid cell values', () => {
		fc.assert(
			fc.property(
				fc.array(
					fc.array(fc.stringMatching(/^[a-zA-Z0-9 ]*$/), { minLength: 1, maxLength: 4 }),
					{ minLength: 1, maxLength: 4 }
				),
				(rows) => {
					// Normalize to a rectangle the way the editor would.
					const width = rows.reduce((w, r) => Math.max(w, r.length), 1);
					const grid = rows.map((r) => {
						const row = r.map((c) => c.trim());
						while (row.length < width) row.push('');
						return row;
					});
					const serialized = serializeGrid(grid);
					if (serialized === '') {
						// All-empty grid serializes to empty; parse gives back a single cell.
						expect(grid.flat().every((c) => c === '')).toBe(true);
						return;
					}
					expect(parseGrid(serialized)).toEqual(grid);
				}
			),
			{ numRuns: 150 }
		);
	});
});
