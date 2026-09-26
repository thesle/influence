<script lang="ts">
	// First-run Setup_Screen (task 28.3, Req 31.1, 31.3). Shown in place of the
	// login screen while the Tenant is in First_Run_State. It collects the
	// Bootstrap_Admin username + password and posts to /api/setup/admin. On
	// success the tenant leaves First_Run_State, so the screen signals its parent
	// to transition to the normal login screen (Req 31.3).
	//
	// Validation errors from the standard envelope are surfaced inline: a weak
	// password or bad username (VALIDATION) is shown as a form error; if setup
	// has already been completed (SETUP_COMPLETE) the screen falls straight back
	// to login rather than trapping the user on a form for a closed flow.

	import { SetupApi, setupApi, ApiError, type BootstrapAdminInput } from '$lib/setupApi';

	let {
		client = setupApi,
		tenant = undefined,
		oncomplete
	}: {
		client?: SetupApi;
		/** Optional tenant_uuid for a multi-tenant host (Req 31). */
		tenant?: string;
		/** Called after the Bootstrap_Admin is created (or setup is already done). */
		oncomplete?: () => void;
	} = $props();

	let username = $state('');
	let password = $state('');
	let submitting = $state(false);
	let errorMessage = $state<string | null>(null);

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (username.trim() === '' || password === '') return;
		submitting = true;
		errorMessage = null;
		const input: BootstrapAdminInput = {
			username: username.trim(),
			password,
			tenant
		};
		try {
			await client.createAdmin(input);
			// First_Run_State cleared — hand off to the login screen (Req 31.3).
			oncomplete?.();
		} catch (err) {
			if (err instanceof ApiError && err.isSetupComplete) {
				// Setup already done by someone else: fall back to login (Req 31.4).
				oncomplete?.();
				return;
			}
			errorMessage =
				err instanceof ApiError ? err.message : 'Could not complete setup. Please try again.';
		} finally {
			submitting = false;
		}
	}
</script>

<section class="setup" aria-label="First-run setup">
	<h1>Set up Influence</h1>
	<p class="setup__intro">
		This tenant has no administrator yet. Create the first administrator account to get started.
	</p>

	<form class="setup__form" onsubmit={submit}>
		<label>
			Username
			<input
				name="username"
				bind:value={username}
				autocomplete="username"
				maxlength="100"
				required
				placeholder="admin"
			/>
		</label>
		<label>
			Password
			<input
				name="password"
				type="password"
				bind:value={password}
				autocomplete="new-password"
				required
			/>
		</label>

		{#if errorMessage}
			<p class="setup__error" role="alert">{errorMessage}</p>
		{/if}

		<button type="submit" disabled={submitting || username.trim() === '' || password === ''}>
			{submitting ? 'Creating administrator…' : 'Create administrator'}
		</button>
	</form>
</section>

<style>
	.setup {
		max-width: 26rem;
		margin: 3rem auto;
		padding: 2rem;
		border: 1px solid #e4e7eb;
		border-radius: 8px;
		background: #fff;
	}

	.setup h1 {
		margin-top: 0;
	}

	.setup__intro {
		color: #52606d;
		font-size: 0.9rem;
	}

	.setup__form {
		display: flex;
		flex-direction: column;
		gap: 0.9rem;
	}

	.setup__form label {
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
		font-size: 0.85rem;
	}

	.setup__form input {
		padding: 0.5rem;
		border: 1px solid #cbd2d9;
		border-radius: 4px;
		font-size: 1rem;
	}

	.setup__error {
		margin: 0;
		color: #a4262c;
		font-size: 0.85rem;
	}

	.setup__form button {
		padding: 0.6rem;
		border: none;
		border-radius: 4px;
		background: #2f6fed;
		color: #fff;
		font-size: 0.95rem;
		cursor: pointer;
	}

	.setup__form button:disabled {
		background: #9aa5b1;
		cursor: not-allowed;
	}
</style>
