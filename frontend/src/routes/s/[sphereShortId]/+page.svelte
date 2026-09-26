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
	// Sphere landing view: shows the selected Sphere's name and its Facet/Polygon
	// contents (Req 21.2), or an empty-contents indicator when it has none
	// (Req 21.3). Polygon views themselves are implemented separately (task
	// 23.2); this page lists the contents and links into them by Record_ID.
	import FacetPolygonTree from '$lib/components/nav/FacetPolygonTree.svelte';
	import EmptyContents from '$lib/components/nav/EmptyContents.svelte';
	import { isEmptyContents } from '$lib/components/nav/tree';

	let { data } = $props();
</script>

<section class="sphere-page">
	{#if data.sphere}
		<h1>{data.sphere.name}</h1>
	{:else}
		<h1>Sphere</h1>
		<p class="notice" role="note">
			This Sphere is not in your accessible list, or its identifier
			<code>{data.shortId}</code> was not found.
		</p>
	{/if}

	{#if data.contentError}
		<p class="error" role="alert">{data.contentError}</p>
	{:else if data.tree}
		{#if isEmptyContents(data.tree)}
			<EmptyContents />
		{:else}
			{#if data.tree.roots.length > 0}
				<FacetPolygonTree nodes={data.tree.roots} />
			{/if}
			{#if data.tree.loosePolygons.length > 0}
				<FacetPolygonTree
					nodes={[
						{
							facet: {
								recordId: `${data.sphere?.recordId ?? data.shortId}:__root__`,
								sphereRecordId: data.sphere?.recordId ?? data.shortId,
								name: 'Ungrouped'
							},
							children: [],
							polygons: data.tree.loosePolygons
						}
					]}
				/>
			{/if}
		{/if}
	{/if}
</section>

<style>
	.sphere-page h1 {
		margin-top: 0;
	}

	.notice {
		color: #7b8794;
	}

	.error {
		color: #cf1124;
	}
</style>
