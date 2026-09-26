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

// Polygon-specific API client (task 23.2).
//
// This module is intentionally separate from the shell's `src/lib/api.ts`
// (owned by task 23.1) so the two tasks can evolve without clashing. It wraps
// the Go backend's polygon routes (see design "API Surface"): get, render,
// encrypt, per-token reveal, image upload, list-children, and the edit-lock
// endpoints. Everything is same-origin under `/api` (the vite dev proxy and the
// single Go binary both serve that prefix), so requests use credentials so the
// session cookie rides along.

/** One of the four Polygon types (Req 10.1). */
export type PolygonType =
	| 'Markdown_Page'
	| 'Folder_Polygon'
	| 'Tabular_Polygon'
	| 'Whiteboard_Polygon';

/** A Polygon's stored state, matching the backend `polygonResponse`. */
export interface Polygon {
	recordId: string;
	sphereRecordId: string;
	facetRecordId?: string;
	parentRecordId?: string;
	type: PolygonType;
	content: string;
	editMode: string;
	authorUserId: number;
	createdAt: string;
	updatedAt: string;
}

/** Author/date footer appended to a rendered Polygon (Req 18). */
export interface MetadataFooter {
	authorDisplayName: string;
	createdAt: string;
	updatedAt: string;
}

/** The render route returns display HTML plus an optional metadata footer. */
export interface RenderResult {
	html: string;
	footer?: MetadataFooter;
}

/** Result of encrypting a span: the minted token id and the rewritten content. */
export interface EncryptResult {
	tokenId: string;
	newContent: string;
}

/** The standard backend error envelope: `{ error: { code, message } }`. */
export interface ApiErrorBody {
	error?: { code?: string; message?: string };
}

/** An error carrying the backend envelope's code + message when present. */
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

async function toApiError(res: Response): Promise<ApiError> {
	let code = 'UNKNOWN';
	let message = `request failed (${res.status})`;
	try {
		const body = (await res.json()) as ApiErrorBody;
		if (body?.error) {
			code = body.error.code ?? code;
			message = body.error.message ?? message;
		}
	} catch {
		// Non-JSON body (e.g. a served image error) — keep the defaults.
	}
	return new ApiError(res.status, code, message);
}

async function getJSON<T>(url: string): Promise<T> {
	const res = await fetch(url, { credentials: 'same-origin' });
	if (!res.ok) throw await toApiError(res);
	return (await res.json()) as T;
}

async function sendJSON<T>(method: string, url: string, body?: unknown): Promise<T> {
	const res = await fetch(url, {
		method,
		credentials: 'same-origin',
		headers: { 'content-type': 'application/json' },
		body: body === undefined ? undefined : JSON.stringify(body)
	});
	if (!res.ok) throw await toApiError(res);
	if (res.status === 204) return undefined as T;
	return (await res.json()) as T;
}

/** Fetch a Polygon's stored state (`GET /api/polygons/{id}`). */
export function getPolygon(id: string): Promise<Polygon> {
	return getJSON<Polygon>(`/api/polygons/${encodeURIComponent(id)}`);
}

/** Render a Polygon to masked HTML for the current user (`GET .../render`). */
export function renderPolygon(id: string): Promise<RenderResult> {
	return getJSON<RenderResult>(`/api/polygons/${encodeURIComponent(id)}/render`);
}

/** List Polygons within a Sphere (`GET /api/spheres/{id}/polygons`). */
export function listSpherePolygons(sphereId: string): Promise<Polygon[]> {
	return getJSON<{ polygons: Polygon[] }>(
		`/api/spheres/${encodeURIComponent(sphereId)}/polygons`
	).then((r) => r.polygons ?? []);
}

/**
 * List the direct child Polygons of a folder/parent Polygon. The backend does
 * not expose a dedicated children route, so this derives children client-side
 * from the Sphere's Polygon list by `parentRecordId` — matching the render-time
 * `@children` semantics (Req 11.7).
 */
export async function listChildren(parent: Polygon): Promise<Polygon[]> {
	const all = await listSpherePolygons(parent.sphereRecordId);
	return all.filter((p) => p.parentRecordId === parent.recordId);
}

/** Encrypt a selected span, replacing it with an obfuscation token (Req 16.1). */
export function encryptSpan(id: string, start: number, end: number): Promise<EncryptResult> {
	return sendJSON<EncryptResult>('POST', `/api/polygons/${encodeURIComponent(id)}/encrypt`, {
		start,
		end
	});
}

/** Reveal a single token's plaintext (`POST .../reveal/{tokenId}`, Req 16.4). */
export function revealToken(id: string, tokenId: string): Promise<string> {
	return sendJSON<{ plaintext: string }>(
		'POST',
		`/api/polygons/${encodeURIComponent(id)}/reveal/${encodeURIComponent(tokenId)}`
	).then((r) => r.plaintext);
}

/**
 * Upload a pasted image blob to a Polygon and return the in-content reference
 * (`influence://image/{image_id}`) to insert (Req 17.1). The raw bytes are the
 * request body; the backend sniffs the type and validates size/format.
 */
export async function uploadImage(id: string, blob: Blob): Promise<string> {
	const res = await fetch(`/api/polygons/${encodeURIComponent(id)}/images`, {
		method: 'POST',
		credentials: 'same-origin',
		headers: { 'content-type': blob.type || 'application/octet-stream' },
		body: blob
	});
	if (!res.ok) throw await toApiError(res);
	const body = (await res.json()) as { reference: string };
	return body.reference;
}

/** Acquire the edit lock for a Polygon before editing (Req 25.3). */
export function acquireLock(id: string): Promise<unknown> {
	return sendJSON('POST', `/api/polygons/${encodeURIComponent(id)}/lock`);
}

/** Release the edit lock for a Polygon (Req 25.5). */
export function releaseLock(id: string): Promise<void> {
	return sendJSON<void>('DELETE', `/api/polygons/${encodeURIComponent(id)}/lock`);
}
