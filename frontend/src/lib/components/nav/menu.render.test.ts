import { describe, expect, test } from 'vitest';
import { render } from 'svelte/server';
import type { Sphere } from '$lib/api';
import { spherePath, sphereShortId } from '$lib/api';
import SphereList from './SphereList.svelte';
import EmptyContents from './EmptyContents.svelte';

// Component/example tests for the left-menu navigation (Req 21.1, 21.2, 21.3,
// 15.2). These render the actual Svelte components to HTML (server-side, no DOM
// dependency) and assert on the rendered markup. They complement the pure-logic
// tests in order.test.ts / tree.test.ts:
//   - order.test.ts proves orderSpheres partitions/sorts correctly;
//   - here we prove SphereList actually paints the circled group ABOVE the
//     alphabetical remainder, emits an empty-contents indicator, and builds hrefs
//     from Short-UUID Record_IDs.
// The universal ordering invariant is covered by the backend Property 5 test.

function sphere(partial: Partial<Sphere> & { recordId: string; name: string }): Sphere {
	return {
		shortId: partial.shortId ?? partial.recordId,
		createdAt: partial.createdAt ?? '2024-01-01T00:00:00Z',
		...partial
	} as Sphere;
}

/** Render SphereList to an HTML string for markup assertions. */
function renderList(props: {
	spheres: Sphere[];
	activeSphereId?: string | null;
	activePolygonId?: string | null;
}): string {
	return render(SphereList, { props }).body;
}

/** All `href` values in document order. */
function hrefs(html: string): string[] {
	return [...html.matchAll(/href="([^"]*)"/g)].map((m) => m[1]);
}

describe('SphereList render — Req 21.1, 15.2', () => {
	test('circled Spheres render above the alphabetical remainder', () => {
		const spheres = [
			sphere({ recordId: 'a', shortId: 'sa', name: 'Zebra' }),
			sphere({ recordId: 'b', shortId: 'sb', name: 'Alpha', circled: true }),
			sphere({ recordId: 'c', shortId: 'sc', name: 'Mango' })
		];
		const html = renderList({ spheres });

		// A dedicated Circled section exists and is emitted before the remainder.
		const circledIdx = html.indexOf('aria-label="Circled Spheres"');
		const remainderIdx = html.indexOf('aria-label="Spheres"');
		expect(circledIdx).toBeGreaterThanOrEqual(0);
		expect(remainderIdx).toBeGreaterThan(circledIdx);

		// Every Sphere name is present.
		expect(html).toContain('Alpha');
		expect(html).toContain('Mango');
		expect(html).toContain('Zebra');

		// Document order: circled 'Alpha' precedes the alphabetical remainder
		// (Mango before Zebra). This is the visible circled-above-alphabetical rule.
		const posAlpha = html.indexOf('>Alpha<');
		const posMango = html.indexOf('>Mango<');
		const posZebra = html.indexOf('>Zebra<');
		expect(posAlpha).toBeLessThan(posMango);
		expect(posMango).toBeLessThan(posZebra);
	});

	test('with no circled Spheres, only the remainder section renders (alphabetized)', () => {
		const spheres = [
			sphere({ recordId: 'a', shortId: 'sa', name: 'Charlie' }),
			sphere({ recordId: 'b', shortId: 'sb', name: 'alpha' }),
			sphere({ recordId: 'c', shortId: 'sc', name: 'Bravo' })
		];
		const html = renderList({ spheres });

		// No circled group is painted when nothing is circled.
		expect(html).not.toContain('aria-label="Circled Spheres"');
		expect(html).not.toContain('>Circled<');

		// Case-insensitive ascending order in the rendered markup.
		const posAlpha = html.indexOf('>alpha<');
		const posBravo = html.indexOf('>Bravo<');
		const posCharlie = html.indexOf('>Charlie<');
		expect(posAlpha).toBeLessThan(posBravo);
		expect(posBravo).toBeLessThan(posCharlie);
	});

	test('empty accessible list shows the no-Spheres indicator', () => {
		const html = renderList({ spheres: [] });
		expect(html).toContain('No accessible Spheres.');
	});

	test('Sphere links are built from the Short-UUID Record_ID (Req 15.2)', () => {
		const spheres = [
			sphere({ recordId: 'canonical-uuid-1', shortId: 'ShortOne', name: 'One' }),
			sphere({ recordId: 'canonical-uuid-2', shortId: 'ShortTwo', name: 'Two', circled: true })
		];
		const html = renderList({ spheres });
		const links = hrefs(html);

		// Routes use the Short form, not the canonical UUID.
		expect(links).toContain('/s/ShortOne');
		expect(links).toContain('/s/ShortTwo');
		expect(links).not.toContain('/s/canonical-uuid-1');
		expect(links).not.toContain('/s/canonical-uuid-2');

		// And they match spherePath, which encodes the Short-UUID Record_ID.
		for (const s of spheres) {
			expect(links).toContain(spherePath(s));
			expect(spherePath(s)).toBe(`/s/${sphereShortId(s)}`);
		}
	});

	test('a missing Short form falls back to the canonical Record_ID so links are never empty', () => {
		const s = sphere({ recordId: 'canonical-uuid-3', name: 'Fallback' });
		// Simulate an API row that lacks a shortId.
		(s as { shortId?: string }).shortId = '';
		const html = renderList({ spheres: [s] });
		expect(hrefs(html)).toContain('/s/canonical-uuid-3');
	});
});

describe('EmptyContents render — Req 21.3', () => {
	test('empty-contents indicator renders its note text and role', () => {
		const html = render(EmptyContents).body;
		expect(html).toContain('This Sphere has no contents yet.');
		expect(html).toContain('role="note"');
	});
});
