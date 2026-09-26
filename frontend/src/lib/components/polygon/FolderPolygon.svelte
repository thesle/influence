<!--
  Influence — a self-hostable documentation platform.
  Copyright (C) 2026  Conrad Smith

  This program is free software: you can redistribute it and/or modify
  it under the terms of the GNU General Public License as published by
  the Free Software Foundation, version 3.

  This program is distributed in the hope that it will be useful,
  but WITHOUT ANY WARRANTY; without even the implied warranty of
  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
  GNU General Public License for more details.

  You should have received a copy of the GNU General Public License
  along with this program.  If not, see <https://www.gnu.org/licenses/>.
-->

<script lang="ts">
	// Folder_Polygon view (Req 10.1). A folder contains other Polygons; this view
	// shows its direct children — the same set `@children` expands to at render
	// (Req 11.7). An empty folder shows an empty-children indicator (Req 11.8).

	import * as api from '$lib/polygonApi';

	interface Props {
		polygon: api.Polygon;
	}

	let { polygon }: Props = $props();

	let children = $state<api.Polygon[] | null>(null);
	let error = $state('');

	$effect(() => {
		if (children === null && error === '') {
			api
				.listChildren(polygon)
				.then((c) => (children = c))
				.catch((e) => (error = e instanceof Error ? e.message : 'Failed to load children.'));
		}
	});

	function typeLabel(t: api.PolygonType): string {
		return t.replace(/_/g, ' ');
	}
</script>

<section class="folder" data-testid="folder-polygon">
	<h2>Folder contents</h2>
	{#if error}
		<p class="error">{error}</p>
	{:else if children === null}
		<p class="loading">Loading…</p>
	{:else if children.length === 0}
		<p class="empty" data-testid="empty-children">This folder has no Polygons yet.</p>
	{:else}
		<ul class="children" data-testid="children-list">
			{#each children as child (child.recordId)}
				<li>
					<a href={`/p/${encodeURIComponent(child.recordId)}`}>
						<span class="child-type">{typeLabel(child.type)}</span>
						<span class="child-id">{child.recordId}</span>
					</a>
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	.children {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.35rem;
	}
	.children a {
		display: flex;
		gap: 0.75rem;
		align-items: baseline;
		padding: 0.4rem 0.6rem;
		border: 1px solid #e4e7eb;
		border-radius: 4px;
		text-decoration: none;
		color: inherit;
	}
	.children a:hover {
		background: #f0f4f8;
	}
	.child-type {
		font-weight: 600;
	}
	.child-id {
		color: #7b8794;
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
		font-size: 0.8rem;
	}
	.empty {
		color: #7b8794;
	}
	.error {
		color: #b91c1c;
	}
</style>
