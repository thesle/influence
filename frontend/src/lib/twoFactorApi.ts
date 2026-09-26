// Two-Factor Authentication API client + login state machine for the Influence
// frontend (task 29.5, Req 33.1, 33.4, 33.7, 33.8).
//
// This module wraps the 2FA-related endpoints and, crucially, the login flow's
// three-way outcome so the shell can route the user to the right view. Following
// the setupApi/adminApi pattern, all network calls and the pure decision logic
// live here (a testable *.ts module) so they can be unit- and property-tested
// without a DOM harness; the .svelte components stay thin.
//
// Endpoints (design.md — "API Surface", internal/api/handlers.go):
//
//   POST /api/auth/login      body { username, password, code? }
//                             -> 200 { status: "ok" }                  (active)
//                             -> 202 { status: "second_factor_required" }
//                             -> 202 { status: "enrolment_required" }  (must_enrol)
//                             -> 401 UNAUTHENTICATED                   (bad creds / 2nd factor)
//   POST /api/2fa/enrol       -> 200 { secret, uri }
//   POST /api/2fa/confirm     body { code }
//                             -> 200 { recoveryCodes: string[] }
//                             -> 400 VALIDATION                        (bad code, Req 33.3)
//   PUT  /api/admin/2fa-policy body { required } -> 200 { required }   (admin only)

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

	/** True when the request failed validation (400 VALIDATION) — e.g. a bad TOTP during confirm (Req 33.3). */
	get isValidation(): boolean {
		return this.status === 400 || this.code === 'VALIDATION';
	}

	/** True when credentials or the second factor were rejected (401, Req 33.5). */
	get isUnauthenticated(): boolean {
		return this.status === 401;
	}

	/** True when the failure was an admin-privileges rejection (Req 5.5). */
	get isAdminRequired(): boolean {
		return this.status === 403 || this.code === 'ADMIN_REQUIRED';
	}
}

// ---------------------------------------------------------------------------
// Response / request shapes (mirror internal/api/handlers.go)
// ---------------------------------------------------------------------------

/** `POST /api/2fa/enrol` success response — the pending secret + provisioning URI (Req 33.1). */
export interface EnrolmentChallenge {
	/** Base32 TOTP secret, shown as selectable text as a QR fallback. */
	secret: string;
	/** `otpauth://totp/...` provisioning URI the web app renders as a QR code. */
	uri: string;
}

/** `POST /api/2fa/confirm` success response — the one-time Recovery_Codes (Req 33.2). */
export interface RecoveryCodes {
	recoveryCodes: string[];
}

/** Credentials submitted from the login form; `code` is added on the second-factor re-submit. */
export interface LoginInput {
	username: string;
	password: string;
	/** A TOTP or single-use Recovery_Code, supplied on the second-factor re-submit (Req 33.4, 33.6). */
	code?: string;
}

/** The raw `{ status }` value the login endpoint returns for 2xx outcomes. */
export type LoginStatus = 'ok' | 'second_factor_required' | 'enrolment_required';

/** `POST /api/auth/login` 2xx body. */
export interface LoginResponse {
	status: LoginStatus;
}

// ---------------------------------------------------------------------------
// Pure login state machine (no network — unit/property testable)
// ---------------------------------------------------------------------------

/**
 * The next UI state after a login attempt.
 *  - `app`        — fully authenticated; show the app shell (200 ok).
 *  - `challenge`  — 2FA enrolled but no/bad code yet; prompt for a TOTP or
 *                   recovery code and re-submit (202 second_factor_required).
 *  - `enrolment`  — restricted must_enrol session; land on the enrolment view
 *                   (202 enrolment_required, Req 33.7).
 */
export type LoginUiState = 'app' | 'challenge' | 'enrolment';

/**
 * Map a login `{ status }` response to the next UI state (Req 33.4, 33.7).
 *
 * This is the single source of truth for how the shell routes each login
 * outcome, kept pure so it can be exhaustively property-tested. An unrecognised
 * status is treated as `challenge`: the safe default never grants app access on
 * an outcome the client does not understand, and re-prompting for a code is
 * always recoverable.
 */
