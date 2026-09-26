package repo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// These tests exercise record-locking mode (Req 25.3, 25.4, 25.5) over the
// production tenant-routing fixture, so every lock operation runs through the
// same tenant-scoped path as the rest of the repository. Time is pinned via a
// controllable clock so the 300s inactivity window is deterministic.

// editLockFixture pairs the two-tenant fixture with an EditLockRepo bound to a
// controllable clock, plus a record-locking Polygon seeded in tenant A.
type editLockFixture struct {
	twoTenantFixture
	repo       *EditLockRepo
	now        time.Time
	polygonRID string
}

// setNow moves the fixture's injected clock. Lock operations read it through
// the closure installed below, so tests advance simulated time freely.
func (f *editLockFixture) setNow(tm time.Time) { f.now = tm }

func newEditLockFixture(t *testing.T) *editLockFixture {
	t.Helper()
	base := newTwoTenantFixture(t)

	f := &editLockFixture{
		twoTenantFixture: base,
		now:              time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
	}
	f.repo = (&EditLockRepo{Base: base.base}).withClock(func() time.Time { return f.now })

	// Seed a Sphere + Polygon in tenant A to lock against.
	sphereRID := recordid.New()
	sphereSurrogate := seedSphere(t, f.dbA, sphereRID, "Sphere One")
	polygonRID := recordid.New()
	if _, err := f.dbA.Exec(
		`INSERT INTO polygon (record_id, sphere_id, type, author_user_id, created_at, updated_at)
		 VALUES (?, ?, 'markdown', 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
		polygonRID.Canonical(), sphereSurrogate,
	); err != nil {
		t.Fatalf("seed polygon: %v", err)
	}
	f.polygonRID = polygonRID.Canonical()
	return f
}

// rcUser builds a RequestContext for the given user id in tenant A (tenant id 1
// per the two-tenant fixture).
func rcUser(userID int64) data.RequestContext {
	return data.NewRequestContext(data.UserID(userID), 1, nil, nil)
}

// TestAcquireLockWhenFree confirms a user can take a free lock and becomes the
// holder (Req 25.3).
func TestAcquireLockWhenFree(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	lock, err := f.repo.AcquireLock(ctx, rcUser(10), f.polygonRID)
	if err != nil {
		t.Fatalf("acquire free lock: %v", err)
	}
	if lock.HolderUserID != 10 {
		t.Errorf("holder = %d, want 10", lock.HolderUserID)
	}

	// A second user's save is now blocked by the active holder.
	if err := f.repo.GuardSave(ctx, rcUser(20), f.polygonRID); !isLockedBy(err, 10) {
		t.Errorf("other user save: got %v, want ErrLocked{10}", err)
	}
	// The holder may save.
	if err := f.repo.GuardSave(ctx, rcUser(10), f.polygonRID); err != nil {
		t.Errorf("holder save: %v", err)
	}
}

// TestSameUserReacquireRefreshes confirms re-acquiring a lock the caller
// already holds is idempotent: acquired_at is preserved while last_activity_at
// slides forward (Req 25.3).
func TestSameUserReacquireRefreshes(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	first, err := f.repo.AcquireLock(ctx, rcUser(10), f.polygonRID)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	f.setNow(f.now.Add(60 * time.Second))
	second, err := f.repo.AcquireLock(ctx, rcUser(10), f.polygonRID)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}

	if second.AcquiredAt != first.AcquiredAt {
		t.Errorf("acquired_at changed on re-acquire: %q -> %q", first.AcquiredAt, second.AcquiredAt)
	}
	if second.LastActivityAt == first.LastActivityAt {
		t.Errorf("last_activity_at did not advance on re-acquire: still %q", second.LastActivityAt)
	}
}

// TestAcquireRejectsOtherActiveHolder confirms a second user cannot take a lock
// held by an active holder, and the rejection carries the holder's identity
// (Req 25.4).
func TestAcquireRejectsOtherActiveHolder(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	if _, err := f.repo.AcquireLock(ctx, rcUser(10), f.polygonRID); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	// Within the active window, user 20 is rejected with holder 10's identity.
	f.setNow(f.now.Add(120 * time.Second))
	_, err := f.repo.AcquireLock(ctx, rcUser(20), f.polygonRID)
	if !isLockedBy(err, 10) {
		t.Fatalf("other user acquire: got %v, want ErrLocked{10}", err)
	}

	// The lock is untouched — user 10 still holds it.
	if err := f.repo.GuardSave(ctx, rcUser(10), f.polygonRID); err != nil {
		t.Errorf("holder still blocked after rejected takeover: %v", err)
	}
}

// TestStaleLockReclaimable confirms a lock unrefreshed past the 300s window is
// reclaimable by another user (Req 25.5). Before the window elapses the lock
// still blocks; after it, the new user takes over.
func TestStaleLockReclaimable(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	if _, err := f.repo.AcquireLock(ctx, rcUser(10), f.polygonRID); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	// Exactly at 300s the lock is not yet stale (strictly greater than window).
	f.setNow(f.now.Add(lockInactivityTimeout))
	if _, err := f.repo.AcquireLock(ctx, rcUser(20), f.polygonRID); !isLockedBy(err, 10) {
		t.Fatalf("at boundary: got %v, want ErrLocked{10}", err)
	}

	// Just past 300s the lock is stale and user 20 reclaims it.
	f.setNow(f.now.Add(lockInactivityTimeout + time.Second))
	lock, err := f.repo.AcquireLock(ctx, rcUser(20), f.polygonRID)
	if err != nil {
		t.Fatalf("reclaim stale lock: %v", err)
	}
	if lock.HolderUserID != 20 {
		t.Errorf("holder after reclaim = %d, want 20", lock.HolderUserID)
	}
	// Now user 10's save is the one that is blocked.
	if err := f.repo.GuardSave(ctx, rcUser(10), f.polygonRID); !isLockedBy(err, 20) {
		t.Errorf("prior holder save after reclaim: got %v, want ErrLocked{20}", err)
	}
}

// TestExplicitReleaseFreesLock confirms the holder can explicitly release the
// lock, after which another user may acquire it (Req 25.5).
func TestExplicitReleaseFreesLock(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	if _, err := f.repo.AcquireLock(ctx, rcUser(10), f.polygonRID); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	if err := f.repo.ReleaseLock(ctx, rcUser(10), f.polygonRID); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Freed: user 20 acquires it immediately, within the active window.
	f.setNow(f.now.Add(10 * time.Second))
	lock, err := f.repo.AcquireLock(ctx, rcUser(20), f.polygonRID)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if lock.HolderUserID != 20 {
		t.Errorf("holder after release = %d, want 20", lock.HolderUserID)
	}
}

// TestReleaseByNonHolderRejected confirms a user cannot release another user's
// lock; the attempt is rejected naming the holder and the lock stays put.
func TestReleaseByNonHolderRejected(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	if _, err := f.repo.AcquireLock(ctx, rcUser(10), f.polygonRID); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	if err := f.repo.ReleaseLock(ctx, rcUser(20), f.polygonRID); !isLockedBy(err, 10) {
		t.Fatalf("non-holder release: got %v, want ErrLocked{10}", err)
	}
	// Lock intact.
	if err := f.repo.GuardSave(ctx, rcUser(20), f.polygonRID); !isLockedBy(err, 10) {
		t.Errorf("lock freed by non-holder release: got %v", err)
	}
}

// TestHeartbeatExtendsLock confirms RefreshActivity slides last_activity_at so
// a lock stays active past what would otherwise be the staleness window
// (Req 25.5).
func TestHeartbeatExtendsLock(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	if _, err := f.repo.AcquireLock(ctx, rcUser(10), f.polygonRID); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	acquireTime := f.now

	// Heartbeat at 200s past acquire keeps the holder active.
	f.setNow(acquireTime.Add(200 * time.Second))
	if err := f.repo.RefreshActivity(ctx, rcUser(10), f.polygonRID); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// At 450s past the original acquire — only 250s since the heartbeat, inside
	// the 300s window — the lock is still active, so another user is blocked.
	// Without the heartbeat this point would be well past 300s and stale.
	f.setNow(acquireTime.Add(450 * time.Second))
	if _, err := f.repo.AcquireLock(ctx, rcUser(20), f.polygonRID); !isLockedBy(err, 10) {
		t.Errorf("lock went stale despite heartbeat: got %v, want ErrLocked{10}", err)
	}
}

// TestHeartbeatByNonHolderRejected confirms only the holder may refresh; a
// non-holder refresh is rejected naming the holder and does not extend the
// lock.
func TestHeartbeatByNonHolderRejected(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	if _, err := f.repo.AcquireLock(ctx, rcUser(10), f.polygonRID); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	if err := f.repo.RefreshActivity(ctx, rcUser(20), f.polygonRID); !isLockedBy(err, 10) {
		t.Errorf("non-holder refresh: got %v, want ErrLocked{10}", err)
	}
}

// TestRefreshAndReleaseOnFreeLock confirms operations that require a held lock
// report ErrNoLock on a free Polygon.
func TestRefreshAndReleaseOnFreeLock(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	if err := f.repo.RefreshActivity(ctx, rcUser(10), f.polygonRID); !errors.Is(err, ErrNoLock) {
		t.Errorf("refresh free: got %v, want ErrNoLock", err)
	}
	if err := f.repo.ReleaseLock(ctx, rcUser(10), f.polygonRID); !errors.Is(err, ErrNoLock) {
		t.Errorf("release free: got %v, want ErrNoLock", err)
	}
}

// TestReleaseOnDisconnectFreesHolderLock confirms the ws-layer disconnect entry
// frees the holder's lock, and a disconnect of a non-holder frees nothing
// (Req 25.5).
func TestReleaseOnDisconnectFreesHolderLock(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	if _, err := f.repo.AcquireLock(ctx, rcUser(10), f.polygonRID); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	// A disconnect by a non-holder does not free the lock.
	if err := f.repo.ReleaseOnDisconnect(ctx, rcUser(10), f.polygonRID, 20); err != nil {
		t.Fatalf("disconnect non-holder: %v", err)
	}
	if err := f.repo.GuardSave(ctx, rcUser(20), f.polygonRID); !isLockedBy(err, 10) {
		t.Fatalf("lock freed by non-holder disconnect: got %v", err)
	}

	// The holder's disconnect frees it; user 20 can then take it.
	if err := f.repo.ReleaseOnDisconnect(ctx, rcUser(10), f.polygonRID, 10); err != nil {
		t.Fatalf("disconnect holder: %v", err)
	}
	if _, err := f.repo.AcquireLock(ctx, rcUser(20), f.polygonRID); err != nil {
		t.Errorf("acquire after holder disconnect: %v", err)
	}
}

// TestLockCrossTenantNotAccessible confirms lock operations are tenant-scoped:
// a Polygon in tenant A is not lockable from tenant B (Req 1.6).
func TestLockCrossTenantNotAccessible(t *testing.T) {
	f := newEditLockFixture(t)
	ctx := context.Background()

	// rcB is user 20 in tenant 2 per the two-tenant fixture.
	if _, err := f.repo.AcquireLock(ctx, f.rcB, f.polygonRID); !errors.Is(err, ErrNotAccessible) {
		t.Errorf("cross-tenant acquire: got %v, want ErrNotAccessible", err)
	}
	if err := f.repo.GuardSave(ctx, f.rcB, f.polygonRID); !errors.Is(err, ErrNotAccessible) {
		t.Errorf("cross-tenant guard: got %v, want ErrNotAccessible", err)
	}
}

// isLockedBy reports whether err is an ErrLocked naming the given holder.
func isLockedBy(err error, holder int64) bool {
	var locked ErrLocked
	return errors.As(err, &locked) && locked.HolderUserID == holder
}
