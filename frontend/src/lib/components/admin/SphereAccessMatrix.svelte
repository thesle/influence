<script lang="ts">
	// Per-Sphere access matrix (task 23.3, Req 5.3). Renders a grid of Spheres ×
	// Groups; each cell exposes an access-level selector (none/read/write) and a
	// Reveal toggle. Changing either upserts the grant via
	// PUT /api/admin/sphere-access.
	//
	// Reveal is only meaningful when a Group has access, so the toggle is
	// disabled for `none` cells and any stored reveal is ignored there
	// (grantFor enforces this). Cells default to the most-restrictive
	// none/no-reveal when no grant exists.

	import { onMount } from 'svelte';
	import {
		AdminApi,
		adminApi,
		ApiError,
		grantFor,
		type AccessLevel,
		type AdminGroup,
		type Sphere,
		type SphereAccessGrant
	} from '$lib/adminApi';

	let { client = adminApi }: { client?: AdminApi } = $props();

	let loading = $state(true);
	let errorMessage = $state<string | null>(null);
	let spheres = $state<Sphere[]>([]);
	let groups = $state<AdminGroup[]>([]);
	let grants = $state<SphereAccessGrant[]>([]);

	const ACCESS_OPTIONS: AccessLevel[] = ['none', 'read', 'write'];

	async function load() {
		loading = true;
		errorMessage = null;
		try {
			const data = await client.getSphereAccess();
			spheres = data.spheres ?? [];
			groups = data.groups ?? [];
			grants = data.grants ?? [];
		} catch (err) {
			errorMessage =
				err instanceof ApiError ? err.message : 'Could not load Sphere access.';
		} finally {
			loading = false;
		}
	}

	// Replace (or insert) a grant in the local list so the UI reflects the change
	// immediately after a successful upsert.
	function applyLocalGrant(next: SphereAccessGrant) {
		const rest = grants.filter(
			(g) => !(g.sphereRecordId === next.sphereRecordId && g.groupId === next.groupId)
		);
		grants = [...rest, next];
	}

	async function persist(next: SphereAccessGrant) {
		errorMessage = null;
		try {
			await client.setSphereAccess(next);
			applyLocalGrant(next);
		} catch (err) {
			errorMessage =
				err instanceof ApiError ? err.message : 'Could not update Sphere access.';
		}
	}

	async function changeAccess(sphere: Sphere, group: AdminGroup, access: AccessLevel) {
		const current = grantFor(grants, sphere.recordId, group.groupId);
		// Dropping to `none` also clears Reveal (it is meaningless without access).
		const reveal = access === 'none' ? false : current.reveal;
		await persist({
			sphereRecordId: sphere.recordId,
			groupId: group.groupId,
			access,
			reveal
		});
	}

	async function toggleReveal(sphere: Sphere, group: AdminGroup, reveal: boolean) {
		const current = grantFor(grants, sphere.recordId, group.groupId);
		if (current.access === 'none') return; // guarded by disabled control
		await persist({
			sphereRecordId: sphere.recordId,
			groupId: group.groupId,
			access: current.access,
			reveal
		});
	}

	onMount(load);
</script>

<section class="matrix" aria-label="Sphere access">
	<h2>Sphere access</h2>

	{#if errorMessage}
		<p class="matrix__error" role="alert">{errorMessage}</p>
	{/if}

	{#if loading}
		<p role="status">Loading Sphere access…</p>
	{:else if spheres.length === 0 || groups.length === 0}
		<p class="matrix__empty">
			{spheres.length === 0
				? 'No Spheres to grant access to yet.'
				: 'No Groups to assign access to yet.'}
		</p>
	{:else}
		<div class="matrix__scroll">
			<table class="matrix__table">
				<thead>
					<tr>
						<th scope="col">Sphere \ Group</th>
						{#each groups as group (group.groupId)}
							<th scope="col">{group.name}</th>
						{/each}
					</tr>
				</thead>
				<tbody>
					{#each spheres as sphere (sphere.recordId)}
						<tr>
							<th scope="row">{sphere.name}</th>
							{#each groups as group (group.groupId)}
								{@const cell = grantFor(grants, sphere.recordId, group.groupId)}
								<td class="matrix__cell">
									<label class="matrix__access">
										<span class="matrix__label">Access</span>
										<select
											aria-label={`Access for ${group.name} on ${sphere.name}`}
											value={cell.access}
											onchange={(e) =>
												changeAccess(
													sphere,
													group,
													e.currentTarget.value as AccessLevel
												)}
										>
											{#each ACCESS_OPTIONS as option (option)}
												<option value={option}>{option}</option>
											{/each}
										</select>
									</label>
									<label class="matrix__reveal">
										<input
											type="checkbox"
											aria-label={`Reveal for ${group.name} on ${sphere.name}`}
											checked={cell.reveal}
											disabled={cell.access === 'none'}
											onchange={(e) =>
												toggleReveal(sphere, group, e.currentTarget.checked)}
										/>
										Reveal
									</label>
								</td>
							{/each}
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<style>
	.matrix__scroll {
		overflow-x: auto;
	}

	.matrix__table {
		border-collapse: collapse;
	}

	.matrix__table th,
	.matrix__table td {
		border: 1px solid #e4e7eb;
		padding: 0.5rem 0.75rem;
		text-align: left;
		vertical-align: top;
	}

	.matrix__cell {
		min-width: 9rem;
	}

	.matrix__access {
		display: flex;
		flex-direction: column;
		font-size: 0.75rem;
		gap: 0.2rem;
		margin-bottom: 0.35rem;
	}

	.matrix__label {
		color: #7b8794;
	}

	.matrix__reveal {
		display: flex;
		align-items: center;
		gap: 0.3rem;
		font-size: 0.8rem;
	}

	.matrix__error {
		color: #a4262c;
	}

	.matrix__empty {
		color: #52606d;
	}
</style>
