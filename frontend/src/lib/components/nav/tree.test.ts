import { describe, expect, test } from 'vitest';
import fc from 'fast-check';
import type { Facet, Polygon } from '$lib/api';
import { buildSphereTree, isEmptyContents, type FacetNode } from './tree';

function facet(recordId: string, name: string, parentRecordId?: string | null): Facet {
	return { recordId, name, sphereRecordId: 'sph', parentRecordId: parentRecordId ?? null };
}

function polygon(recordId: string, facetRecordId?: string | null): Polygon {
	return {
		recordId,
		sphereRecordId: 'sph',
		facetRecordId: facetRecordId ?? null,
		type: 'markdown',
		content: '',
		editMode: 'section',
		authorUserId: 1,
		createdAt: '2024-01-01T00:00:00Z',
		updatedAt: '2024-01-01T00:00:00Z'
	};
}

function countNodes(nodes: FacetNode[]): number {
	return nodes.reduce((n, node) => n + 1 + countNodes(node.children), 0);
}

function countPolygons(nodes: FacetNode[]): number {
	return nodes.reduce((n, node) => n + node.polygons.length + countPolygons(node.children), 0);
}

describe('buildSphereTree — Req 21.2/21.3', () => {
	test('links child facets under parents and polygons under owning facets', () => {
		const facets = [facet('f1', 'Root'), facet('f2', 'Child', 'f1')];
		const polygons = [polygon('p1', 'f2'), polygon('p2', 'f1')];
		const tree = buildSphereTree(facets, polygons);

		expect(tree.roots).toHaveLength(1);
		expect(tree.roots[0].facet.recordId).toBe('f1');
		expect(tree.roots[0].polygons.map((p) => p.recordId)).toEqual(['p2']);
		expect(tree.roots[0].children).toHaveLength(1);
		expect(tree.roots[0].children[0].polygons.map((p) => p.recordId)).toEqual(['p1']);
		expect(tree.loosePolygons).toHaveLength(0);
	});

	test('polygons without an owning facet become loose polygons', () => {
		const tree = buildSphereTree([], [polygon('p1'), polygon('p2')]);
		expect(tree.roots).toHaveLength(0);
		expect(tree.loosePolygons.map((p) => p.recordId)).toEqual(['p1', 'p2']);
	});

	test('facet with unknown parent is treated as a root (nothing dropped)', () => {
		const tree = buildSphereTree([facet('f1', 'Orphan', 'missing')], []);
		expect(tree.roots.map((n) => n.facet.recordId)).toEqual(['f1']);
	});

	test('empty sphere reports empty contents (Req 21.3)', () => {
		expect(isEmptyContents(buildSphereTree([], []))).toBe(true);
		expect(isEmptyContents(buildSphereTree([facet('f1', 'X')], []))).toBe(false);
		expect(isEmptyContents(buildSphereTree([], [polygon('p1')]))).toBe(false);
	});

	test('sibling facets are ordered case-insensitively by name then Record_ID', () => {
		const facets = [
			facet('b', 'bravo'),
			facet('a', 'Bravo'),
			facet('m', 'alpha')
		];
		const tree = buildSphereTree(facets, []);
		expect(tree.roots.map((n) => n.facet.recordId)).toEqual(['m', 'a', 'b']);
	});

	// Property: the assembled forest preserves every facet and polygon exactly
	// once, and empty detection matches the raw input emptiness.
	test('property: tree preserves all facets and polygons; cycles never hang', () => {
		const idArb = fc.string({ minLength: 1, maxLength: 5 });
		fc.assert(
			fc.property(
				fc.uniqueArray(idArb, { maxLength: 12 }),
				fc.array(idArb, { maxLength: 20 }),
				(facetIds, parentPicks) => {
					// Facets: each optionally points to some (possibly cyclic/unknown) parent.
					const facets: Facet[] = facetIds.map((id, i) => {
						const parent = parentPicks[i];
						return facet(id, `name-${id}`, parent && parent !== id ? parent : null);
					});
					// Polygons: some attached to a facet id, some loose (unknown/absent facet).
					const polygons: Polygon[] = parentPicks
						.slice(0, 15)
						.map((pick, i) => polygon(`p${i}`, pick || null));

					const tree = buildSphereTree(facets, polygons);

					// Every facet appears exactly once in the forest.
					expect(countNodes(tree.roots)).toBe(facets.length);

					// Every polygon appears exactly once (in a facet or loose).
					const inFacets = countPolygons(tree.roots);
					expect(inFacets + tree.loosePolygons.length).toBe(polygons.length);

					// Empty detection matches raw emptiness.
					expect(isEmptyContents(tree)).toBe(facets.length === 0 && polygons.length === 0);
				}
			),
			{ numRuns: 200 }
		);
	});
});
