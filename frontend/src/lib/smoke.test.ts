import { describe, expect, test } from 'vitest';
import fc from 'fast-check';

// Smoke test confirming the property-testing toolchain (fast-check + vitest)
// is wired into the frontend scaffold. Feature-level property tests live with
// the components they validate in later tasks.
describe('frontend scaffold', () => {
	test('fast-check runs a trivial property', () => {
		fc.assert(
			fc.property(fc.integer(), fc.integer(), (a, b) => {
				expect(a + b).toBe(b + a);
			}),
			{ numRuns: 100 }
		);
	});
});
