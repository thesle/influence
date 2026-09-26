<script lang="ts">
	// App shell: top bar (search, quick searches, bookmarks, user menu) + left
	// menu (accessible Spheres, ordered per Req 7) + main content region
	// (design.md — "Frontend Structure"). The left menu lists only the Spheres
	// the caller can access (Req 21.1); the active Sphere/Polygon are derived
	// from the current route so a deep link opens the right subtree.
	import { page } from '$app/stores';
	import { InfluenceApi, type Sphere } from '$lib/api';
	import type { EntryScreen } from '$lib/setupApi';
	import TopBar from '$lib/components/nav/TopBar.svelte';
	import SphereList from '$lib/components/nav/SphereList.svelte';
	import SetupScreen from '$lib/components/setup/SetupScreen.svelte';
	import LoginScreen from '$lib/components/setup/LoginScreen.svelte';
	import TwoFactorEnrol from '$lib/components/setup/TwoFactorEnrol.svelte';

	let { data, children } = $props();

	const api = new InfluenceApi();

	const spheres = $derived((data.spheres ?? []) as Sphere[]);
	const sphereError = $derived(data.sphereError as string | null);

	// First-run gate (Req 31.1, 31.3) plus the login/2FA flow (Req 33.4, 33.7).
	// The load fn resolves the initial entry screen (Setup_Screen vs login). The
	// login screen then drives client-side transitions without a full reload:
	//  - a successful login flips to 'app' (the authenticated shell);
	//  - a restricted must_enrol session flips to 'enrol' so the user lands on
	//    the enrolment view (Req 33.7), then to 'app' once enrolment completes.
	// A real navigation would re-run the load and arrive at the same place.
	type ShellScreen = EntryScreen | 'enrol' | 'app';
	let entryOverride = $state<ShellScreen | null>(null);
	const entryScreen = $derived(entryOverride ?? ((data.entryScreen ?? 'login') as ShellScreen));

	function onSetupComplete() {
		entryOverride = 'login';
	}

	function onAuthenticated() {
		entryOverride = 'app';
	}

	function onEnrolmentRequired() {
		entryOverride = 'enrol';
	}

	function onEnrolmentComplete() {
		entryOverride = 'app';
	}

	// Active Sphere: the Short-UUID from a /s/{shortId} route, matched back to a
	// loaded Sphere's Record_ID so the list can highlight/expand it.
	const activeSphereShortId = $derived($page.params.sphereShortId ?? null);
	const activeSphereId = $derived(
		activeSphereShortId
			? (spheres.find(
					(s) => s.shortId === activeSphereShortId || s.recordId === activeSphereShortId
				)?.recordId ?? null)
			: null
	);
	// Active Polygon: the Record_ID from a /p/{id} route (route param is `id`).
	const activePolygonId = $derived($page.params.id ?? null);
</script>

{#if entryScreen === 'setup'}
	<SetupScreen oncomplete={onSetupComplete} />
{:else if entryScreen === 'login'}
	<LoginScreen onauthenticated={onAuthenticated} onenrolment={onEnrolmentRequired} />
{:else if entryScreen === 'enrol'}
	<TwoFactorEnrol oncomplete={onEnrolmentComplete} />
{:else}
	<div class="app-shell">
		<TopBar />

		<div class="app-body">
			<nav class="left-menu" aria-label="Spheres">
				{#if sphereError}
					<p class="left-menu__error" role="alert">{sphereError}</p>
				{/if}
				<SphereList {spheres} {activeSphereId} {activePolygonId} {api} />
			</nav>

			<main class="content">
				{@render children()}
			</main>
		</div>
	</div>
{/if}

<style>
	:global(body) {
		margin: 0;
		font-family:
			system-ui,
			-apple-system,
			'Segoe UI',
			Roboto,
			sans-serif;
	}

	.app-shell {
		display: flex;
		flex-direction: column;
		min-height: 100vh;
	}

	.app-body {
		display: flex;
		flex: 1;
		min-height: 0;
	}

	.left-menu {
		width: 260px;
		border-right: 1px solid #e4e7eb;
		background: #f5f7fa;
		padding: 1rem;
		overflow-y: auto;
	}

	.left-menu__error {
		margin: 0 0 0.75rem;
		font-size: 0.85rem;
		color: #cf1124;
	}

	.content {
		flex: 1;
		padding: 1.5rem;
	}
</style>
