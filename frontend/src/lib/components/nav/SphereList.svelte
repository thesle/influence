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
	// The left-menu Sphere list (Req 21.1). Lists only the accessible Spheres the
	// caller was given, ordered per Req 7: the circled group (reorderable, in the
	// user's order) shown ABOVE the alphabetical remainder (Req 7.3, 7.5). A
	// single Sphere may be expanded at a time to reveal its Facet/Polygon tree
	// (Req 21.2).
	import { InfluenceApi, type Sphere } from '$lib/api';
	import { orderSpheres, type CircleContext } from './order';
	import SphereItem from './SphereItem.svelte';

	let {
		spheres,
		circleContext = {},
		activeSphereId = null,
		activePolygonId = null,
		api = new InfluenceApi()
	}: {
		spheres: Sphere[];
		circleContext?: CircleContext;
		activeSphereId?: string | null;
		activePolygonId?: string | null;
		api?: InfluenceApi;
	} = $props();

	const ordered = $derived(orderSpheres(spheres, circleContext));

	// The currently expanded Sphere. A click toggles selection; a deep link to a
	// Sphere expands it via the effect below so the routed Sphere opens its tree.
	let expandedId = $state<string | null>(null);
	let userToggled = $state(false);

	$effect(() => {
		// Follow the routed Sphere until the user makes an explicit selection.
		if (activeSphereId && !userToggled) {
			expandedId = activeSphereId;
		}
	});

	function toggle(sphere: Sphere) {
		userToggled = true;
		expandedId = expandedId === sphere.recordId ? null : sphere.recordId;
	}
</script>

<div class="sphere-list">
	{#if ordered.circled.length > 0}
		<section class="group" aria-label="Circled Spheres">
			<h2 class="group__label">Circled</h2>
			<ul class="group__items">
				{#each ordered.circled as sphere (sphere.recordId)}
					<li>
						<SphereItem
							{sphere}
							circled={true}
							expanded={expandedId === sphere.recordId}
							{activePolygonId}
							{api}
							onToggle={toggle}
						/>
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	<section class="group" aria-label="Spheres">
		{#if ordered.circled.length > 0}
			<h2 class="group__label">All Spheres</h2>
		{/if}
		{#if ordered.alphabetical.length === 0 && ordered.circled.length === 0}
			<p class="group__empty" role="note">No accessible Spheres.</p>
		{:else}
			<ul class="group__items">
				{#each ordered.alphabetical as sphere (sphere.recordId)}
					<li>
						<SphereItem
							{sphere}
							expanded={expandedId === sphere.recordId}
							{activePolygonId}
							{api}
							onToggle={toggle}
						/>
					</li>
				{/each}
			</ul>
		{/if}
	</section>
</div>

<style>
	.sphere-list {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.group__label {
		margin: 0 0 0.25rem;
		font-size: 0.7rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #9aa5b1;
		font-weight: 700;
	}

	.group__items {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.group__empty {
		margin: 0;
		font-size: 0.85rem;
		color: #7b8794;
		font-style: italic;
	}
</style>
