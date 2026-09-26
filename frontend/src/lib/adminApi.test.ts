import { describe, expect, test } from 'vitest';
import fc from 'fast-check';
import {
	AdminApi,
	ApiError,
	grantFor,
	normalizeBookmarks,
	type BookmarksResponse,
	type FetchLike,
	type PolygonSummary,
	type Sphere,
	type SphereAccessGrant
} from './adminApi';

// A tiny fake Response so we can drive AdminApi without a network. Only the
// fields the client reads (ok, status, json, text) are implemented.
function fakeResponse(opts: {
	status: number;
	body?: unknown;
	text?: string;
}): Response {
	const { status, body, text } = opts;
	return {
		ok: status >= 200 && status < 300,
		status,
		json: async () => body,
		text: async () => text ?? ''
	} as unknown as Response;
}

function sphere(name: string, recordId: string): Sphere {
	return { recordId, shortId: recordId.slice(0, 6), name };
}

function polygon(recordId: string, sphereRecordId: string): PolygonSummary {
	return { recordId, sphereRecordId, type: 'markdown', content: `# ${recordId}` };
}

describe('normalizeBookmarks (Req 26.3, 26.4)', () => {
	test('reports empty when there are no groups', () => {
		const result = normalizeBookmarks({ groups: [], empty: true });
		expect(result.empty).toBe(true);
		expect(result.groups).toEqual([]);
	});

	test('drops groups that hold no polygons and reports empty', () => {
		const resp: BookmarksResponse = {
			groups: [{ sphere: sphere('S1', 'aaa'), polygons: [] }],
			empty: false
		};
		const result = normalizeBookmarks(resp);
		expect(result.groups).toEqual([]);
		expect(result.empty).toBe(true);
	});

	test('keeps non-empty groups and reports non-empty', () => {
		const resp: BookmarksResponse = {
			groups: [
				{ sphere: sphere('S1', 'aaa'), polygons: [polygon('p1', 'aaa')] },
				{ sphere: sphere('S2', 'bbb'), polygons: [] }
			],
			empty: false
		};
		const result = normalizeBookmarks(resp);
		expect(result.empty).toBe(false);
		expect(result.groups).toHaveLength(1);
		expect(result.groups[0].sphere.recordId).toBe('aaa');
	});

	// Property 29 (design.md): the menu partitions bookmarks by owning Sphere,
	// with every bookmark under exactly its own Sphere, and is empty exactly when
	// no Polygon is bookmarked. **Validates: Requirements 26.3, 26.4**
	test('partitions every bookmark under exactly its owning Sphere', () => {
		const sphereIdArb = fc.constantFrom('aaa', 'bbb', 'ccc');
		const groupArb = fc.record({
			id: sphereIdArb,
			count: fc.nat({ max: 4 })
		});
		fc.assert(
			fc.property(fc.array(groupArb, { maxLength: 5 }), (rawGroups) => {
				// Build a response with one group per distinct sphere id, mirroring
				// the server's grouping. Polygons carry their owning sphere id.
				const byId = new Map<string, PolygonSummary[]>();
				for (const g of rawGroups) {
					const list = byId.get(g.id) ?? [];
					for (let i = 0; i < g.count; i++) {
						list.push(polygon(`${g.id}-${list.length}`, g.id));
					}
					byId.set(g.id, list);
				}
				const groups = [...byId.entries()].map(([id, polygons]) => ({
					sphere: sphere(id.toUpperCase(), id),
					polygons
				}));
				const totalPolys = groups.reduce((n, g) => n + g.polygons.length, 0);

				const result = normalizeBookmarks({ groups, empty: totalPolys === 0 });

				// Empty exactly when no polygon is bookmarked.
				expect(result.empty).toBe(totalPolys === 0);
				// No surviving group is empty.
				for (const g of result.groups) {
					expect(g.polygons.length).toBeGreaterThan(0);
					// Every polygon sits under its own sphere.
					for (const p of g.polygons) {
						expect(p.sphereRecordId).toBe(g.sphere.recordId);
					}
				}
				// Partition preserves the total count (no bookmark lost or duplicated).
				const kept = result.groups.reduce((n, g) => n + g.polygons.length, 0);
				expect(kept).toBe(totalPolys);
			}),
			{ numRuns: 200 }
		);
	});
});

describe('grantFor (Req 5.3)', () => {
	const grants: SphereAccessGrant[] = [
		{ sphereRecordId: 'aaa', groupId: 1, access: 'read', reveal: false },
		{ sphereRecordId: 'aaa', groupId: 2, access: 'write', reveal: true },
		{ sphereRecordId: 'bbb', groupId: 1, access: 'none', reveal: true }
	];

	test('returns the stored grant for a granted cell', () => {
		expect(grantFor(grants, 'aaa', 2)).toEqual({ access: 'write', reveal: true });
	});

	test('defaults to none/no-reveal for an absent cell', () => {
		expect(grantFor(grants, 'ccc', 9)).toEqual({ access: 'none', reveal: false });
	});

	test('forces reveal off when access is none even if stored true', () => {
		expect(grantFor(grants, 'bbb', 1)).toEqual({ access: 'none', reveal: false });
	});
});

describe('AdminApi error handling', () => {
	test('surfaces ADMIN_REQUIRED as an admin-required ApiError (Req 5.5)', async () => {
		const fetchFn: FetchLike = async () =>
			fakeResponse({
				status: 403,
				body: { error: { code: 'ADMIN_REQUIRED', message: 'admin privileges required' } }
			});
		const api = new AdminApi(fetchFn);
		await expect(api.listUsers()).rejects.toSatisfy(
			(err: unknown) => err instanceof ApiError && err.isAdminRequired
		);
	});

	test('me() returns the decoded identity', async () => {
		const fetchFn: FetchLike = async () =>
			fakeResponse({
				status: 200,
				body: { userId: 7, username: 'admin', displayName: 'Admin', isAdmin: true }
			});
		const api = new AdminApi(fetchFn);
		await expect(api.me()).resolves.toEqual({
			userId: 7,
			username: 'admin',
			displayName: 'Admin',
			isAdmin: true
		});
	});

	test('getDoc returns markdown text on success', async () => {
		const fetchFn: FetchLike = async (url) => {
			expect(url).toBe('/api/docs/getting-started');
			return fakeResponse({ status: 200, text: '# Hello' });
		};
		const api = new AdminApi(fetchFn);
		await expect(api.getDoc('getting-started')).resolves.toEqual({
			path: 'getting-started',
			markdown: '# Hello'
		});
	});

	test('getDoc throws DOC_UNAVAILABLE on a non-2xx response (Req 27.3)', async () => {
		const fetchFn: FetchLike = async () => fakeResponse({ status: 404 });
		const api = new AdminApi(fetchFn);
		await expect(api.getDoc('missing')).rejects.toSatisfy(
			(err: unknown) => err instanceof ApiError && err.code === 'DOC_UNAVAILABLE'
		);
	});

	test('setSphereAccess sends the grant as the request body', async () => {
		let capturedBody = '';
		const fetchFn: FetchLike = async (_url, init) => {
			capturedBody = (init?.body as string) ?? '';
			return fakeResponse({ status: 200, body: { status: 'ok' } });
		};
		const api = new AdminApi(fetchFn);
		const grant: SphereAccessGrant = {
			sphereRecordId: 'aaa',
			groupId: 3,
			access: 'write',
			reveal: true
		};
		await api.setSphereAccess(grant);
		expect(JSON.parse(capturedBody)).toEqual(grant);
	});
});
