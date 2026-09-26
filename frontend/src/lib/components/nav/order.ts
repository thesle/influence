// Left-menu Sphere ordering (Req 7.3, 7.5, 21.1).
//
// The rule has two parts:
//   - The circled group is shown ABOVE the alphabetical remainder (Req 7.3) and
//     is reorderable in the user's chosen order (Req 7.4), so within the group
//     we preserve the caller-provided order rather than re-sorting.
//   - The remaining (non-circled) accessible Spheres are shown in ascending
//     order by name using case-insensitive comparison, with Record_ID as the
//     tie-break (Req 7.5).
//
// This logic runs on whatever `Sphere[]` the API returns. Circled membership is
// taken from an explicit `circled` flag when present; when absent, an optional
// caller-supplied set of circled Record_IDs (persisted client-side order) is
// used. This keeps the menu correct whether the backend pre-orders the list or
// not.

import type { Sphere } from '$lib/api';

/** A Sphere partitioned into its display group. */
export interface OrderedSpheres {
	/** Circled group, in the user's order, shown first (Req 7.3, 7.4). */
	circled: Sphere[];
	/** Non-circled remainder, alphabetized case-insensitively (Req 7.5). */
	alphabetical: Sphere[];
	/** Convenience: circled group followed by the alphabetical remainder. */
	all: Sphere[];
}

/** Optional inputs describing which Spheres are circled and in what order. */
export interface CircleContext {
	/**
	 * Record_IDs of circled Spheres in the user's chosen display order. When
	 * provided this defines both membership and the within-group order,
	 * overriding any per-Sphere `circled` flag.
	 */
	circledOrder?: readonly string[];
}

/**
 * Case-insensitive ascending compare by name, with Record_ID as a deterministic
 * tie-break (Req 7.5). Uses locale-aware, case-insensitive collation for names
 * and a stable code-point compare for the Record_ID tie-break.
 */
export function compareAlphabetical(a: Sphere, b: Sphere): number {
	const byName = a.name.localeCompare(b.name, undefined, { sensitivity: 'accent' });
	if (byName !== 0) {
		return byName;
	}
	// Deterministic tie-break so equal (case-insensitively equal) names always
	// order the same way regardless of input order.
	if (a.recordId < b.recordId) return -1;
	if (a.recordId > b.recordId) return 1;
	return 0;
}

function isCircled(sphere: Sphere, circledSet: Set<string> | null): boolean {
	if (circledSet) {
		return circledSet.has(sphere.recordId);
	}
	return sphere.circled === true;
}

/**
 * Partition and order the accessible Spheres for the left menu per Req 7.
 *
 * @param spheres the caller's accessible Spheres, in any order
 * @param ctx     optional circled-order context (client-persisted order)
 */
export function orderSpheres(spheres: readonly Sphere[], ctx: CircleContext = {}): OrderedSpheres {
	const circledOrder = ctx.circledOrder;
	const circledSet = circledOrder ? new Set(circledOrder) : null;

	const circledMembers: Sphere[] = [];
	const alphabetical: Sphere[] = [];

	for (const sphere of spheres) {
		if (isCircled(sphere, circledSet)) {
			circledMembers.push(sphere);
		} else {
			alphabetical.push(sphere);
		}
	}

	// Within-group order for the circled group.
	let circled: Sphere[];
	if (circledOrder) {
		// Honor the user's explicit order; append any circled members missing
		// from the order list (defensive) after those that are listed.
		const rank = new Map(circledOrder.map((id, i) => [id, i] as const));
		circled = [...circledMembers].sort((a, b) => {
			const ra = rank.get(a.recordId) ?? Number.MAX_SAFE_INTEGER;
			const rb = rank.get(b.recordId) ?? Number.MAX_SAFE_INTEGER;
			if (ra !== rb) return ra - rb;
			return compareAlphabetical(a, b);
		});
	} else {
		// No explicit order supplied: preserve the source order of the circled
		// group (the backend returns it in the user's order — Req 7.4).
		circled = circledMembers;
	}

	// Alphabetical remainder (Req 7.5).
	alphabetical.sort(compareAlphabetical);

	return { circled, alphabetical, all: [...circled, ...alphabetical] };
}
