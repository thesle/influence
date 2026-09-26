import { describe, expect, test } from 'vitest';
import fc from 'fast-check';
import {
	TwoFactorApi,
	ApiError,
	loginUiStateFor,
	type FetchLike,
	type LoginResponse,
	type LoginStatus,
	type LoginUiState
} from './twoFactorApi';

// A tiny fake Response so we can drive TwoFactorApi without a network. Only the
// fields the client reads (ok, status, json) are implemented.
function fakeResponse(opts: { status: number; body?: unknown }): Response {
	const { status, body } = opts;
	return {
		ok: status >= 200 && status < 300,
		status,
		statusText: '',
		json: async () => body
	} as unknown as Response;
}

describe('loginUiStateFor (Req 33.4, 33.7)', () => {
	test('an active session lands on the app', () => {
		expect(loginUiStateFor({ status: 'ok' })).toBe('app');
	});

	test('second_factor_required routes to the code challenge', () => {
		expect(loginUiStateFor({ status: 'second_factor_required' })).toBe('challenge');
	});

	test('enrolment_required routes straight to the enrolment view', () => {
		expect(loginUiStateFor({ status: 'enrolment_required' })).toBe('enrolment');
	});

	test('an unknown or missing status is treated as a challenge (never grants app access)', () => {
		expect(loginUiStateFor({ status: 'weird' as LoginStatus })).toBe('challenge');
		expect(loginUiStateFor(null)).toBe('challenge');
		expect(loginUiStateFor(undefined)).toBe('challenge');
	});

	// Property: the mapping is total and deterministic — the app state is reached
	// exactly and only for the 'ok' status; every other (including unknown)
	// outcome is a non-app state. This guards the invariant that an
	// unrecognised login outcome can never silently authenticate the user.
	test('property: app iff status is exactly "ok"', () => {
		const knownStatuses: LoginStatus[] = ['ok', 'second_factor_required', 'enrolment_required'];
		fc.assert(
			fc.property(
				fc.oneof(
					fc.record({ status: fc.constantFrom(...knownStatuses) }),
					fc.record({ status: fc.string() as fc.Arbitrary<LoginStatus> }),
					fc.constant(null),
					fc.constant(undefined)
				),
				(response) => {
					const state: LoginUiState = loginUiStateFor(
						response as LoginResponse | null | undefined
					);
					const isOk = response != null && response.status === 'ok';
					// app is reached exactly when status is 'ok'.
					expect(state === 'app').toBe(isOk);
					// enrolment is reached exactly for the enrolment_required status.
					const isEnrol = response != null && response.status === 'enrolment_required';
					expect(state === 'enrolment').toBe(isEnrol);
					// every mapping yields one of the three known UI states.
					expect(['app', 'challenge', 'enrolment']).toContain(state);
				}
			),
			{ numRuns: 300 }
		);
	});
});

describe('TwoFactorApi.login (Req 33.4, 33.7)', () => {
	test('posts credentials and decodes the 200 active status', async () => {
		let capturedUrl = '';
		let capturedBody = '';
		const fetchImpl: FetchLike = async (url, init) => {
			capturedUrl = url;
			capturedBody = (init?.body as string) ?? '';
			return fakeResponse({ status: 200, body: { status: 'ok' } });
		};
		const api = new TwoFactorApi(fetchImpl);
		const res = await api.login({ username: 'alice', password: 'hunter2xY!' });
		expect(capturedUrl).toBe('/api/auth/login');
		expect(JSON.parse(capturedBody)).toEqual({ username: 'alice', password: 'hunter2xY!' });
		expect(res).toEqual({ status: 'ok' });
	});

	test('decodes 202 second_factor_required without a code', async () => {
		const fetchImpl: FetchLike = async () =>
			fakeResponse({ status: 202, body: { status: 'second_factor_required' } });
		const api = new TwoFactorApi(fetchImpl);
		await expect(api.login({ username: 'alice', password: 'pw' })).resolves.toEqual({
			status: 'second_factor_required'
		});
	});

	test('decodes 202 enrolment_required for a must_enrol session', async () => {
		const fetchImpl: FetchLike = async () =>
			fakeResponse({ status: 202, body: { status: 'enrolment_required' } });
		const api = new TwoFactorApi(fetchImpl);
		await expect(api.login({ username: 'alice', password: 'pw' })).resolves.toEqual({
			status: 'enrolment_required'
		});
	});

	test('includes the code field only on the second-factor re-submit', async () => {
		let capturedBody = '';
		const fetchImpl: FetchLike = async (_url, init) => {
			capturedBody = (init?.body as string) ?? '';
			return fakeResponse({ status: 200, body: { status: 'ok' } });
		};
		const api = new TwoFactorApi(fetchImpl);
		await api.login({ username: 'alice', password: 'pw', code: '123456' });
		expect(JSON.parse(capturedBody)).toEqual({
			username: 'alice',
			password: 'pw',
			code: '123456'
		});
	});

	test('omits an empty code so a first attempt is not a second-factor submit', async () => {
		let capturedBody = '';
		const fetchImpl: FetchLike = async (_url, init) => {
			capturedBody = (init?.body as string) ?? '';
			return fakeResponse({ status: 200, body: { status: 'ok' } });
		};
		const api = new TwoFactorApi(fetchImpl);
		await api.login({ username: 'alice', password: 'pw', code: '' });
		expect(JSON.parse(capturedBody)).toEqual({ username: 'alice', password: 'pw' });
	});

	test('surfaces 401 as an unauthenticated ApiError (Req 33.5)', async () => {
		const fetchImpl: FetchLike = async () =>
			fakeResponse({
				status: 401,
				body: { error: { code: 'UNAUTHENTICATED', message: 'authentication failed' } }
			});
		const api = new TwoFactorApi(fetchImpl);
		await expect(api.login({ username: 'alice', password: 'bad' })).rejects.toSatisfy(
			(err: unknown) => err instanceof ApiError && err.isUnauthenticated
		);
	});
});

