<script lang="ts">
	// Tenant Two-Factor policy toggle (task 29.5, Req 33.8). An Admin_Group member
	// sets whether Two_Factor_Authentication is optional or required for the
	// tenant. When required, users who have not enrolled receive a restricted
	// must_enrol session on their next login until they enrol (Req 33.7).
	//
	// PUT /api/admin/2fa-policy echoes the applied { required } flag; the backend
	// takes the tenant from the session, never from client input (Req 32.6). The
	// backend enforces authorization (403 ADMIN_REQUIRED for non-admins, Req 5.5);
	// this component surfaces that inline.
	//
	// There is no GET for the current policy in this task's endpoint set, so the
	// control starts from an optional `initialRequired` (defaults to optional) and
	// reflects the server-confirmed value after each successful save.

	import { untrack } from 'svelte';
	import { TwoFactorApi, twoFactorApi, ApiError } from '$lib/twoFactorApi';

	let {
		client = twoFactorApi,
		initialRequired = false
	}: {
		client?: TwoFactorApi;
		/** Best-known current policy to seed the control (no read endpoint in scope). */
		initialRequired?: boolean;
	} = $props();

	// Seed the control from the best-known policy once. `initialRequired` is only
	// an initial value (there is no read endpoint in scope), so capturing it here
	// is intentional; the control tracks the server-confirmed value thereafter.
	let required = $state(untrack(() => initialRequired === true));
	let saving = $state(false);
	let errorMessage = $state<string | null>(null);
	let savedMessage = $state<string | null>(null);

	async function apply(next: boolean) {
		saving = true;
		errorMessage = null;
		savedMessage = null;
		try {
			const result = await client.setPolicy(next);
			required = result.required;
			savedMessage = result.required
				? 'Two-factor authentication is now required for this tenant.'
				: 'Two-factor authentication is now optional for this tenant.';
		} catch (err) {
			errorMessage =
				err instanceof ApiError ? err.message : 'Could not update the policy. Please try again.';
		} finally {
			saving = false;
		}
	}
</script>

<section class="policy" aria-label="Two-factor authentication policy">
	<h2>Two-factor authentication</h2>
	<p class="policy__intro">
		Choose whether members of this tenant must protect their account with an authenticator app.
		When required, users who have not enrolled are prompted to set it up before they can use the
		app.
	</p>

	<label class="policy__toggle">
		<input
			type="checkbox"
			checked={required}
			disabled={saving}
			onchange={(e) => apply(e.currentTarget.checked)}
		/>
		Require two-factor authentication for all users
	</label>

	{#if saving}
		<p class="policy__status" role="status">Saving…</p>
	{:else if savedMessage}
		<p class="policy__status" role="status">{savedMessage}</p>
	{/if}

	{#if errorMessage}
		<p class="policy__error" role="alert">{errorMessage}</p>
	{/if}
</section>

<style>
	.policy {
		max-width: 40rem;
	}

	.policy__intro {
		color: #52606d;
		font-size: 0.9rem;
	}

	.policy__toggle {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.95rem;
	}

	.policy__status {
		color: #35635b;
		font-size: 0.85rem;
	}

	.policy__error {
		color: #a4262c;
		font-size: 0.85rem;
	}
</style>
