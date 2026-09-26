<script lang="ts">
	// In-app documentation viewer (task 23.3, Req 27.2, 27.3). Fetches the
	// requested document as markdown from GET /api/docs/{path}, renders it in-app
	// with the lightweight markdown renderer, and shows an "unavailable"
	// indication when the document cannot be loaded (Req 27.3).
	//
	// The document path comes from the catch-all route param, so /docs/a/b/c
	// requests the doc "a/b/c". Fetching runs on mount and re-runs whenever the
	// path changes (client-side navigation between docs).

	import { page } from '$app/stores';
	import { adminApi } from '$lib/adminApi';
	import { renderMarkdown } from '$lib/markdown';

	type State =
		| { kind: 'loading' }
		| { kind: 'loaded'; html: string }
		| { kind: 'unavailable' };

	let state = $state<State>({ kind: 'loading' });

	// The current doc path from the catch-all param (may be empty for /docs/).
	const docPath = $derived($page.params.path ?? '');

	async function loadDoc(path: string) {
		state = { kind: 'loading' };
		if (path === '') {
			state = { kind: 'unavailable' };
			return;
		}
		try {
			const doc = await adminApi.getDoc(path);
			state = { kind: 'loaded', html: renderMarkdown(doc.markdown) };
		} catch {
			// Any failure (missing doc, network, non-2xx) is an unavailable doc.
			state = { kind: 'unavailable' };
		}
	}

	// Re-fetch whenever the path changes (covers initial mount and navigation).
	$effect(() => {
		void loadDoc(docPath);
	});
</script>

<article class="doc">
	<nav class="doc__breadcrumb" aria-label="Documentation">
		<a href="/docs">Documentation</a>
		{#if docPath}<span aria-hidden="true"> / </span><span>{docPath}</span>{/if}
	</nav>

	{#if state.kind === 'loading'}
		<p role="status">Loading documentation…</p>
	{:else if state.kind === 'unavailable'}
		<p class="doc__unavailable" role="alert" data-testid="doc-unavailable">
			This documentation could not be loaded.
		</p>
	{:else}
		<!-- Rendered from trusted platform markdown; renderMarkdown escapes all text. -->
		<div class="doc__body">{@html state.html}</div>
	{/if}
</article>

<style>
	.doc {
		max-width: 48rem;
	}

	.doc__breadcrumb {
		font-size: 0.85rem;
		color: #7b8794;
		margin-bottom: 1rem;
	}

	.doc__breadcrumb a {
		color: #52606d;
	}

	.doc__unavailable {
		color: #a4262c;
	}

	.doc__body :global(pre) {
		background: #f5f7fa;
		padding: 0.75rem;
		border-radius: 4px;
		overflow-x: auto;
	}

	.doc__body :global(code) {
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
	}
</style>
