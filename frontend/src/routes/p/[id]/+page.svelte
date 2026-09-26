<script lang="ts">
	// Polygon-detail route (owned by task 23.2). Routes are built from Short-UUID
	// Record_IDs (Req 15.2); this loads the Polygon for the id in the path and
	// dispatches to the type-specific view. Link candidates for the `@link`
	// picker are the other Polygons in the same Sphere (Req 11.2).

	import { page } from '$app/stores';
	import * as api from '$lib/polygonApi';
	import PolygonView from '$lib/components/polygon/PolygonView.svelte';

	let polygon = $state<api.Polygon | null>(null);
	let linkCandidates = $state<{ recordId: string; name: string }[]>([]);
	let error = $state('');
	let loadedId = $state('');

	async function load(id: string) {
		error = '';
		polygon = null;
		try {
			const p = await api.getPolygon(id);
			polygon = p;
			// Other Polygons in the Sphere become link candidates (Req 11.2). The
			// backend list has no display name field for Polygons, so the id doubles
			// as the label until a name is available.
			try {
				const siblings = await api.listSpherePolygons(p.sphereRecordId);
				linkCandidates = siblings
					.filter((s) => s.recordId !== p.recordId)
					.map((s) => ({ recordId: s.recordId, name: s.recordId }));
			} catch {
				linkCandidates = [];
			}
		} catch (e) {
			error = e instanceof api.ApiError ? e.message : 'Failed to load Polygon.';
		}
	}

	// React to route id changes.
	$effect(() => {
		const id = $page.params.id;
		if (id && id !== loadedId) {
			loadedId = id;
			load(id);
		}
	});
</script>

<svelte:head>
	<title>Polygon · Influence</title>
</svelte:head>

<div class="polygon-detail">
	{#if error}
		<p class="error" data-testid="load-error">{error}</p>
	{:else if polygon}
		<PolygonView {polygon} {linkCandidates} />
	{:else}
		<p class="loading">Loading Polygon…</p>
	{/if}
</div>

<style>
	.polygon-detail {
		max-width: 900px;
	}
	.error {
		color: #b91c1c;
	}
	.loading {
		color: #7b8794;
	}
</style>
