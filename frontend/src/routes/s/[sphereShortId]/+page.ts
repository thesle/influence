// Influence — a self-hostable documentation platform.
// Copyright (C) 2026  Conrad Smith
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

// Sphere page load (Req 21.1, 15.2). The route param is the Sphere's Short-UUID
// Record_ID (Req 15.2); we load the Sphere's Facets and Polygons and assemble
// the same tree the left menu uses, so the main content area lists the Sphere's
// contents and shows an empty-contents state when it has none (Req 21.3).
import { InfluenceApi } from '$lib/api';
import { buildSphereTree } from '$lib/components/nav/tree';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ params, fetch, parent }) => {
	const api = new InfluenceApi(fetch);
	const shortId = params.sphereShortId;

	// Resolve the Sphere from the parent layout's already-loaded list so a rename
	// never changes the URL and we avoid an extra fetch (Req 15.2, 15.3).
	const { spheres } = await parent();
	const sphere =
		spheres.find((s) => s.shortId === shortId || s.recordId === shortId) ?? null;

	const sphereId = sphere?.recordId ?? shortId;

	let tree: ReturnType<typeof buildSphereTree> | null = null;
	let contentError: string | null = null;
	try {
		const [facets, polygons] = await Promise.all([
			api.listFacets(sphereId),
			api.listPolygons(sphereId)
		]);
		tree = buildSphereTree(facets, polygons);
	} catch (err) {
		contentError = err instanceof Error ? err.message : 'Failed to load Sphere contents.';
	}

	return { sphere, shortId, tree, contentError };
};
