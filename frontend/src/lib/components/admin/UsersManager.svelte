<!--
  Influence — a self-hostable documentation platform.
  Copyright (C) 2026  Conrad Smith

  This program is free software: you can redistribute it and/or modify
  it under the terms of the GNU General Public License as published by
  the Free Software Foundation, version 3.

  This program is distributed in the hope that it will be useful,
  but WITHOUT ANY WARRANTY; without even the implied warranty of
  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
  GNU General Public License for more details.

  You should have received a copy of the GNU General Public License
  along with this program.  If not, see <https://www.gnu.org/licenses/>.
-->

<script lang="ts">
	// Users management (task 23.3, Req 5.1, 5.4). Lists tenant users and lets an
	// Admin_Group member create, rename, and (de)activate them. Deactivation is a
	// reversible toggle: the backend retains the account and Group memberships
	// while denying access (Req 5.4), so the UI shows an "Activate"/"Deactivate"
	// action rather than a destructive delete.
	//
	// Self-contained: it takes an injectable AdminApi client and renders only its
	// own table + form. Route-level admin gating lives in the /admin layout.

	import { onMount } from 'svelte';
	import { AdminApi, adminApi, ApiError, type AdminUser } from '$lib/adminApi';

	let { client = adminApi }: { client?: AdminApi } = $props();

	let loading = $state(true);
	let errorMessage = $state<string | null>(null);
	let users = $state<AdminUser[]>([]);

	let newUsername = $state('');
	let newDisplayName = $state('');
	let submitting = $state(false);

	async function load() {
		loading = true;
		errorMessage = null;
		try {
			const resp = await client.listUsers();
			users = resp.users ?? [];
		} catch (err) {
			errorMessage = err instanceof ApiError ? err.message : 'Could not load users.';
		} finally {
			loading = false;
		}
	}

	async function createUser(event: SubmitEvent) {
		event.preventDefault();
		if (newUsername.trim() === '') return;
		submitting = true;
		errorMessage = null;
		try {
			await client.createUser({
				username: newUsername.trim(),
				displayName: newDisplayName.trim() || newUsername.trim()
			});
			newUsername = '';
			newDisplayName = '';
			await load();
		} catch (err) {
			errorMessage = err instanceof ApiError ? err.message : 'Could not create user.';
		} finally {
			submitting = false;
		}
	}

	async function toggleActive(user: AdminUser) {
		errorMessage = null;
		try {
			await client.updateUser(user.userId, { active: !user.active });
			await load();
		} catch (err) {
			errorMessage = err instanceof ApiError ? err.message : 'Could not update user.';
		}
	}

	onMount(load);
</script>

<section class="users" aria-label="Users">
	<h2>Users</h2>

	<form class="users__create" onsubmit={createUser}>
		<label>
			Username
			<input bind:value={newUsername} required placeholder="jdoe" />
		</label>
		<label>
			Display name
			<input bind:value={newDisplayName} placeholder="Jane Doe" />
		</label>
		<button type="submit" disabled={submitting || newUsername.trim() === ''}>
			Add user
		</button>
	</form>

	{#if errorMessage}
		<p class="users__error" role="alert">{errorMessage}</p>
	{/if}

	{#if loading}
		<p role="status">Loading users…</p>
	{:else if users.length === 0}
		<p class="users__empty">No users in this tenant yet.</p>
	{:else}
		<table class="users__table">
			<thead>
				<tr>
					<th>Username</th>
					<th>Display name</th>
					<th>Status</th>
					<th>Actions</th>
				</tr>
			</thead>
			<tbody>
				{#each users as user (user.userId)}
					<tr class:users__row--inactive={!user.active}>
						<td>{user.username}</td>
						<td>{user.displayName}</td>
						<td>{user.active ? 'Active' : 'Deactivated'}</td>
						<td>
							<button type="button" onclick={() => toggleActive(user)}>
								{user.active ? 'Deactivate' : 'Activate'}
							</button>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</section>

<style>
	.users__create {
		display: flex;
		align-items: flex-end;
		gap: 0.75rem;
		margin-bottom: 1rem;
		flex-wrap: wrap;
	}

	.users__create label {
		display: flex;
		flex-direction: column;
		font-size: 0.8rem;
		gap: 0.2rem;
	}

	.users__table {
		border-collapse: collapse;
		width: 100%;
	}

	.users__table th,
	.users__table td {
		border-bottom: 1px solid #e4e7eb;
		padding: 0.4rem 0.6rem;
		text-align: left;
	}

	.users__row--inactive {
		color: #9aa5b1;
	}

	.users__error {
		color: #a4262c;
	}

	.users__empty {
		color: #52606d;
	}
</style>
