import { describe, expect, test } from 'vitest';
import fc from 'fast-check';
import {
	InfluenceApi,
	polygonPath,
	spherePath,
	sphereShortId,
	type FetchLike,
	type Sphere
} from './api';

function jsonResponse(body: unknown, status = 200): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json' }
	});
}

describe('route builders — Req 15.2, 21.1', () => {
	test('spherePath uses the Short-UUID Record_ID', () => {
		const sphere = { shortId: 'abc123', recordId: 'canonical-uuid' } as Sphere;
		expect(spherePath(sphere)).toBe('/s/abc123');
	});

	test('spherePath falls back to canonical id when short is missing', () => {
		const sphere = { shortId: '', recordId: 'canonical-uuid' } as Sphere;
		expect(sphereShortId(sphere)).toBe('canonical-uuid');
		expect(spherePath(sphere)).toBe('/s/canonical-uuid');
	});

	test('polygonPath builds from Record_ID', () => {
		expect(polygonPath('poly-1')).toBe('/p/poly-1');
	});

	// Property: a Sphere route is always /s/{encoded shortId} — built from the
	// Short-UUID Record_ID (Req 15.2), never from the display name.
	test('property: spherePath is /s/{encoded shortId} regardless of name', () => {
		fc.assert(
			fc.property(
				fc.string({ minLength: 1, maxLength: 12 }),
				fc.string({ maxLength: 20 }),
				(shortId, name) => {
					const path = spherePath({ shortId, recordId: 'r', name } as Sphere);
					// URL is derived solely from the short id; the name is not an input.
					expect(path).toBe(`/s/${encodeURIComponent(shortId)}`);
				}
			),
			{ numRuns: 100 }
		);
	});
});

describe('InfluenceApi', () => {
	test('listSpheres parses the { spheres } envelope', async () => {
		const stub: FetchLike = async () =>
			jsonResponse({ spheres: [{ recordId: 'r', shortId: 's', name: 'S', createdAt: 't' }] });
		const api = new InfluenceApi(stub);
		const spheres = await api.listSpheres();
		expect(spheres).toHaveLength(1);
		expect(spheres[0].shortId).toBe('s');
	});

	test('listFacets requests the sphere-scoped path', async () => {
		let calledPath = '';
		const stub: FetchLike = async (path) => {
			calledPath = path;
			return jsonResponse({ facets: [] });
		};
		const api = new InfluenceApi(stub);
		await api.listFacets('sph-1');
		expect(calledPath).toBe('/api/spheres/sph-1/facets');
	});

	test('listPolygons requests the sphere-scoped path', async () => {
		let calledPath = '';
		const stub: FetchLike = async (path) => {
			calledPath = path;
			return jsonResponse({ polygons: [] });
		};
		const api = new InfluenceApi(stub);
		await api.listPolygons('sph-1');
		expect(calledPath).toBe('/api/spheres/sph-1/polygons');
	});

	test('non-ok response throws an ApiError carrying the backend code', async () => {
		const stub: FetchLike = async () =>
			jsonResponse({ error: { code: 'TENANT_ISOLATION', message: 'nope' } }, 403);
		const api = new InfluenceApi(stub);
		await expect(api.listSpheres()).rejects.toMatchObject({
			status: 403,
			code: 'TENANT_ISOLATION'
		});
	});

	test('missing envelope arrays default to empty', async () => {
		const stub: FetchLike = async () => jsonResponse({});
		const api = new InfluenceApi(stub);
		expect(await api.listSpheres()).toEqual([]);
		expect(await api.listFacets('x')).toEqual([]);
		expect(await api.listPolygons('x')).toEqual([]);
	});
});