describe('TwoFactorApi.enrol (Req 33.1)', () => {
	test('returns the pending secret and otpauth provisioning URI', async () => {
		let capturedUrl = '';
		let capturedMethod = '';
		const fetchImpl: FetchLike = async (url, init) => {
			capturedUrl = url;
			capturedMethod = init?.method ?? '';
			return fakeResponse({
				status: 200,
				body: { secret: 'JBSWY3DPEHPK3PXP', uri: 'otpauth://totp/Influence:alice?secret=JBSWY3DPEHPK3PXP' }
			});
		};
		const api = new TwoFactorApi(fetchImpl);
		const challenge = await api.enrol();
		expect(capturedUrl).toBe('/api/2fa/enrol');
		expect(capturedMethod).toBe('POST');
		expect(challenge.secret).toBe('JBSWY3DPEHPK3PXP');
		expect(challenge.uri).toMatch(/^otpauth:\/\/totp\//);
	});

	test('throws an ApiError on a non-2xx response', async () => {
		const fetchImpl: FetchLike = async () =>
			fakeResponse({ status: 401, body: { error: { code: 'UNAUTHENTICATED', message: 'no session' } } });
		const api = new TwoFactorApi(fetchImpl);
		await expect(api.enrol()).rejects.toMatchObject({ status: 401, code: 'UNAUTHENTICATED' });
	});
});

describe('TwoFactorApi.confirm (Req 33.2, 33.3)', () => {
	test('posts the code and returns the one-time recovery codes', async () => {
		let capturedBody = '';
		const fetchImpl: FetchLike = async (url, init) => {
			expect(url).toBe('/api/2fa/confirm');
			capturedBody = (init?.body as string) ?? '';
			return fakeResponse({ status: 200, body: { recoveryCodes: ['aaaa-bbbb', 'cccc-dddd'] } });
		};
		const api = new TwoFactorApi(fetchImpl);
		const result = await api.confirm('123456');
		expect(JSON.parse(capturedBody)).toEqual({ code: '123456' });
		expect(result.recoveryCodes).toEqual(['aaaa-bbbb', 'cccc-dddd']);
	});

	test('coerces a missing recoveryCodes to an empty array', async () => {
		const fetchImpl: FetchLike = async () => fakeResponse({ status: 200, body: {} });
		const api = new TwoFactorApi(fetchImpl);
		await expect(api.confirm('123456')).resolves.toEqual({ recoveryCodes: [] });
	});

	test('surfaces VALIDATION for an invalid TOTP (Req 33.3)', async () => {
		const fetchImpl: FetchLike = async () =>
			fakeResponse({
				status: 400,
				body: { error: { code: 'VALIDATION', message: 'invalid code' } }
			});
		const api = new TwoFactorApi(fetchImpl);
		await expect(api.confirm('000000')).rejects.toSatisfy(
			(err: unknown) => err instanceof ApiError && err.isValidation
		);
	});
});

describe('TwoFactorApi.setPolicy (Req 33.8)', () => {
	test('PUTs the required flag and echoes it back', async () => {
		let capturedUrl = '';
		let capturedMethod = '';
		let capturedBody = '';
		const fetchImpl: FetchLike = async (url, init) => {
			capturedUrl = url;
			capturedMethod = init?.method ?? '';
			capturedBody = (init?.body as string) ?? '';
			return fakeResponse({ status: 200, body: { required: true } });
		};
		const api = new TwoFactorApi(fetchImpl);
		const result = await api.setPolicy(true);
		expect(capturedUrl).toBe('/api/admin/2fa-policy');
		expect(capturedMethod).toBe('PUT');
		expect(JSON.parse(capturedBody)).toEqual({ required: true });
		expect(result).toEqual({ required: true });
	});

	test('round-trips the optional (false) setting', async () => {
		const fetchImpl: FetchLike = async () => fakeResponse({ status: 200, body: { required: false } });
		const api = new TwoFactorApi(fetchImpl);
		await expect(api.setPolicy(false)).resolves.toEqual({ required: false });
	});

	test('surfaces ADMIN_REQUIRED for a non-admin caller (Req 5.5)', async () => {
		const fetchImpl: FetchLike = async () =>
			fakeResponse({
				status: 403,
				body: { error: { code: 'ADMIN_REQUIRED', message: 'admin privileges required' } }
			});
		const api = new TwoFactorApi(fetchImpl);
		await expect(api.setPolicy(true)).rejects.toSatisfy(
			(err: unknown) => err instanceof ApiError && err.isAdminRequired
		);
	});
});
