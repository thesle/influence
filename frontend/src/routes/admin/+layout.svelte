<script lang="ts">
	// Admin section gate (task 23.3, Req 5). Every /admin/* page renders inside
	// this layout, which fetches the caller's identity from GET /api/me and only
	// reveals the admin UI to Admin_Group members (isAdmin). Non-admins see an
	// access-denied notice instead of the admin navigation and pages.
	//
	// This is defence-in-depth for the UI: the backend independently enforces the
	// rule and returns 403 ADMIN_REQUIRED for non-admins hitting admin routes
	// (design.md — Authorization Middleware; Req 5.5). A 403 while loading /api/me
	// is treated the same as "not an admin".

	import { onMount } from 'svelte';
	import { page } from '$app/stores';
	import { adminApi, ApiError, type Me } from '$lib/adminApi';

	let { children } = $props();

	let loading = $state(true);
	let me = $state<Me | null>(null);
	let loadError = $state<string | null>(null);

	const isAdmin = $derived(me?.isAdmin === true);

	const tabs = [
		{ href: '/admin/users', label: 'Users' },
		{ href: '/admin/groups', label: 'Groups' },
		{ href: '/admin/sphere-access', label: 'Sphere access' },
		{ href: '/admin/security', label: 'Security' }
	];

	onMount(async () => {
		try {
			me = await adminApi.me();
		} catch (err) {
			if (err instanceof ApiError && (err.isAdminRequired || err.status === 401)) {
				me = null;
			} else {
				loadError = err instanceof ApiError ? err.message : 'Could not verify access.';
			}
		} finally {
			loading = false;
		}
	});
</script>

<div class="admin">
	<h1>Administration</h1>

	{#if loading}
		<p role="status">Checking access…</p>
	{:else if loadError}
		<p class="admin__error" role="alert">{loadError}</p>
	{:else if !isAdmin}
		<p class="admin__denied" role="alert" data-testid="admin-denied">
			Administrative privileges are required to view this section.
		</p>
	{:else}
		<nav class="admin__tabs" aria-label="Admin sections">
			{#each tabs as tab (tab.href)}
				<a
					href={tab.href}
					class="admin__tab"
					aria-current={$page.url.pathname === tab.href ? 'page' : undefined}
				>
					{tab.label}
				</a>
			{/each}
		</nav>

		<div class="admin__content">
			{@render children()}
		</div>
	{/if}
</div>

<style>
	.admin {
		max-width: 60rem;
	}

	.admin__tabs {
		display: flex;
		gap: 0.25rem;
		border-bottom: 1px solid #e4e7eb;
		margin-bottom: 1rem;
	}

	.admin__tab {
		padding: 0.5rem 0.9rem;
		text-decoration: none;
		color: #52606d;
		border-bottom: 2px solid transparent;
	}

	.admin__tab[aria-current='page'] {
		color: #1f2933;
		border-bottom-color: #1f2933;
		font-weight: 600;
	}

	.admin__denied,
	.admin__error {
		color: #a4262c;
	}
</style>
