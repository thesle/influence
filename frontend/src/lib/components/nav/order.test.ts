import { describe, expect, test } from 'vitest';
import fc from 'fast-check';
import type { Sphere } from '$lib/api';
import { compareAlphabetical, orderSpheres } from './order';

function sphere(partial: Partial<Sphere> & { recordId: string; name: string }): Sphere {
	return {
		shortId: partial.shortId ?? partial.recordId,
		createdAt: partial.createdAt ?? '2024-01-01T00:00:00Z',
		...partial
	} as Sphere;
}

describe('orderSpheres — Req 7.3/7.5, 21.1', () => {
	test('circled group is shown above the alphabetical remainder', () => {
		const spheres = [
			sphere({ recordId: 'a', name: 'Zebra' }),
			sphere({ recordId: 'b', name: 'Alpha', circled: true }),
			sphere({ recordId: 'c', name: 'Mango' })
		];
		const out = orderSpheres(spheres);
		expect(out.circled.map((s) => s.recordId)).toEqual(['b']);
		// Remainder alphabetical: Mango before Zebra.
		expect(out.alphabetical.map((s) => s.name)).toEqual(['Mango', 'Zebra']);
		// all = circled first, then alphabetical.
		expect(out.all.map((s) => s.recordId)).toEqual(['b', 'c', 'a']);
	});

	test('alphabetical remainder is case-insensitive with Record_ID tie-break', () => {
		const spheres = [
			sphere({ recordId: 'z', name: 'bravo' }),
			sphere({ recordId: 'a', name: 'Bravo' }),
			sphere({ recordId: 'm', name: 'alpha' })
		];
		const out = orderSpheres(spheres);
		// alpha first; then the two "Bravo"s tie-broken by Record_ID (a before z).
		expect(out.alphabetical.map((s) => s.recordId)).toEqual(['m', 'a', 'z']);
	});

	test('explicit circledOrder defines membership and order', () => {
		const spheres = [
			sphere({ recordId: 'a', name: 'Alpha' }),
			sphere({ recordId: 'b', name: 'Bravo' }),
			sphere({ recordId: 'c', name: 'Charlie' })
		];
		const out = orderSpheres(spheres, { circledOrder: ['c', 'a'] });
		expect(out.circled.map((s) => s.recordId)).toEqual(['c', 'a']);
		expect(out.alphabetical.map((s) => s.recordId)).toEqual(['b']);
	});

	// Property: circled always precede non-circled; the non-circled remainder is
	// always sorted per compareAlphabetical; and no Sphere is lost or duplicated.
	test('property: partition preserves all spheres and orders them per Req 7', () => {
		const sphereArb = fc.record({
			recordId: fc.string({ minLength: 1, maxLength: 6 }),
			name: fc.string({ maxLength: 8 }),
			circled: fc.boolean()
		});
		fc.assert(
			fc.property(
				fc.uniqueArray(sphereArb, { selector: (s) => s.recordId, maxLength: 30 }),
				(raw) => {
					const spheres = raw.map((r) =>
						sphere({ recordId: r.recordId, name: r.name, circled: r.circled })
					);
					const out = orderSpheres(spheres);

					// No loss / no duplication.
					expect(out.all.length).toBe(spheres.length);
					expect(new Set(out.all.map((s) => s.recordId)).size).toBe(spheres.length);

					// Circled group holds exactly the circled spheres.
					expect(out.circled.every((s) => s.circled === true)).toBe(true);
					expect(out.alphabetical.every((s) => s.circled !== true)).toBe(true);

					// all = circled ++ alphabetical.
					expect(out.all.map((s) => s.recordId)).toEqual([
						...out.circled.map((s) => s.recordId),
						...out.alphabetical.map((s) => s.recordId)
					]);

					// Alphabetical remainder is sorted per the rule.
					for (let i = 1; i < out.alphabetical.length; i++) {
						expect(compareAlphabetical(out.alphabetical[i - 1], out.alphabetical[i])).toBeLessThanOrEqual(
							0
						);
					}
				}
			),
			{ numRuns: 200 }
		);
	});
});
