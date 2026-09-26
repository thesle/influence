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
	// Recursive Facet/Polygon subtree for the left menu (Req 21.2). Renders a
	// list of Facet nodes: each shows its child Facets (recursively) and the
	// Polygons directly under it. Polygon links are built from Record_IDs
	// (Req 15.2) via `polygonPath`.
	import { polygonPath } from '$lib/api';
	import type { FacetNode } from './tree';
	import Self from './FacetPolygonTree.svelte';

	let { nodes, activePolygonId = null }: { nodes: FacetNode[]; activePolygonId?: string | null } =
		$props();
</script>

<ul class="tree">
	{#each nodes as node (node.facet.recordId)}
		<li class="facet">
			<span class="facet__name">{node.facet.name}</span>
			{#if node.polygons.length > 0}
				<ul class="polygons">
					{#each node.polygons as polygon (polygon.recordId)}
						<li>
							<a
								class="polygon"
								class:polygon--active={polygon.recordId === activePolygonId}
								href={polygonPath(polygon.recordId)}
								aria-current={polygon.recordId === activePolygonId ? 'page' : undefined}
							>
								{polygon.content ? polygon.content.slice(0, 40) : polygon.recordId}
							</a>
						</li>
					{/each}
				</ul>
			{/if}
			{#if node.children.length > 0}
				<Self nodes={node.children} {activePolygonId} />
			{/if}
		</li>
	{/each}
</ul>

<style>
	.tree {
		list-style: none;
		margin: 0;
		padding-left: 0.75rem;
	}

	.facet__name {
		display: block;
		font-weight: 600;
		font-size: 0.85rem;
		color: #3e4c59;
		padding: 0.15rem 0;
	}

	.polygons {
		list-style: none;
		margin: 0;
		padding-left: 0.75rem;
	}

	.polygon {
		display: block;
		padding: 0.1rem 0.25rem;
		font-size: 0.85rem;
		color: #486581;
		text-decoration: none;
		border-radius: 3px;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.polygon:hover {
		background: #e4e7eb;
	}

	.polygon--active {
		background: #d9e2ec;
		font-weight: 600;
	}
</style>
