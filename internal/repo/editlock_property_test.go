package repo

import (
	"context"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// Feature: influence, Property 27: Record lock exclusivity
//
// For all pairs of distinct users editing a record-locking Polygon, once one
// user holds the edit lock the other user's save is rejected with the holder's
// identity, and the holder is permitted to save.
//
// Validates: Requirements 25.3, 25.4
//
// The property is exercised over the real production tenant-routing path: a
// file-backed tenant behind a real ConnManager (the twoTenantFixture pattern,
// via newEditLockFixture from editlock_test.go) with an EditLockRepo bound to a
// controllable clock. A record-locking Polygon is seeded once into tenant A so
// every operation targets a valid, accessible Polygon and the only variable is
// the lock contest between users.
//
// Each rapid run draws a small pool of distinct user ids and an arbitrary
// sequence of operations — acquire, save (guard), and release — attributed to
// arbitrary users. The clock is pinned to a single fixed instant for the whole
// run and never advances, so no lock can ever cross the 300s inactivity window
// and reclaim-by-timeout is deliberately excluded: the invariant under test is
// pure exclusivity, not staleness (staleness/reclaim is Req 25.5, covered by
// the unit tests). With time frozen, a held lock is always active, so the only
// variable is the lock contest between the holder and every other user.
//
// A reference model tracks the single expected holder (or "free"). After each
// operation the test asserts the observed behaviour matches the model, and the
// core exclusivity invariant across the whole interleaving:
//
//   - at most one user ever holds the lock at a time;
//   - while a holder is active, every OTHER user's acquire and save is rejected
//     with ErrLocked naming exactly the current holder (Req 25.4);
//   - the holder's own save is always permitted (Req 25.3), and the holder's
//     re-acquire is the idempotent refresh;
//   - a release only succeeds for the holder and frees the lock; a non-holder's
//     release is rejected naming the holder and leaves the holder in place.
func TestRecordLockExclusivityProperty(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	// Pin the clock to a single fixed instant for the entire property. Time
	// never advances during a run, so a held lock can never cross the 300s
	// inactivity window and go stale. This isolates the exclusivity invariant
	// from staleness/reclaim (Req 25.5), which is exercised by the unit tests.
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	f.setNow(base)

	rapid.Check(t, func(rt *rapid.T) {
		// Reset lock state at the start of each run so runs do not leak state
		// into one another: whoever (if anyone) holds the lock releases it.
		if _, err := f.dbA.Exec(`DELETE FROM edit_lock`); err != nil {
			rt.Fatalf("reset lock table: %v", err)
		}
		// Re-pin the frozen clock for a fresh, staleness-free timeline each run.
		f.setNow(base)

		// A pool of at least two distinct users, so exclusivity is always
		// contended by a genuine "other" user.
		userCount := rapid.IntRange(2, 4).Draw(rt, "userCount")
		users := make([]int64, userCount)
		for i := range users {
			users[i] = int64(10 * (i + 1)) // 10, 20, 30, 40
		}

		// The reference model: the expected holder, or 0 when the lock is free.
		// Because time is frozen inside the 300s window, a held lock is always
		// active, so the model tracks only who holds it — never staleness.
		var holder int64 // 0 == free

		steps := rapid.IntRange(1, 40).Draw(rt, "steps")
		for s := 0; s < steps; s++ {
			user := users[rapid.IntRange(0, userCount-1).Draw(rt, "user")]
			op := rapid.SampledFrom([]string{"acquire", "save", "release"}).Draw(rt, "op")

			switch op {
			case "acquire":
				lock, err := f.repo.AcquireLock(ctx, rcUser(user), f.polygonRID)
				switch {
				case holder == 0 || holder == user:
					// Free, or the caller already holds it (idempotent refresh):
					// the acquire succeeds and the caller becomes/stays holder.
					if err != nil {
						rt.Fatalf("acquire by user %d (model holder=%d): unexpected error %v", user, holder, err)
					}
					if lock.HolderUserID != user {
						rt.Fatalf("acquire by user %d: holder = %d, want %d", user, lock.HolderUserID, user)
					}
					holder = user
				default:
					// A different, active user holds the lock: the acquire must
					// be rejected naming exactly the current holder (Req 25.4),
					// and the holder must be unchanged.
					if !isLockedBy(err, holder) {
						rt.Fatalf("acquire by user %d while %d holds: got %v, want ErrLocked{%d}", user, holder, err, holder)
					}
				}

			case "save":
				err := f.repo.GuardSave(ctx, rcUser(user), f.polygonRID)
				switch {
				case holder == 0 || holder == user:
					// Free or the holder itself: the save is permitted
					// (Req 25.3 — the holder may save; a free Polygon is open).
					if err != nil {
						rt.Fatalf("save by user %d (model holder=%d): unexpected error %v", user, holder, err)
					}
				default:
					// Any other user's save while a lock is held is rejected
					// with the holder's identity (Req 25.4).
					if !isLockedBy(err, holder) {
						rt.Fatalf("save by user %d while %d holds: got %v, want ErrLocked{%d}", user, holder, err, holder)
					}
				}

			case "release":
				err := f.repo.ReleaseLock(ctx, rcUser(user), f.polygonRID)
				switch {
				case holder == user:
					// The holder releases: succeeds and frees the lock.
					if err != nil {
						rt.Fatalf("release by holder %d: unexpected error %v", user, err)
					}
					holder = 0
				case holder == 0:
					// Nobody holds it: release reports ErrNoLock and the lock
					// stays free.
					if err == nil {
						rt.Fatalf("release by user %d on free lock: got nil, want ErrNoLock", user)
					}
				default:
					// A non-holder tries to release: rejected naming the holder,
					// lock left in place (a user cannot free another's lock).
					if !isLockedBy(err, holder) {
						rt.Fatalf("release by non-holder %d while %d holds: got %v, want ErrLocked{%d}", user, holder, err, holder)
					}
				}
			}

			// Cross-check the exclusivity invariant against the database after
			// every step: at most one holder, and it is exactly the model's
			// holder. Every non-holder's save must be rejected naming that
			// holder; the holder's own save must be permitted.
			if holder == 0 {
				// Model says free: every user may save.
				for _, u := range users {
					if err := f.repo.GuardSave(ctx, rcUser(u), f.polygonRID); err != nil {
						rt.Fatalf("model free but user %d save rejected: %v", u, err)
					}
				}
			} else {
				for _, u := range users {
					err := f.repo.GuardSave(ctx, rcUser(u), f.polygonRID)
					if u == holder {
						if err != nil {
							rt.Fatalf("holder %d save rejected: %v", u, err)
						}
						continue
					}
					if !isLockedBy(err, holder) {
						rt.Fatalf("exclusivity broken: user %d save while %d holds got %v, want ErrLocked{%d}", u, holder, err, holder)
					}
				}
			}
		}
	})
}