export function loginUiStateFor(response: LoginResponse | null | undefined): LoginUiState {
	switch (response?.status) {
		case 'ok':
			return 'app';
		case 'enrolment_required':
			return 'enrolment';
		case 'second_factor_required':
			return 'challenge';
		default:
			return 'challenge';
	}
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
 * Two-Factor Authentication client bound to a Fetch implementation. In the
 * browser this is the global `fetch`; SvelteKit `load` passes its scoped
 * `fetch`; tests pass a fake.
 */
export class TwoFactorApi {
	private readonly fetchImpl: FetchLike;

	constructor(fetchImpl: FetchLike = fetch) {
		this.fetchImpl = fetchImpl;
	}

	/**
	 * Attempt a login (Req 33.4, 33.7). Returns the decoded `{ status }` for any
	 * 2xx outcome (200 ok / 202 second_factor_required / 202 enrolment_required)
	 * so the caller can route via `loginUiStateFor`. A 401 (bad credentials or a
	 * rejected second factor) throws an ApiError with `isUnauthenticated`.
	 */
	async login(input: LoginInput): Promise<LoginResponse> {
		const payload: LoginInput = { username: input.username, password: input.password };
		if (input.code !== undefined && input.code !== '') {
			payload.code = input.code;
		}
		const res = await this.fetchImpl('/api/auth/login', {
			method: 'POST',
			headers: { 'content-type': 'application/json', Accept: 'application/json' },
			credentials: 'same-origin',
			body: JSON.stringify(payload)
		});
		if (!res.ok) {
			throw await readError(res);
		}
		const body = (await res.json()) as Partial<LoginResponse>;
		return { status: (body.status ?? 'ok') as LoginStatus };
	}

	/**
	 * Begin enrolment: mint a pending TOTP_Secret and return it plus the
	 * `otpauth://` provisioning URI (Req 33.1). Reachable by an authenticated
	 * session or a restricted must_enrol session.
	 */
	async enrol(): Promise<EnrolmentChallenge> {
		const res = await this.fetchImpl('/api/2fa/enrol', {
			method: 'POST',
			headers: { Accept: 'application/json' },
			credentials: 'same-origin'
		});
		if (!res.ok) {
			throw await readError(res);
		}
		const body = (await res.json()) as Partial<EnrolmentChallenge>;
		return { secret: body.secret ?? '', uri: body.uri ?? '' };
	}

	/**
	 * Confirm enrolment with a TOTP read from the authenticator app (Req 33.2).
	 * On success returns the one-time Recovery_Codes (the caller's sole copy). An
	 * invalid code throws an ApiError with `isValidation` (Req 33.3).
	 */
	async confirm(code: string): Promise<RecoveryCodes> {
		const res = await this.fetchImpl('/api/2fa/confirm', {
			method: 'POST',
			headers: { 'content-type': 'application/json', Accept: 'application/json' },
			credentials: 'same-origin',
			body: JSON.stringify({ code })
		});
		if (!res.ok) {
			throw await readError(res);
		}
		const body = (await res.json()) as Partial<RecoveryCodes>;
		return { recoveryCodes: Array.isArray(body.recoveryCodes) ? body.recoveryCodes : [] };
	}

	/**
	 * Set the caller's tenant Two_Factor_Policy to required/optional (Req 33.8).
	 * Admin only — the backend takes the tenant from the session, never from
	 * client input. Echoes the applied `{ required }` flag. A non-admin caller
	 * throws an ApiError with `isAdminRequired`.
	 */
	async setPolicy(required: boolean): Promise<{ required: boolean }> {
		const res = await this.fetchImpl('/api/admin/2fa-policy', {
			method: 'PUT',
			headers: { 'content-type': 'application/json', Accept: 'application/json' },
			credentials: 'same-origin',
			body: JSON.stringify({ required })
		});
		if (!res.ok) {
			throw await readError(res);
		}
		const body = (await res.json()) as Partial<{ required: boolean }>;
		return { required: body.required === true };
	}
}

/** Shared client bound to the global `fetch`, for browser-side use. */
export const twoFactorApi = new TwoFactorApi();
