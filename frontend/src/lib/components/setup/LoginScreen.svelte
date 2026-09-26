<script lang="ts">
	// Login screen (task 29.5, Req 33.4, 33.7). Shown once a Tenant is out of
	// First_Run_State (Req 31.3). It drives the three-way login outcome:
	//
	//  - 200 ok                     -> authenticated; signal the shell to enter
	//                                  the app (onauthenticated).
	//  - 202 second_factor_required -> the account has 2FA enrolled; reveal a
	//                                  code field and re-POST username+password+code.
	//                                  A TOTP or single-use recovery code is
	//                                  accepted (Req 33.4, 33.6).
	//  - 202 enrolment_required     -> a restricted must_enrol session; hand off
	//                                  to the enrolment view (onenrolment, Req 33.7).
	//  - 401                        -> bad credentials or a rejected second factor
	//                                  (Req 33.5); shown inline.
	//
	// Network + the status→UI-state mapping live in $lib/twoFactorApi
	// (loginUiStateFor), keeping this component thin and the routing testable.

	import {
		TwoFactorApi,
		twoFactorApi,
		ApiError,
		loginUiStateFor
	} from '$lib/twoFactorApi';

	let {
		client = twoFactorApi,
		onauthenticated,
		onenrolment
	}: {
		client?: TwoFactorApi;
		/** Called after a fully authenticated login (200 ok). */
		onauthenticated?: () => void;
		/** Called when the session is restricted to enrolment (202 enrolment_required). */
		onenrolment?: () => void;
	} = $props();

	let username = $state('');
	let password = $state('');
	let code = $state('');
	// Whether the second-factor code field is shown (after a 202 second_factor_required).
	let needsCode = $state(false);
	let submitting = $state(false);
	let errorMessage = $state<string | null>(null);

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (username.trim() === '' || password === '') return;
		if (needsCode && code.trim() === '') return;
		submitting = true;
		errorMessage = null;
		try {
			const response = await client.login({
				username: username.trim(),
				password,
				code: needsCode ? code.trim() : undefined
			});
			switch (loginUiStateFor(response)) {
				case 'app':
					onauthenticated?.();
					return;
				case 'enrolment':
					onenrolment?.();
					return;
				case 'challenge':
					// 2FA enrolled: reveal the code field and ask for the second factor.
					needsCode = true;
					return;
			}
		} catch (err) {
			if (err instanceof ApiError && err.isUnauthenticated) {
				errorMessage = needsCode
					? 'That code was not valid. Enter a fresh authenticator code or a recovery code.'
					: 'Incorrect username or password.';
			} else {
				errorMessage =
					err instanceof ApiError ? err.message : 'Could not sign in. Please try again.';
			}
		} finally {
			submitting = false;
		}
	}
</script>

<section class="login" aria-label="Log in">
	<h1>Log in to Influence</h1>
	<p class="login__note">Sign in with your Influence account.</p>

	<form class="login__form" onsubmit={submit}>
		<label>
			Username
			<input
				name="username"
				bind:value={username}
				autocomplete="username"
				maxlength="100"
				required
				disabled={needsCode}
				placeholder="you"
			/>
		</label>
		<label>
			Password
			<input
				name="password"
				type="password"
				bind:value={password}
				autocomplete="current-password"
				required
				disabled={needsCode}
			/>
		</label>

		{#if needsCode}
			<label>
				Authenticator or recovery code
				<input
					name="code"
					bind:value={code}
					inputmode="numeric"
					autocomplete="one-time-code"
					placeholder="123456"
					required
				/>
			</label>
			<p class="login__note">
				Enter the 6-digit code from your authenticator app, or one of your recovery codes.
			</p>
		{/if}

		{#if errorMessage}
			<p class="login__error" role="alert">{errorMessage}</p>
		{/if}

		<button
			type="submit"
			disabled={submitting ||
				username.trim() === '' ||
				password === '' ||
				(needsCode && code.trim() === '')}
		>
			{#if submitting}
				Signing in…
			{:else if needsCode}
				Verify code
			{:else}
				Log in
			{/if}
		</button>
	</form>
</section>

<style>
	.login {
		max-width: 26rem;
		margin: 3rem auto;
		padding: 2rem;
		border: 1px solid #e4e7eb;
		border-radius: 8px;
		background: #fff;
	}

	.login h1 {
		margin-top: 0;
	}

	.login__note {
		color: #52606d;
		font-size: 0.9rem;
	}

	.login__form {
		display: flex;
		flex-direction: column;
		gap: 0.9rem;
	}

	.login__form label {
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
		font-size: 0.85rem;
	}

	.login__form input {
		padding: 0.5rem;
		border: 1px solid #cbd2d9;
		border-radius: 4px;
		font-size: 1rem;
	}

	.login__form input:disabled {
		background: #f5f7fa;
		color: #52606d;
	}

	.login__error {
		margin: 0;
		color: #a4262c;
		font-size: 0.85rem;
	}

	.login__form button {
		padding: 0.6rem;
		border: none;
		border-radius: 4px;
		background: #2f6fed;
		color: #fff;
		font-size: 0.95rem;
		cursor: pointer;
	}

	.login__form button:disabled {
		background: #9aa5b1;
		cursor: not-allowed;
	}
</style>
