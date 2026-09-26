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

// Facet/Polygon tree assembly for the left menu (Req 21.2, 21.3).
//
// A selected Sphere expands into a tree of its Facets with the Polygons that
// belong to each Facet (Req 21.2). Facets form a single-parent, acyclic
// hierarchy (Req 8, 9); Polygons hang off a Facet via `facetRecordId`, or off
// the Sphere root when they have no Facet. When a Sphere has neither Facets nor
// Polygons the UI shows an empty-contents indicator (Req 21.3), which callers
// detect via `isEmptyContents`.

import type { Facet, Polygon } from '$lib/api';

/** A node in the left-menu Facet/Polygon tree. */
export interface FacetNode {
	facet: Facet;
	/** Child Facets, ordered by name (case-insensitive) then Record_ID. */
	children: FacetNode[];
	/** Polygons directly under this Facet, ordered by Record_ID for stability. */
	polygons: Polygon[];
}

/** The assembled contents of a Sphere for the left menu. */
export interface SphereTree {
	/** Top-level Facets (those with no parent). */
	roots: FacetNode[];
	/** Polygons that sit at the Sphere root (no owning Facet). */
	loosePolygons: Polygon[];
}

function byNameThenId(a: Facet, b: Facet): number {
	const byName = a.name.localeCompare(b.name, undefined, { sensitivity: 'accent' });
	if (byName !== 0) return byName;
	if (a.recordId < b.recordId) return -1;
	if (a.recordId > b.recordId) return 1;
	return 0;
}

function byRecordId(a: Polygon, b: Polygon): number {
	if (a.recordId < b.recordId) return -1;
	if (a.recordId > b.recordId) return 1;
	return 0;
}

function parentId(facet: Facet): string | null {
	return facet.parentRecordId ?? null;
}

function facetIdOf(polygon: Polygon): string | null {
	return polygon.facetRecordId ?? null;
}

/**
 * Build the Facet/Polygon tree for a Sphere (Req 21.2).
 *
 * Facets are linked to their parents; any Facet whose declared parent is not in
 * the set is treated as a root so nothing is silently dropped. Cycles (which the
 * backend prevents — Req 9) are broken defensively: a node is attached to at
 * most one parent and is never revisited, so the output is always a finite
 * forest.
 */
export function buildSphereTree(facets: readonly Facet[], polygons: readonly Polygon[]): SphereTree {
	const nodes = new Map<string, FacetNode>();
	for (const facet of facets) {
		nodes.set(facet.recordId, { facet, children: [], polygons: [] });
	}

	// Attach Polygons to their owning Facet, or collect the loose ones.
	const loosePolygons: Polygon[] = [];
	for (const polygon of polygons) {
		const fid = facetIdOf(polygon);
		const owner = fid ? nodes.get(fid) : undefined;
		if (owner) {
			owner.polygons.push(polygon);
		} else {
			loosePolygons.push(polygon);
		}
	}

	// Link Facets to parents; a missing/unknown parent makes the node a root.
	const roots: FacetNode[] = [];
	for (const node of nodes.values()) {
		const pid = parentId(node.facet);
		const parent = pid ? nodes.get(pid) : undefined;
		if (parent && parent !== node) {
			parent.children.push(node);
		} else {
			roots.push(node);
		}
	}

	// Stable, name-based ordering throughout.
	const sortNode = (node: FacetNode) => {
		node.children.sort((a, b) => byNameThenId(a.facet, b.facet));
		node.polygons.sort(byRecordId);
		node.children.forEach(sortNode);
	};
	roots.sort((a, b) => byNameThenId(a.facet, b.facet));
	roots.forEach(sortNode);
	loosePolygons.sort(byRecordId);

	return { roots, loosePolygons };
}

/**
 * Whether a Sphere has no Facets and no Polygons, in which case the left menu
 * shows an empty-contents indicator (Req 21.3).
 */
export function isEmptyContents(tree: SphereTree): boolean {
	return tree.roots.length === 0 && tree.loosePolygons.length === 0;
}
