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
	// Type dispatcher for a Polygon (Req 10.1). Renders the view matching the
	// Polygon's type, defaulting to the Markdown_Page view for the common case.

	import type { Polygon } from '$lib/polygonApi';
	import MarkdownPage from './MarkdownPage.svelte';
	import FolderPolygon from './FolderPolygon.svelte';
	import TabularPolygon from './TabularPolygon.svelte';
	import WhiteboardPolygon from './WhiteboardPolygon.svelte';

	interface Props {
		polygon: Polygon;
		linkCandidates?: { recordId: string; name: string }[];
		onchange?: (content: string) => void;
	}

	let { polygon, linkCandidates = [], onchange }: Props = $props();
</script>

{#if polygon.type === 'Folder_Polygon'}
	<FolderPolygon {polygon} />
{:else if polygon.type === 'Tabular_Polygon'}
	<TabularPolygon {polygon} {onchange} />
{:else if polygon.type === 'Whiteboard_Polygon'}
	<WhiteboardPolygon {polygon} />
{:else}
	<MarkdownPage {polygon} {linkCandidates} />
{/if}
