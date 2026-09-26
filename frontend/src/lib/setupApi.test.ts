import { describe, expect, test } from 'vitest';
import fc from 'fast-check';
import {
	SetupApi,
	ApiError,
	entryScreenFor,
	type FetchLike,
	type SetupState
} from './setupApi';

// A tiny fake Response so we can drive SetupApi without a network. Only the
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

describe('entryScreenFor (Req 31.1, 31.3)', () => {
	test('shows the Setup_Screen while First_Run_State is true', () => {
		expect(entryScreenFor({ firstRun: true })).toBe('setup');
	});

	test('shows the login screen once setup is done', () => {
		expect(entryScreenFor({ firstRun: false })).toBe('login');
	});

	test('defaults to login when the state is unknown', () => {
		expect(entryScreenFor(null)).toBe('login');
		expect(entryScreenFor(undefined)).toBe('login');
	});

	// Property: the Setup_Screen is shown in place of login exactly while
	// First_Run_State is true, and the login screen in every other case (Req
	// 31.1, 31.3). No input other than the firstRun flag changes the decision.
	test('property: setup iff firstRun is exactly true', () => {
		fc.assert(
			fc.property(
				fc.oneof(
					fc.record({ firstRun: fc.boolean() }),
					fc.constant(null),
					fc.constant(undefined)
				),
				(state) => {
					const screen = entryScreenFor(state as SetupState | null | undefined);
					const expected = state != null && state.firstRun === true ? 'setup' : 'login';
					expect(screen).toBe(expected);
				}
			),
			{ numRuns: 200 }
		);
	});
});

describe('SetupApi.state', () => {
	test('parses the { firstRun } envelope', async () => {
		const fetchImpl: FetchLike = async (url) => {
			expect(url).toBe('/api/setup/state');
			return fakeResponse({ status: 200, body: { firstRun: true } });
		};
		const api = new SetupApi(fetchImpl);
		await expect(api.state()).resolves.toEqual({ firstRun: true });
	});

	test('coerces a missing firstRun to false', async () => {
		const fetchImpl: FetchLike = async () => fakeResponse({ status: 200, body: {} });
		const api = new SetupApi(fetchImpl);
		await expect(api.state()).resolves.toEqual({ firstRun: false });
	});

	test('passes the tenant as a query parameter when supplied', async () => {
		let calledUrl = '';
		const fetchImpl: FetchLike = async (url) => {
			calledUrl = url;
			return fakeResponse({ status: 200, body: { firstRun: false } });
		};
		const api = new SetupApi(fetchImpl);
		await api.state('tenant a/uuid');
		expect(calledUrl).toBe('/api/setup/state?tenant=tenant%20a%2Fuuid');
	});

	test('throws an ApiError on a non-2xx response', async () => {
		const fetchImpl: FetchLike = async () =>
			fakeResponse({ status: 404, body: { error: { code: 'NOT_FOUND', message: 'no tenant' } } });
		const api = new SetupApi(fetchImpl);
		await expect(api.state()).rejects.toMatchObject({ status: 404, code: 'NOT_FOUND' });
	});
});

describe('SetupApi.createAdmin (Req 31.2, 31.3, 31.4, 31.5)', () => {
	test('posts the credentials and returns the created admin on success', async () => {
		let capturedUrl = '';
		let capturedBody = '';
		const fetchImpl: FetchLike = async (url, init) => {
			capturedUrl = url;
			capturedBody = (init?.body as string) ?? '';
			return fakeResponse({ status: 201, body: { userId: 42, firstRun: false } });
		};
		const api = new SetupApi(fetchImpl);
		const result = await api.createAdmin({ username: 'boss', password: 'Sup3rSecret!pw' });
		expect(capturedUrl).toBe('/api/setup/admin');
		expect(JSON.parse(capturedBody)).toEqual({ username: 'boss', password: 'Sup3rSecret!pw' });
		expect(result).toEqual({ userId: 42, firstRun: false });
	});

	test('includes the tenant field only when provided', async () => {
		let capturedBody = '';
		const fetchImpl: FetchLike = async (_url, init) => {
			capturedBody = (init?.body as string) ?? '';
			return fakeResponse({ status: 201, body: { userId: 1, firstRun: false } });
		};
		const api = new SetupApi(fetchImpl);
		await api.createAdmin({ username: 'boss', password: 'pw', tenant: 't-uuid' });
		expect(JSON.parse(capturedBody)).toEqual({ username: 'boss', password: 'pw', tenant: 't-uuid' });
	});

	test('surfaces VALIDATION for a weak password (Req 31.5)', async () => {
		const fetchImpl: FetchLike = async () =>
			fakeResponse({
				status: 400,
				body: { error: { code: 'VALIDATION', message: 'password too weak' } }
			});
		const api = new SetupApi(fetchImpl);
		await expect(api.createAdmin({ username: 'boss', password: 'weak' })).rejects.toSatisfy(
			(err: unknown) => err instanceof ApiError && err.isValidation
		);
	});

	test('surfaces SETUP_COMPLETE when setup is already done (Req 31.4)', async () => {
		const fetchImpl: FetchLike = async () =>
			fakeResponse({
				status: 409,
				body: { error: { code: 'SETUP_COMPLETE', message: 'setup has already been completed' } }
			});
		const api = new SetupApi(fetchImpl);
		await expect(api.createAdmin({ username: 'boss', password: 'pw' })).rejects.toSatisfy(
			(err: unknown) => err instanceof ApiError && err.isSetupComplete
		);
	});
});
