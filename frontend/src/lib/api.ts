// Typed API client for the Influence Go backend (design.md — "API Surface").
//
// The frontend and the Go API share an origin: in development the Vite dev
// server proxies `/api` to the backend (see vite.config.ts); in production the
// Go binary reverse-proxies to (or serves alongside) the SvelteKit node
// server. So every request here is a same-origin call to a `/api/...` path.
//
// The response shapes mirror the JSON the handlers emit (internal/api/
// handlers.go): `sphereResponse`, `facetResponse`, `polygonResponse`. Fields the
// backend may add later (e.g. a per-user `circled` flag on a Sphere) are typed
// optional so the client keeps working against the current shapes while the
// left-menu ordering logic can consume them when present.

/** A Sphere as returned by `GET /api/spheres`. */
export interface Sphere {
	/** Canonical UUID Record_ID. */
	recordId: string;
	/** Short-UUID form used to build URLs (Req 15.2). */
	shortId: string;
	name: string;
	createdAt: string;
	/**
	 * Whether the Sphere is in the caller's Circled_Sphere list (Req 7). The
	 * current backend orders the list server-side and does not emit this flag;
	 * it is optional so the client can consume it if/when the API adds it and
	 * otherwise infer circled membership from ordering metadata.
	 */
	circled?: boolean;
}

/** A Facet as returned by `GET /api/spheres/{id}/facets`. */
export interface Facet {
	recordId: string;
	sphereRecordId: string;
	/** Parent Facet Record_ID, or absent/null for a top-level Facet. */
	parentRecordId?: string | null;
	name: string;
}

/** The Polygon content types (repo.PolygonType). */
export type PolygonType = 'markdown' | 'folder' | 'tabular' | 'whiteboard' | (string & {});

/** A Polygon as returned by `GET /api/spheres/{id}/polygons`. */
export interface Polygon {
	recordId: string;
	sphereRecordId: string;
	/** Owning Facet Record_ID, or absent/null when it sits at the Sphere root. */
	facetRecordId?: string | null;
	/** Parent Folder_Polygon Record_ID, if nested under a folder. */
	parentRecordId?: string | null;
	type: PolygonType;
	content: string;
	editMode: string;
	authorUserId: number;
	createdAt: string;
	updatedAt: string;
}

/** The standard error envelope: `{ error: { code, message } }`. */
export interface ApiErrorBody {
	error: { code: string; message: string };
}

/** An error carrying the backend's error code and HTTP status. */
export class ApiError extends Error {
	readonly status: number;
	readonly code: string;
	constructor(status: number, code: string, message: string) {
		super(message);
		this.name = 'ApiError';
		this.status = status;
		this.code = code;
	}
}

/**
 * A minimal fetch surface so the client can be constructed with SvelteKit's
 * load `fetch`, the global `fetch`, or a stub in tests.
 */
export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

/**
 * The Short-UUID Record_ID used to build a Sphere URL. URLs are built from the
 * Short form (Req 15.2, 21.1); the canonical id is the fallback if a Short form
 * is somehow absent so a link is never emitted empty.
 */
export function sphereShortId(sphere: Pick<Sphere, 'shortId' | 'recordId'>): string {
	return sphere.shortId || sphere.recordId;
}

/** The route path for a Sphere, built from its Short-UUID Record_ID (Req 15.2). */
export function spherePath(sphere: Pick<Sphere, 'shortId' | 'recordId'>): string {
	return `/s/${encodeURIComponent(sphereShortId(sphere))}`;
}

/** The route path for a Polygon, built from its Record_ID (Req 15.2). */
export function polygonPath(polygonRecordId: string): string {
	return `/p/${encodeURIComponent(polygonRecordId)}`;
}

async function readError(res: Response): Promise<ApiError> {
	let code = 'UNKNOWN';
	let message = res.statusText || `request failed with status ${res.status}`;
	try {
		const body = (await res.json()) as Partial<ApiErrorBody>;
		if (body?.error) {
			code = body.error.code ?? code;
			message = body.error.message ?? message;
		}
	} catch {
		// Non-JSON error body; keep the status-derived defaults.
	}
	return new ApiError(res.status, code, message);
}

async function getJSON<T>(fetchImpl: FetchLike, path: string): Promise<T> {
	const res = await fetchImpl(path, {
		method: 'GET',
		headers: { Accept: 'application/json' },
		credentials: 'same-origin'
	});
	if (!res.ok) {
		throw await readError(res);
	}
	return (await res.json()) as T;
}

/**
 * The Influence API client. Construct it with a `fetch` implementation so it
 * works inside SvelteKit `load` (which supplies a scoped `fetch`), in the
 * browser (global `fetch`), and under test (a stub).
 */
export class InfluenceApi {
	private readonly fetchImpl: FetchLike;

	constructor(fetchImpl: FetchLike = fetch) {
		this.fetchImpl = fetchImpl;
	}

	/**
	 * Lists the caller's accessible Spheres. The backend already returns the
	 * circled group first in the user's order followed by the alphabetical
	 * remainder (Req 7, 21.1); the left menu re-applies the ordering rule so it
	 * is correct regardless of source order.
	 */
	async listSpheres(): Promise<Sphere[]> {
		const body = await getJSON<{ spheres: Sphere[] }>(this.fetchImpl, '/api/spheres');
		return body.spheres ?? [];
	}

	/** Lists the Facets in a Sphere by its Record_ID (short or canonical). */
	async listFacets(sphereId: string): Promise<Facet[]> {
		const body = await getJSON<{ facets: Facet[] }>(
			this.fetchImpl,
			`/api/spheres/${encodeURIComponent(sphereId)}/facets`
		);
		return body.facets ?? [];
	}

	/** Lists the Polygons in a Sphere by its Record_ID (short or canonical). */
	async listPolygons(sphereId: string): Promise<Polygon[]> {
		const body = await getJSON<{ polygons: Polygon[] }>(
			this.fetchImpl,
			`/api/spheres/${encodeURIComponent(sphereId)}/polygons`
		);
		return body.polygons ?? [];
	}
}

/** A shared client bound to the global `fetch`, for browser-side use. */
export const api = new InfluenceApi();
