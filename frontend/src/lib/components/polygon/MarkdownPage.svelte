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
	// Markdown_Page view (Req 10.1, 12.1, 12.2).
	//
	// Two panes: the authoritative server-rendered rich view (masked, with
	// resolved Cross-Refraction links and the metadata footer per Req 14, 16.3,
	// 18) and the section editor. After a section edit is confirmed the server
	// render is refetched so the rich view reflects the change.

	import MarkdownEditor from './MarkdownEditor.svelte';
	import * as api from '$lib/polygonApi';

	interface Props {
		polygon: api.Polygon;
		linkCandidates?: { recordId: string; name: string }[];
	}

	let { polygon, linkCandidates = [] }: Props = $props();

	// svelte-ignore state_referenced_locally
	let content = $state(polygon.content);
	let rendered = $state<api.RenderResult | null>(null);
	let renderError = $state('');
	let editing = $state(false);

	async function refreshRender() {
		renderError = '';
		try {
			rendered = await api.renderPolygon(polygon.recordId);
		} catch (e) {
			renderError = e instanceof Error ? e.message : 'Render failed.';
		}
	}

	// Load the server render once on mount.
	$effect(() => {
		if (rendered === null && renderError === '') {
			refreshRender();
		}
	});

	function onEditorChange(next: string) {
		content = next;
		// The editor only rewrites in-memory content here; a full save flow
		// (PATCH) is owned by the shell. Refresh the server render so the rich
		// view reflects resolved links/masking for the new content.
		refreshRender();
	}
</script>

<article class="markdown-page" data-testid="markdown-page">
	<div class="tabs">
		<button type="button" class:active={!editing} onclick={() => (editing = false)}>View</button>
		<button type="button" class:active={editing} onclick={() => (editing = true)}>Edit</button>
	</div>

	{#if editing}
		<MarkdownEditor
			polygonId={polygon.recordId}
			bind:content
			{linkCandidates}
			onchange={onEditorChange}
		/>
	{:else if renderError}
		<p class="error">{renderError}</p>
	{:else if rendered}
		<!-- eslint-disable-next-line svelte/no-at-html-tags -->
		<div class="rendered">{@html rendered.html}</div>
		{#if rendered.footer}
			<footer class="meta" data-testid="metadata-footer">
				<span>By {rendered.footer.authorDisplayName}</span>
				<span>Created {rendered.footer.createdAt}</span>
				<span>Updated {rendered.footer.updatedAt}</span>
			</footer>
		{/if}
	{:else}
		<p class="loading">Loading…</p>
	{/if}
</article>

<style>
	.tabs {
		display: flex;
		gap: 0.25rem;
		margin-bottom: 0.75rem;
	}
	.tabs button {
		border: 1px solid #cbd2d9;
		background: #fff;
		padding: 0.3rem 0.9rem;
		cursor: pointer;
		border-radius: 4px;
	}
	.tabs button.active {
		background: #2563eb;
		color: #fff;
		border-color: #2563eb;
	}
	.rendered {
		line-height: 1.55;
	}
	.meta {
		margin-top: 1.5rem;
		padding-top: 0.75rem;
		border-top: 1px solid #e4e7eb;
		display: flex;
		gap: 1rem;
		color: #7b8794;
		font-size: 0.85rem;
	}
	.error {
		color: #b91c1c;
	}
</style>
