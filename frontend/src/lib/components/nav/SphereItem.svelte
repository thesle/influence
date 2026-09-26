<script lang="ts">
	// A single Sphere row in the left menu. Selecting the Sphere expands it to
	// show its Facet/Polygon tree (Req 21.2); an empty Sphere shows the
	// empty-contents indicator (Req 21.3). Facets and Polygons are loaded lazily
	// on first expand via the API client and assembled with buildSphereTree.
	import { InfluenceApi, spherePath, type Facet, type Polygon, type Sphere } from '$lib/api';
	import { buildSphereTree, isEmptyContents, type SphereTree } from './tree';
	import FacetPolygonTree from './FacetPolygonTree.svelte';
	import EmptyContents from './EmptyContents.svelte';

	let {
		sphere,
		circled = false,
		expanded = false,
		activePolygonId = null,
		api = new InfluenceApi(),
		onToggle
	}: {
		sphere: Sphere;
		circled?: boolean;
		expanded?: boolean;
		activePolygonId?: string | null;
		api?: InfluenceApi;
		onToggle?: (sphere: Sphere) => void;
	} = $props();

	let tree = $state<SphereTree | null>(null);
	let loading = $state(false);
	let loadError = $state<string | null>(null);
	let loaded = $state(false);

	async function loadContents() {
		if (loaded || loading) return;
		loading = true;
		loadError = null;
		try {
			const [facets, polygons]: [Facet[], Polygon[]] = await Promise.all([
				api.listFacets(sphere.recordId),
				api.listPolygons(sphere.recordId)
			]);
			tree = buildSphereTree(facets, polygons);
			loaded = true;
		} catch (err) {
			loadError = err instanceof Error ? err.message : 'Failed to load Sphere contents.';
		} finally {
			loading = false;
		}
	}

	// Load contents the first time the Sphere becomes expanded.
	$effect(() => {
		if (expanded && !loaded && !loading) {
			void loadContents();
		}
	});

	function toggle() {
		onToggle?.(sphere);
	}
</script>

<div class="sphere" class:sphere--circled={circled}>
	<div class="sphere__header">
		<button
			type="button"
			class="sphere__toggle"
			aria-expanded={expanded}
			aria-label={expanded ? `Collapse ${sphere.name}` : `Expand ${sphere.name}`}
			onclick={toggle}
		>
			<span class="sphere__caret" aria-hidden="true">{expanded ? '▾' : '▸'}</span>
		</button>
		<a class="sphere__link" href={spherePath(sphere)}>{sphere.name}</a>
	</div>

	{#if expanded}
		<div class="sphere__body">
			{#if loading}
				<p class="sphere__status" role="status">Loading…</p>
			{:else if loadError}
				<p class="sphere__status sphere__status--error" role="alert">{loadError}</p>
			{:else if tree}
				{#if isEmptyContents(tree)}
					<EmptyContents />
				{:else}
					{#if tree.roots.length > 0}
						<FacetPolygonTree nodes={tree.roots} {activePolygonId} />
					{/if}
					{#if tree.loosePolygons.length > 0}
						<FacetPolygonTree
							nodes={[
								{
									facet: {
										recordId: `${sphere.recordId}:__root__`,
										sphereRecordId: sphere.recordId,
										name: 'Ungrouped'
									},
									children: [],
									polygons: tree.loosePolygons
								}
							]}
							{activePolygonId}
						/>
					{/if}
				{/if}
			{/if}
		</div>
	{/if}
</div>

<style>
	.sphere {
		margin-bottom: 0.15rem;
	}

	.sphere__header {
		display: flex;
		align-items: center;
		gap: 0.25rem;
	}

	.sphere__toggle {
		border: none;
		background: transparent;
		cursor: pointer;
		padding: 0.1rem 0.25rem;
		color: #52606d;
		font-size: 0.75rem;
		line-height: 1;
	}

	.sphere__caret {
		display: inline-block;
		width: 0.75rem;
	}

	.sphere__link {
		flex: 1;
		color: #1f2933;
		text-decoration: none;
		font-size: 0.95rem;
		padding: 0.15rem 0;
	}

	.sphere--circled .sphere__link {
		font-weight: 600;
	}

	.sphere__link:hover {
		text-decoration: underline;
	}

	.sphere__body {
		padding-left: 0.5rem;
	}

	.sphere__status {
		margin: 0.25rem 0 0.25rem 1rem;
		font-size: 0.85rem;
		color: #7b8794;
	}

	.sphere__status--error {
		color: #cf1124;
	}
</style>
