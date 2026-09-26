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
	// The app-shell top bar (design.md — "Shell / layout"). It provides the four
	// regions called for in the design — search, quick searches, bookmarks menu,
	// and user menu — as named snippet slots so the components that own those
	// features (search/quick in 23.2, bookmarks/user in 23.3) can fill them
	// without this shell taking a dependency on them.
	import type { Snippet } from 'svelte';

	let {
		search,
		quickSearches,
		bookmarks,
		userMenu
	}: {
		search?: Snippet;
		quickSearches?: Snippet;
		bookmarks?: Snippet;
		userMenu?: Snippet;
	} = $props();
</script>

<header class="top-bar">
	<a class="brand" href="/">Influence</a>

	<div class="top-bar__region top-bar__search" aria-label="Search">
		{#if search}{@render search()}{:else}
			<div class="placeholder" data-slot="search">Search</div>
		{/if}
	</div>

	<div class="top-bar__region top-bar__quick" aria-label="Quick searches">
		{#if quickSearches}{@render quickSearches()}{:else}
			<div class="placeholder" data-slot="quick-searches">Quick searches</div>
		{/if}
	</div>

	<div class="top-bar__spacer"></div>

	<div class="top-bar__region top-bar__bookmarks" aria-label="Bookmarks">
		{#if bookmarks}{@render bookmarks()}{:else}
			<div class="placeholder" data-slot="bookmarks">Bookmarks</div>
		{/if}
	</div>

	<div class="top-bar__region top-bar__user" aria-label="User menu">
		{#if userMenu}{@render userMenu()}{:else}
			<div class="placeholder" data-slot="user-menu">Account</div>
		{/if}
	</div>
</header>

<style>
	.top-bar {
		display: flex;
		align-items: center;
		gap: 1rem;
		padding: 0.5rem 1rem;
		background: #1f2933;
		color: #fff;
	}

	.brand {
		font-weight: 600;
		color: #fff;
		text-decoration: none;
	}

	.top-bar__spacer {
		flex: 1;
	}

	.top-bar__region {
		display: flex;
		align-items: center;
	}

	.placeholder {
		font-size: 0.85rem;
		color: #cbd2d9;
		border: 1px dashed #52606d;
		border-radius: 4px;
		padding: 0.2rem 0.5rem;
	}
</style>
