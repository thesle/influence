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
	// Groups management (task 23.3, Req 5.2). Lists tenant Groups and lets an
	// Admin_Group member create and rename them. The Admin_Group itself is
	// flagged and its name is not editable here (renaming or removing the admin
	// group is out of scope for this screen).

	import { onMount } from 'svelte';
	import { AdminApi, adminApi, ApiError, type AdminGroup } from '$lib/adminApi';

	let { client = adminApi }: { client?: AdminApi } = $props();

	let loading = $state(true);
	let errorMessage = $state<string | null>(null);
	let groups = $state<AdminGroup[]>([]);

	let newName = $state('');
	let submitting = $state(false);
	let editingId = $state<number | null>(null);
	let editingName = $state('');

	async function load() {
		loading = true;
		errorMessage = null;
		try {
			const resp = await client.listGroups();
			groups = resp.groups ?? [];
		} catch (err) {
			errorMessage = err instanceof ApiError ? err.message : 'Could not load groups.';
		} finally {
			loading = false;
		}
	}

	async function createGroup(event: SubmitEvent) {
		event.preventDefault();
		if (newName.trim() === '') return;
		submitting = true;
		errorMessage = null;
		try {
			await client.createGroup({ name: newName.trim() });
			newName = '';
			await load();
		} catch (err) {
			errorMessage = err instanceof ApiError ? err.message : 'Could not create group.';
		} finally {
			submitting = false;
		}
	}

	function startEdit(group: AdminGroup) {
		editingId = group.groupId;
		editingName = group.name;
	}

	function cancelEdit() {
		editingId = null;
		editingName = '';
	}

	async function saveEdit(group: AdminGroup) {
		if (editingName.trim() === '') return;
		errorMessage = null;
		try {
			await client.updateGroup(group.groupId, { name: editingName.trim() });
			cancelEdit();
			await load();
		} catch (err) {
			errorMessage = err instanceof ApiError ? err.message : 'Could not update group.';
		}
	}

	onMount(load);
</script>

<section class="groups" aria-label="Groups">
	<h2>Groups</h2>

	<form class="groups__create" onsubmit={createGroup}>
		<label>
			Group name
			<input bind:value={newName} required placeholder="Editors" />
		</label>
		<button type="submit" disabled={submitting || newName.trim() === ''}>
			Add group
		</button>
	</form>

	{#if errorMessage}
		<p class="groups__error" role="alert">{errorMessage}</p>
	{/if}

	{#if loading}
		<p role="status">Loading groups…</p>
	{:else if groups.length === 0}
		<p class="groups__empty">No groups in this tenant yet.</p>
	{:else}
		<table class="groups__table">
			<thead>
				<tr>
					<th>Name</th>
					<th>Role</th>
					<th>Actions</th>
				</tr>
			</thead>
			<tbody>
				{#each groups as group (group.groupId)}
					<tr>
						<td>
							{#if editingId === group.groupId}
								<input bind:value={editingName} aria-label="Group name" />
							{:else}
								{group.name}
							{/if}
						</td>
						<td>{group.isAdmin ? 'Admin_Group' : 'Standard'}</td>
						<td>
							{#if group.isAdmin}
								<span class="groups__locked">—</span>
							{:else if editingId === group.groupId}
								<button type="button" onclick={() => saveEdit(group)}>Save</button>
								<button type="button" onclick={cancelEdit}>Cancel</button>
							{:else}
								<button type="button" onclick={() => startEdit(group)}>Rename</button>
							{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</section>

<style>
	.groups__create {
		display: flex;
		align-items: flex-end;
		gap: 0.75rem;
		margin-bottom: 1rem;
	}

	.groups__create label {
		display: flex;
		flex-direction: column;
		font-size: 0.8rem;
		gap: 0.2rem;
	}

	.groups__table {
		border-collapse: collapse;
		width: 100%;
	}

	.groups__table th,
	.groups__table td {
		border-bottom: 1px solid #e4e7eb;
		padding: 0.4rem 0.6rem;
		text-align: left;
	}

	.groups__error {
		color: #a4262c;
	}

	.groups__empty {
		color: #52606d;
	}

	.groups__locked {
		color: #9aa5b1;
	}
</style>
