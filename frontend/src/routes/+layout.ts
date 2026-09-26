// Influence — a self-hostable documentation platform.
// Copyright (C) 2026  Conrad Smith
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

// Layout load: decide the entry screen and, for the app shell, fetch the
// caller's accessible Spheres for the left menu (Req 21.1). Uses SvelteKit's
// scoped `fetch` so requests are same-origin and, during SSR, carry the
// incoming cookies to the Go API.
//
// First-run gate (Req 31.1, 31.3): the shell queries First_Run_State on load.
// While it is true the Setup_Screen is shown in place of the login screen, so
// there is no point loading Spheres (there is no session yet). Once setup is
// done — or the tenant already has an admin — the normal login screen is shown
// and the authenticated app shell loads its Spheres.
//
// A failure to reach the API must not blank the whole shell, so the Sphere list
// falls back to empty and the menu renders its "no accessible Spheres" note; the
// app chrome (top bar, routing) stays usable. A failed setup-state read defaults
// to the login screen (the safe default in setupApi.entryScreenFor).
import { InfluenceApi, type Sphere } from '$lib/api';
import { SetupApi, entryScreenFor, type EntryScreen } from '$lib/setupApi';
import type { LayoutLoad } from './$types';

export const load: LayoutLoad = async ({ fetch }) => {
	const setup = new SetupApi(fetch);
	let entryScreen: EntryScreen = 'login';
	try {
		entryScreen = entryScreenFor(await setup.state());
	} catch {
		// Unknown state → default to the login screen (never trap a provisioned
		// tenant behind the setup form).
		entryScreen = 'login';
	}

	// While the Setup_Screen is showing there is no session, so skip the
	// authenticated Sphere fetch entirely.
	if (entryScreen === 'setup') {
		return { entryScreen, spheres: [] as Sphere[], sphereError: null };
	}

	const api = new InfluenceApi(fetch);
	let spheres: Sphere[] = [];
	let sphereError: string | null = null;
	try {
		spheres = await api.listSpheres();
	} catch (err) {
		sphereError = err instanceof Error ? err.message : 'Failed to load Spheres.';
	}
	return { entryScreen, spheres, sphereError };
};
