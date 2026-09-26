<script lang="ts">
	// Bookmarks menu (task 23.3, Req 26.3, 26.4). Lists the user's bookmarked
	// Polygons grouped by their owning Sphere, with an empty-state indicator when
	// the user has no bookmarks. Data comes from GET /api/bookmarks (already
	// grouped server-side); normalizeBookmarks drops empty groups and derives the
	// empty flag so the menu never shows a Sphere header with nothing under it.
	//
	// This is a self-contained component (it does not touch the shell layout or
	// nav components owned by task 23.1). It loads on mount and exposes a
	// `reload` via re-fetching after a bookmark is removed (Req 26.5).

	import { onMount } from 'svelte';
	import {
		AdminApi,
		adminApi,
		ApiError,
		normalizeBookmarks,
		type BookmarkGroup,
		type PolygonSummary
	} from '$lib/adminApi';

	// Allow tests / callers to inject a client; default to the shared instance.
	let { client = adminApi }: { client?: AdminApi } = $props();

	let loading = $state(true);
	let errorMessage = $state<string | null>(null);
	let groups = $state<BookmarkGroup[]>([]);
	let empty = $state(false);

	async function load() {
		loading = true;
		errorMessage = null;
		try {
			const resp = await client.listBookmarks();
			const normalized = normalizeBookmarks(resp);
			groups = normalized.groups;
			empty = normalized.empty;
		} catch (err) {
			errorMessage =
				err instanceof ApiError ? err.message : 'Could not load bookmarks.';
			groups = [];
			empty = false;
		} finally {
			loading = false;
		}
	}

	async function remove(polygon: PolygonSummary) {
		try {
			await client.removeBookmark(polygon.recordId);
			await load();
		} catch (err) {
			errorMessage =
				err instanceof ApiError ? err.message : 'Could not remove bookmark.';
		}
	}

	// A short, human-friendly label for a bookmarked Polygon. Content may be a
	// long markdown body; use its first non-empty line, falling back to the id.
	function polygonLabel(p: PolygonSummary): string {
		const firstLine = (p.content ?? '')
			.split('\n')
			.map((l) => l.replace(/^#+\s*/, '').trim())
			.find((l) => l.length > 0);
		return firstLine && firstLine.length > 0 ? firstLine : p.recordId;
	}

	onMount(load);
</script>

<section class="bookmarks" aria-label="Bookmarks">
	<h2 class="bookmarks__title">Bookmarks</h2>

	{#if loading}
		<p class="bookmarks__status" role="status">Loading bookmarks…</p>
	{:else if errorMessage}
		<p class="bookmarks__error" role="alert">{errorMessage}</p>
	{:else if empty}
		<p class="bookmarks__empty" data-testid="bookmarks-empty">
			No bookmarks yet. Bookmark a Polygon to see it here.
		</p>
	{:else}
		<ul class="bookmarks__spheres">
			{#each groups as group (group.sphere.recordId)}
				<li class="bookmarks__sphere">
					<h3 class="bookmarks__sphere-name">{group.sphere.name}</h3>
					<ul class="bookmarks__polygons">
						{#each group.polygons as polygon (polygon.recordId)}
							<li class="bookmarks__polygon">
								<a
									class="bookmarks__link"
									href={`/p/${encodeURIComponent(polygon.recordId)}`}
								>
									{polygonLabel(polygon)}
								</a>
								<button
									type="button"
									class="bookmarks__remove"
									aria-label={`Remove bookmark ${polygonLabel(polygon)}`}
									onclick={() => remove(polygon)}
								>
									Remove
								</button>
							</li>
						{/each}
					</ul>
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	.bookmarks {
		min-width: 16rem;
	}

	.bookmarks__title {
		font-size: 1rem;
		margin: 0 0 0.5rem;
	}

	.bookmarks__status,
	.bookmarks__empty,
	.bookmarks__error {
		color: #52606d;
		font-size: 0.875rem;
		margin: 0.25rem 0;
	}

	.bookmarks__error {
		color: #a4262c;
	}

	.bookmarks__spheres,
	.bookmarks__polygons {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.bookmarks__sphere + .bookmarks__sphere {
		margin-top: 0.75rem;
	}

	.bookmarks__sphere-name {
		font-size: 0.8rem;
		text-transform: uppercase;
		letter-spacing: 0.03em;
		color: #7b8794;
		margin: 0 0 0.25rem;
	}

	.bookmarks__polygon {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.5rem;
		padding: 0.15rem 0;
	}

	.bookmarks__link {
		color: #1f2933;
		text-decoration: none;
	}

	.bookmarks__link:hover {
		text-decoration: underline;
	}

	.bookmarks__remove {
		background: none;
		border: none;
		color: #a4262c;
		cursor: pointer;
		font-size: 0.75rem;
		padding: 0.1rem 0.25rem;
	}
</style>
