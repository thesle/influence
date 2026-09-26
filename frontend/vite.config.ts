import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

// Dev proxy: forward the frontend's /api calls to the Go backend so the
// SvelteKit dev server and the API share an origin during development.
const API_TARGET = process.env.INFLUENCE_API_URL ?? 'http://localhost:8080';

export default defineConfig({
	plugins: [sveltekit()],
	server: {
		proxy: {
			'/api': {
				target: API_TARGET,
				changeOrigin: true
			}
		}
	},
	test: {
		include: ['src/**/*.{test,spec}.{js,ts}']
	}
});
