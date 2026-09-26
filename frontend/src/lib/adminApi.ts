// Admin / docs / bookmarks API client for the Influence frontend (task 23.3).
//
// This module is intentionally separate from the shell's shared client
// (src/lib/api.ts, owned by task 23.1) so the two tasks can land in parallel
// without clashing. It wraps the documented API surface (design.md — "API
// Surface"):
//
//   GET  /api/me                              -> session/identity incl. isAdmin
//   GET  /api/admin/users                     -> tenant users
//   POST /api/admin/users                     -> create user
//   PATCH /api/admin/users/{id}               -> edit / (de)activate user
//   GET  /api/admin/groups                    -> tenant groups
//   POST /api/admin/groups                    -> create group
//   PATCH /api/admin/groups/{id}              -> edit group
//   GET  /api/admin/sphere-access             -> per-Sphere access matrix
//   PUT  /api/admin/sphere-access             -> upsert one grant
//   GET  /api/bookmarks                       -> bookmarks grouped by Sphere
//   DELETE /api/bookmarks/{polygonId}         -> remove a bookmark
//   GET  /api/docs/{path}                     -> in-app documentation markdown
//
// The backend enforces authorization (admin routes return 403 ADMIN_REQUIRED
// for non-admins, Req 5.5); this client surfaces that as ApiError so the UI can
// present it. Pure data-shaping helpers (grouping, matrix building) are exported
// separately from the network calls so they can be unit- and property-tested.

// ---------------------------------------------------------------------------
// Error envelope
// ---------------------------------------------------------------------------

/** Standard error envelope returned by the API (design.md — "API Surface"). */
export interface ApiErrorBody {
	error: { code: string; message: string };
}

/** Thrown for any non-2xx response. Carries the decoded error code when present. */
export class ApiError extends Error {
	readonly status: number;
	readonly code: string;

	constructor(status: number, code: string, message: string) {
		super(message);
		this.name = 'ApiError';
		this.status = status;
		this.code = code;
	}

	/** True when the failure was an admin-privileges rejection (Req 5.5). */
	get isAdminRequired(): boolean {
		return this.status === 403 || this.code === 'ADMIN_REQUIRED';
	}
}

// ---------------------------------------------------------------------------
// Types mirroring the backend response shapes
// ---------------------------------------------------------------------------

/** Identity + capabilities of the current session (GET /api/me). */
export interface Me {
	userId: number;
	username: string;
	displayName: string;
	isAdmin: boolean;
}

export interface AdminUser {
	userId: number;
	username: string;
	displayName: string;
	active: boolean;
}

export interface AdminGroup {
	groupId: number;
	name: string;
	isAdmin: boolean;
}

/** Sphere summary (mirrors the backend sphereResponse). */
export interface Sphere {
	recordId: string;
	shortId: string;
	name: string;
	createdAt?: string;
}

/** Polygon summary (subset of the backend polygonResponse used by bookmarks). */
export interface PolygonSummary {
	recordId: string;
	sphereRecordId: string;
	type: string;
	content: string;
}

/** Access level a Group holds on a Sphere. `none` means no grant. */
export type AccessLevel = 'none' | 'read' | 'write';

/** A single Group→Sphere grant in the access matrix (Req 5.3). */
export interface SphereAccessGrant {
	sphereRecordId: string;
	groupId: number;
	access: AccessLevel;
	reveal: boolean;
}

/** Raw sphere-access payload (GET /api/admin/sphere-access). */
export interface SphereAccessData {
	spheres: Sphere[];
	groups: AdminGroup[];
	grants: SphereAccessGrant[];
}

/** One Sphere's group of bookmarked Polygons (GET /api/bookmarks). */
export interface BookmarkGroup {
	sphere: Sphere;
	polygons: PolygonSummary[];
}

/** Bookmarks list response, already grouped by Sphere server-side (Req 26.3, 26.4). */
export interface BookmarksResponse {
	groups: BookmarkGroup[];
	empty: boolean;
}

/** In-app documentation document (GET /api/docs/{path}, Req 27.2). */
export interface DocContent {
	path: string;
	markdown: string;
}

// ---------------------------------------------------------------------------
// Pure data-shaping helpers (no network — unit/property testable)
// ---------------------------------------------------------------------------

/**
 * Normalise a bookmarks payload into non-empty groups plus an `empty` flag.
 *
 * The backend already groups by Sphere and supplies `empty`, but the UI needs a
 * dependable derivation: a set of bookmarks is empty exactly when no group holds
 * any Polygon. Groups with zero Polygons are dropped so the menu never renders a
 * Sphere header with nothing under it (Req 26.3, 26.4).
 */
export function normalizeBookmarks(resp: BookmarksResponse): {
	groups: BookmarkGroup[];
	empty: boolean;
} {
	const groups = (resp.groups ?? []).filter((g) => g.polygons && g.polygons.length > 0);
	return { groups, empty: groups.length === 0 };
}

/**
 * Look up the grant for a (sphere, group) pair, defaulting to no-access.
 *
 * The matrix stores only granted cells; an absent grant is `none`/no-reveal
 * (most-restrictive default). Reveal is only meaningful alongside access, so a
 * `none` cell always reports `reveal: false` regardless of any stored flag.
 */
