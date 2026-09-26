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

// First-run setup API client + state helper for the Influence frontend
// (task 28.3, Req 31.1, 31.3).
//
// While a Tenant is in First_Run_State it has no administrator and therefore no
// session to authenticate. The shell asks the backend whether setup is still
// open and, while it is, renders the Setup_Screen (username + password) in place
// of the login screen. A successful submit creates the Bootstrap_Admin and
// clears First_Run_State, after which the shell falls back to the normal login
// screen. These two endpoints (design.md — "First-Run Bootstrap") sit outside
// the authenticated middleware chain:
//
//   GET  /api/setup/state          -> { firstRun: bool }
//   POST /api/setup/admin          -> 201 { userId, firstRun: false }
//                                     | 409 SETUP_COMPLETE (already done)
//                                     | 400 VALIDATION     (weak password / bad username)
//
// The network layer is separated from the pure decision helpers so the "which
// screen to show" logic can be unit- and property-tested without a component
// harness (the frontend has no DOM test runner; see existing *.test.ts).

// ---------------------------------------------------------------------------
// Error envelope
// ---------------------------------------------------------------------------

/** Standard error envelope returned by the API: `{ error: { code, message } }`. */
export interface ApiErrorBody {
	error: { code: string; message: string };
}

/** Thrown for any non-2xx response. Carries the decoded backend code + status. */
export class ApiError extends Error {
	readonly status: number;
	readonly code: string;

	constructor(status: number, code: string, message: string) {
		super(message);
		this.name = 'ApiError';
		this.status = status;
		this.code = code;
	}

	/**
	 * True when setup has already been completed (409 SETUP_COMPLETE, Req 31.4).
	 * The UI treats this as "setup is closed" and falls back to the login screen
	 * rather than showing it as a form error.
	 */
	get isSetupComplete(): boolean {
		return this.status === 409 || this.code === 'SETUP_COMPLETE';
	}

	/** True when the request failed validation (400 VALIDATION, Req 31.5). */
	get isValidation(): boolean {
		return this.status === 400 || this.code === 'VALIDATION';
	}
}

// ---------------------------------------------------------------------------
// Response shapes (mirror internal/api/handlers.go)
// ---------------------------------------------------------------------------

/** `GET /api/setup/state` response — whether the Setup_Screen should show. */
export interface SetupState {
	firstRun: boolean;
}

/** `POST /api/setup/admin` success response (201). */
export interface CreatedAdmin {
	userId: number;
	firstRun: false;
}

/** The credentials (and optional tenant) submitted from the Setup_Screen. */
export interface BootstrapAdminInput {
	username: string;
	password: string;
	/** Optional tenant_uuid to disambiguate a multi-tenant host (Req 31). */
	tenant?: string;
}

// ---------------------------------------------------------------------------
// Pure decision helper (no network — unit/property testable)
// ---------------------------------------------------------------------------

/** Which screen the shell should render at the setup/login boundary. */
export type EntryScreen = 'setup' | 'login';

/**
 * Decide which entry screen to show from the fetched First_Run_State.
 *
 * The Setup_Screen is shown in place of login exactly while the Tenant is in
 * First_Run_State (Req 31.1); once setup is done (or the state could not be
 * read) the normal login screen is shown (Req 31.3). Treating an unknown/failed
 * state as "login" is the safe default: it never blocks a tenant that already
 * has an admin behind a setup form, and a still-first-run tenant simply gets
 * another chance to reach setup on reload.
 */
export function entryScreenFor(state: SetupState | null | undefined): EntryScreen {
	return state?.firstRun === true ? 'setup' : 'login';
}

// ---------------------------------------------------------------------------
// Fetch layer
// ---------------------------------------------------------------------------

/** The subset of the Fetch API this client needs (lets tests inject a fake). */
export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

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
		// Non-JSON error body — keep the status-derived defaults.
	}
	return new ApiError(res.status, code, message);
}

/**
 * First-run setup client bound to a Fetch implementation. In the browser this
 * is the global `fetch`; SvelteKit `load` passes its scoped `fetch`; tests pass
 * a fake.
 */
export class SetupApi {
	private readonly fetchImpl: FetchLike;

	constructor(fetchImpl: FetchLike = fetch) {
		this.fetchImpl = fetchImpl;
	}

	/**
	 * Report whether the tenant is still in First_Run_State (Req 31.1). An
	 * optional tenant_uuid disambiguates a multi-tenant host; it is omitted on
	 * the common single-tenant host and the backend resolves the sole tenant.
	 */
	async state(tenant?: string): Promise<SetupState> {
		const query = tenant ? `?tenant=${encodeURIComponent(tenant)}` : '';
		const res = await this.fetchImpl(`/api/setup/state${query}`, {
			method: 'GET',
			headers: { Accept: 'application/json' },
			credentials: 'same-origin'
		});
		if (!res.ok) {
			throw await readError(res);
		}
		const body = (await res.json()) as Partial<SetupState>;
		return { firstRun: body.firstRun === true };
	}

	/**
	 * Create the Bootstrap_Admin (Req 31.2). On success the tenant leaves
	 * First_Run_State and the shell can flip straight to login (Req 31.3). A weak
	 * password or invalid username throws an ApiError with `isValidation`; a
	 * tenant that already has an admin throws one with `isSetupComplete`.
	 */
	async createAdmin(input: BootstrapAdminInput): Promise<CreatedAdmin> {
		const payload: BootstrapAdminInput = {
			username: input.username,
			password: input.password
		};
		if (input.tenant) {
			payload.tenant = input.tenant;
		}
		const res = await this.fetchImpl('/api/setup/admin', {
			method: 'POST',
			headers: { 'content-type': 'application/json', Accept: 'application/json' },
			credentials: 'same-origin',
			body: JSON.stringify(payload)
		});
		if (!res.ok) {
			throw await readError(res);
		}
		const body = (await res.json()) as Partial<CreatedAdmin>;
		return { userId: Number(body.userId ?? 0), firstRun: false };
	}
}

/** Shared client bound to the global `fetch`, for browser-side use. */
export const setupApi = new SetupApi();
