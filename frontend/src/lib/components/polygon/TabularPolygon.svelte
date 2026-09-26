<script lang="ts">
	// Tabular_Polygon view (Req 10.1): an editable grid. The grid is parsed from
	// the stored markdown pipe table and serialized back on change, so content
	// stays markdown-first and round-trips.

	import * as api from '$lib/polygonApi';
	import { parseGrid, serializeGrid, addRow, addColumn, type Grid } from './tabular';

	interface Props {
		polygon: api.Polygon;
		onchange?: (content: string) => void;
	}

	let { polygon, onchange }: Props = $props();

	// svelte-ignore state_referenced_locally
	let grid = $state<Grid>(parseGrid(polygon.content));

	function commit() {
		onchange?.(serializeGrid(grid));
	}

	function setCell(r: number, c: number, value: string) {
		grid[r][c] = value;
		grid = grid; // trigger reactivity for the nested mutation
		commit();
	}

	function onAddRow() {
		grid = addRow(grid);
		commit();
	}
	function onAddColumn() {
		grid = addColumn(grid);
		commit();
	}
</script>

<section class="tabular" data-testid="tabular-polygon">
	<div class="controls">
		<button type="button" onclick={onAddRow}>Add row</button>
		<button type="button" onclick={onAddColumn}>Add column</button>
	</div>
	<table>
		<tbody>
			{#each grid as row, r (r)}
				<tr>
					{#each row as cell, c (c)}
						<td>
							<input
								type="text"
								value={cell}
								aria-label={`Row ${r + 1} column ${c + 1}`}
								oninput={(e) => setCell(r, c, (e.currentTarget as HTMLInputElement).value)}
							/>
						</td>
					{/each}
				</tr>
			{/each}
		</tbody>
	</table>
</section>

<style>
	.controls {
		display: flex;
		gap: 0.5rem;
		margin-bottom: 0.5rem;
	}
	.controls button {
		border: 1px solid #cbd2d9;
		background: #fff;
		border-radius: 4px;
		padding: 0.3rem 0.75rem;
		cursor: pointer;
	}
	table {
		border-collapse: collapse;
	}
	td {
		border: 1px solid #cbd2d9;
		padding: 0;
	}
	input {
		border: none;
		padding: 0.35rem 0.5rem;
		min-width: 8rem;
		font: inherit;
	}
	input:focus {
		outline: 2px solid #2563eb;
		outline-offset: -2px;
	}
</style>
