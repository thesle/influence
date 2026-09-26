import adapter from '@sveltejs/adapter-node';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
const config = {
	preprocess: vitePreprocess(),
	kit: {
		// Node adapter: builds a standalone Node server. The single Go binary can
		// reverse-proxy to it, or the build can be swapped for adapter-static if a
		// fully static SPA is preferred later (see design: "SPA/adapter-static or node adapter").
		adapter: adapter()
	}
};

export default config;