export function grantFor(
	grants: SphereAccessGrant[],
	sphereRecordId: string,
	groupId: number
): { access: AccessLevel; reveal: boolean } {
	const match = grants.find(
		(g) => g.sphereRecordId === sphereRecordId && g.groupId === groupId
	);
	if (!match || match.access === 'none') {
		return { access: 'none', reveal: false };
	}
	return { access: match.access, reveal: match.reveal };
}

// ---------------------------------------------------------------------------
// Fetch layer
// ---------------------------------------------------------------------------

/** The subset of the Fetch API this client needs (lets tests inject a fake). */
export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

async function request<T>(
	fetchFn: FetchLike,
	method: string,
	url: string,
	body?: unknown
): Promise<T> {
	const init: RequestInit = {
		method,
		headers: body === undefined ? undefined : { 'content-type': 'application/json' },
		body: body === undefined ? undefined : JSON.stringify(body)
	};
	const res = await fetchFn(url, init);

	if (!res.ok) {
		let code = 'UNKNOWN';
		let message = `request failed (${res.status})`;
		try {
			const parsed = (await res.json()) as Partial<ApiErrorBody>;
			if (parsed?.error) {
				code = parsed.error.code ?? code;
				message = parsed.error.message ?? message;
			}
		} catch {
			// Non-JSON error body — keep the status-derived defaults.
		}
		throw new ApiError(res.status, code, message);
	}

	if (res.status === 204) {
		return undefined as T;
	}
	return (await res.json()) as T;
}

/**
 * Admin/docs/bookmarks API client bound to a Fetch implementation. In the
 * browser this is the global `fetch`; tests pass a fake.
 */
export class AdminApi {
	private readonly fetchFn: FetchLike;

	constructor(fetchFn: FetchLike = fetch) {
		this.fetchFn = fetchFn;
	}

	// -- Session / identity --------------------------------------------------

	/** Fetch the current session identity, including the admin flag (Req 5). */
	me(): Promise<Me> {
		return request<Me>(this.fetchFn, 'GET', '/api/me');
	}

	// -- Users (Req 5.1) -----------------------------------------------------

	listUsers(): Promise<{ users: AdminUser[] }> {
		return request(this.fetchFn, 'GET', '/api/admin/users');
	}

	createUser(input: {
		username: string;
		displayName: string;
	}): Promise<AdminUser> {
		return request(this.fetchFn, 'POST', '/api/admin/users', input);
	}

	/** Edit a user or toggle activation (Req 5.1, 5.4). */
	updateUser(
		userId: number,
		patch: Partial<{ displayName: string; active: boolean }>
	): Promise<AdminUser> {
		return request(this.fetchFn, 'PATCH', `/api/admin/users/${userId}`, patch);
	}

	// -- Groups (Req 5.2) ----------------------------------------------------

	listGroups(): Promise<{ groups: AdminGroup[] }> {
		return request(this.fetchFn, 'GET', '/api/admin/groups');
	}

	createGroup(input: { name: string }): Promise<AdminGroup> {
		return request(this.fetchFn, 'POST', '/api/admin/groups', input);
	}

	updateGroup(groupId: number, patch: { name: string }): Promise<AdminGroup> {
		return request(this.fetchFn, 'PATCH', `/api/admin/groups/${groupId}`, patch);
	}

	// -- Sphere-access matrix (Req 5.3) --------------------------------------

	getSphereAccess(): Promise<SphereAccessData> {
		return request(this.fetchFn, 'GET', '/api/admin/sphere-access');
	}

	/** Upsert one Group→Sphere grant (access level + Reveal, Req 5.3). */
	setSphereAccess(grant: SphereAccessGrant): Promise<{ status: string }> {
		return request(this.fetchFn, 'PUT', '/api/admin/sphere-access', grant);
	}

	// -- Bookmarks (Req 26) --------------------------------------------------

	listBookmarks(): Promise<BookmarksResponse> {
		return request<BookmarksResponse>(this.fetchFn, 'GET', '/api/bookmarks');
	}

	removeBookmark(polygonRecordId: string): Promise<void> {
		return request<void>(
			this.fetchFn,
			'DELETE',
			`/api/bookmarks/${encodeURIComponent(polygonRecordId)}`
		);
	}

	// -- Docs (Req 27.2, 27.3) -----------------------------------------------

	/**
	 * Fetch an in-app documentation document as raw markdown. The `path` is the
	 * doc path under /api/docs/ (e.g. "getting-started"); it is URL-encoded
	 * segment-by-segment so nested paths survive.
	 */
	async getDoc(path: string): Promise<DocContent> {
		const encoded = path
			.split('/')
			.map((seg) => encodeURIComponent(seg))
			.join('/');
		const res = await this.fetchFn(`/api/docs/${encoded}`, { method: 'GET' });
		if (!res.ok) {
			throw new ApiError(res.status, 'DOC_UNAVAILABLE', `documentation unavailable (${res.status})`);
		}
		const markdown = await res.text();
		return { path, markdown };
	}
}

/** Shared client instance bound to the global fetch (browser use). */
export const adminApi = new AdminApi();
