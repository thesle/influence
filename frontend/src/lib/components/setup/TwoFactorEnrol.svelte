<script lang="ts">
	// Two-Factor Authentication enrolment view (task 29.5, Req 33.1, 33.2, 33.3).
	//
	// Flow:
	//  1. On mount, POST /api/2fa/enrol to mint a pending TOTP_Secret; render the
	//     returned otpauth:// provisioning URI as a scannable QR code (client-side,
	//     via the `qrcode` lib) plus the secret in selectable text form as a
	//     fallback for manual entry (Req 33.1).
	//  2. The user scans/enters the secret in their authenticator app, reads the
	//     6-digit code, and submits it. POST /api/2fa/confirm verifies it; an
	//     invalid code (400 VALIDATION) is shown inline without advancing (Req 33.3).
	//  3. On success the issued single-use Recovery_Codes are displayed once for
	//     the user to save, after which they continue into the app (Req 33.2).
	//
	// Network + decision logic lives in $lib/twoFactorApi; this component is thin.

	import { onMount } from 'svelte';
	import QRCode from 'qrcode';
	import {
		TwoFactorApi,
		twoFactorApi,
		ApiError,
		type EnrolmentChallenge
	} from '$lib/twoFactorApi';

	let {
		client = twoFactorApi,
		oncomplete
	}: {
		client?: TwoFactorApi;
		/** Called once enrolment is confirmed and the user dismisses the recovery codes. */
		oncomplete?: () => void;
	} = $props();

	// Phase of the enrolment flow.
	type Phase = 'loading' | 'scan' | 'done' | 'error';
	let phase = $state<Phase>('loading');

	let challenge = $state<EnrolmentChallenge | null>(null);
	let qrDataUrl = $state<string | null>(null);
	let code = $state('');
	let submitting = $state(false);
	let errorMessage = $state<string | null>(null);
	let recoveryCodes = $state<string[]>([]);

	async function begin() {
		phase = 'loading';
		errorMessage = null;
		try {
			const c = await client.enrol();
			challenge = c;
			// Render the provisioning URI as a QR data URL. If QR generation fails
			// the secret text below still lets the user enrol manually.
			try {
				qrDataUrl = await QRCode.toDataURL(c.uri, { margin: 1, width: 200 });
			} catch {
				qrDataUrl = null;
			}
			phase = 'scan';
		} catch (err) {
			errorMessage =
				err instanceof ApiError ? err.message : 'Could not start two-factor enrolment.';
			phase = 'error';
		}
	}

	async function confirm(event: SubmitEvent) {
		event.preventDefault();
		if (code.trim() === '') return;
		submitting = true;
		errorMessage = null;
		try {
			const result = await client.confirm(code.trim());
			recoveryCodes = result.recoveryCodes;
			phase = 'done';
		} catch (err) {
			// An invalid TOTP (400 VALIDATION) stays on the scan step so the user
			// can re-enter the current code (Req 33.3).
			errorMessage =
				err instanceof ApiError && err.isValidation
					? 'That code was not valid. Check your authenticator app and try again.'
					: err instanceof ApiError
						? err.message
						: 'Could not confirm the code. Please try again.';
		} finally {
			submitting = false;
		}
	}

	onMount(begin);
</script>

<section class="twofa" aria-label="Set up two-factor authentication">
	<h1>Set up two-factor authentication</h1>

	{#if phase === 'loading'}
		<p role="status">Preparing your authenticator setup…</p>
	{:else if phase === 'error'}
		<p class="twofa__error" role="alert">{errorMessage}</p>
		<button type="button" onclick={begin}>Try again</button>
	{:else if phase === 'scan'}
		<p class="twofa__intro">
			Scan this QR code with your authenticator app, or enter the secret manually, then enter the
			6-digit code it shows to confirm.
		</p>

		{#if qrDataUrl}
			<img class="twofa__qr" src={qrDataUrl} alt="Two-factor authentication QR code" />
		{:else}
			<p class="twofa__note">
				A QR code could not be shown — enter the secret below into your app manually.
			</p>
		{/if}

		<label class="twofa__secret">
			Secret
			<input
				class="twofa__secret-value"
				type="text"
				readonly
				value={challenge?.secret ?? ''}
				aria-label="Two-factor secret"
				onfocus={(e) => e.currentTarget.select()}
			/>
		</label>

		<form class="twofa__form" onsubmit={confirm}>
			<label>
				Authenticator code
				<input
					name="code"
					bind:value={code}
					inputmode="numeric"
					autocomplete="one-time-code"
					placeholder="123456"
					required
				/>
			</label>

			{#if errorMessage}
				<p class="twofa__error" role="alert">{errorMessage}</p>
			{/if}

			<button type="submit" disabled={submitting || code.trim() === ''}>
				{submitting ? 'Confirming…' : 'Confirm and enable'}
			</button>
		</form>
	{:else if phase === 'done'}
		<p class="twofa__intro">
			Two-factor authentication is enabled. Save these recovery codes somewhere safe — each can be
			used once if you lose access to your authenticator. They will not be shown again.
		</p>
		<ul class="twofa__codes">
			{#each recoveryCodes as rc (rc)}
				<li>{rc}</li>
			{/each}
		</ul>
		<button type="button" onclick={() => oncomplete?.()}>I have saved my recovery codes</button>
	{/if}
</section>

<style>
	.twofa {
		max-width: 26rem;
		margin: 3rem auto;
		padding: 2rem;
		border: 1px solid #e4e7eb;
		border-radius: 8px;
		background: #fff;
	}

	.twofa h1 {
		margin-top: 0;
	}

	.twofa__intro,
	.twofa__note {
		color: #52606d;
		font-size: 0.9rem;
	}

	.twofa__qr {
		display: block;
		margin: 0.5rem 0 1rem;
		width: 200px;
		height: 200px;
		image-rendering: pixelated;
	}

	.twofa__secret {
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
		font-size: 0.8rem;
		margin-bottom: 1rem;
	}

	.twofa__secret-value {
		padding: 0.5rem;
		border: 1px solid #cbd2d9;
		border-radius: 4px;
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
		font-size: 0.95rem;
		background: #f5f7fa;
	}

	.twofa__form {
		display: flex;
		flex-direction: column;
		gap: 0.9rem;
	}

	.twofa__form label {
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
		font-size: 0.85rem;
	}

	.twofa__form input {
		padding: 0.5rem;
		border: 1px solid #cbd2d9;
		border-radius: 4px;
		font-size: 1rem;
	}

	.twofa__error {
		margin: 0;
		color: #a4262c;
		font-size: 0.85rem;
	}

	.twofa__codes {
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
		background: #f5f7fa;
		border: 1px solid #e4e7eb;
		border-radius: 4px;
		padding: 0.75rem 1.5rem;
		line-height: 1.8;
	}

	.twofa button {
		padding: 0.6rem;
		border: none;
		border-radius: 4px;
		background: #2f6fed;
		color: #fff;
		font-size: 0.95rem;
		cursor: pointer;
	}

	.twofa button:disabled {
		background: #9aa5b1;
		cursor: not-allowed;
	}
</style>
