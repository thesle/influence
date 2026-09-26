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

package repo

// This file implements record-locking mode for Polygons (design.md —
// "Record-locking mode"; Requirements 25.3, 25.4, 25.5). A record-locking
// Polygon is edited by at most one user at a time: that user holds the single
// EDIT_LOCK row for the Polygon and every other user's save is refused until
// the lock is released.
//
// The lock lifecycle, mirroring the design's state machine:
//
//   - Acquire (Req 25.3). When a user begins editing, AcquireLock takes the
//     EDIT_LOCK for that Polygon if it is free. If the same user already holds
//     it, the acquire is idempotent and simply slides last_activity_at. If a
//     DIFFERENT user holds it and the lock is still active (within the 300s
//     inactivity window), the acquire is rejected with ErrLocked carrying the
//     holder's identity (Req 25.4). If the existing lock is stale — more than
//     300s since its last_activity_at — it is reclaimable: the new user takes
//     it over.
//
//   - Guard a save (Req 25.4). GuardSave answers "may this user save?" without
//     changing the lock. A save by anyone other than the active holder is
//     rejected with ErrLocked naming the holder; the holder is permitted.
//
//   - Heartbeat (Req 25.5). RefreshActivity slides last_activity_at forward
//     while the holder is active, keeping the lock from going stale. Only the
//     holder may refresh.
//
//   - Release (Req 25.5). A lock releases on explicit ReleaseLock by the
//     holder, after 300s of holder inactivity (a stale lock is treated as free
//     and is reclaimable on the next acquire), or on holder disconnect —
//     ReleaseOnDisconnect is the entry point the WebSocket layer calls when a
//     connection closes.
//
// Like every repository here, EditLockRepo embeds Base and reaches the database
// only through the caller's own tenant handle, so all reads and writes are
// structurally scoped to rc.TenantID (Req 1.4, 1.5). A Polygon Record_ID that
// belongs to another tenant is simply absent here and resolves to
// ErrNotAccessible (Req 1.6). The EDIT_LOCK row is keyed by the Polygon's
// surrogate id, so lock state lives beside the Polygon it guards.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// lockInactivityTimeout is the window of holder inactivity after which an
// EDIT_LOCK is considered stale and reclaimable (Req 25.5 — 300 seconds).
// last_activity_at is refreshed by RefreshActivity while the holder edits; once
// now is more than this past last_activity_at the lock is treated as free.
const lockInactivityTimeout = 300 * time.Second

// ErrLocked is returned when a user other than the active lock holder attempts
// to acquire or save a record-locking Polygon (Req 25.4). It carries the
// holder's identity so the caller can tell the rejected user that the Polygon
// is locked and by whom. Handlers map it to the LOCKED (409) envelope
// (design.md — "Concurrency (409 LOCKED)").
type ErrLocked struct {
	// HolderUserID is the Central_Directory user id currently holding the lock.
	HolderUserID int64
}

// Error renders the lock rejection, naming the holder so the message is
// actionable (Req 25.4 — "locked and by whom").
func (e ErrLocked) Error() string {
	return fmt.Sprintf("polygon is locked by user %d", e.HolderUserID)
}

// Lock is the observable state of an EDIT_LOCK: who holds it and the timestamps
// that drive the inactivity timeout. It is returned by AcquireLock so a caller
// learns the holder (itself) and when the lock was taken.
type Lock struct {
	// HolderUserID is the Central_Directory user id holding the lock.
	HolderUserID int64
	// AcquiredAt is when the lock was first taken by the current holder, as an
	// RFC3339 UTC timestamp.
	AcquiredAt string
	// LastActivityAt is the most recent holder activity, as an RFC3339 UTC
	// timestamp. It advances on acquire and on each RefreshActivity and drives
	// the 300s staleness check (Req 25.5).
	LastActivityAt string
}

// Clock returns the current time. It is an injection point so tests can drive
// the 300s inactivity window deterministically (mirroring the auth service's
// clock). The zero value of EditLockRepo uses the real clock.
type Clock func() time.Time

// EditLockRepo is the tenant-scoped record-locking repository. It embeds Base
// so every query is routed to the caller's own tenant DB and nowhere else, and
// carries a clock injection point so the inactivity window is deterministic in
// tests (mirroring AuthService).
type EditLockRepo struct {
	Base
	now Clock
}

// NewEditLockRepo constructs an EditLockRepo over a ConnManager, using the real
// wall clock. Tests use withClock to pin time.
func NewEditLockRepo(conns data.ConnManager) *EditLockRepo {
	return &EditLockRepo{Base: NewBase(conns), now: time.Now}
}

// withClock overrides the service clock. Used by tests to control the
// inactivity window deterministically; not part of the public surface.
func (r *EditLockRepo) withClock(c Clock) *EditLockRepo {
	r.now = c
	return r
}

// clock returns the effective clock, defaulting to time.Now when unset, always
// in UTC so stored timestamps are comparable.
func (r *EditLockRepo) clock() time.Time {
	if r.now == nil {
		return time.Now().UTC()
	}
	return r.now().UTC()
}

// resolvePolygon turns a Polygon Record_ID into its surrogate id within the
// caller's tenant, collapsing an unparseable id into ErrNotAccessible so a
// parse failure is indistinguishable from a missing/other-tenant record
// (Req 1.6).
func (r *EditLockRepo) resolvePolygon(ctx context.Context, rc data.RequestContext, polygonRecordID string) (int64, error) {
	pid, err := recordid.Parse(polygonRecordID)
	if err != nil {
		return 0, ErrNotAccessible
	}
	return r.resolveRecord(ctx, rc, RecordPolygon, pid)
}

// AcquireLock takes the EDIT_LOCK for the Polygon identified by
// polygonRecordID on behalf of the caller (rc.UserID), which is what happens
// when that user begins editing a record-locking Polygon (Req 25.3).
//
// Outcomes:
//   - Free (no row, or a stale row past the 300s inactivity window): the caller
//     takes the lock. A stale prior holder's row is overwritten — the lock is
//     reclaimable after 300s of inactivity (Req 25.5).
//   - Already held by the caller: idempotent refresh — acquired_at is kept and
//     last_activity_at slides to now.
//   - Held by another user and still active: rejected with ErrLocked naming the
//     holder (Req 25.4); the existing lock is left untouched.
//
// The read-decide-write runs in one transaction so two concurrent acquirers
// cannot both believe they took a free lock.
func (r *EditLockRepo) AcquireLock(ctx context.Context, rc data.RequestContext, polygonRecordID string) (Lock, error) {
	polygonID, err := r.resolvePolygon(ctx, rc, polygonRecordID)
	if err != nil {
		return Lock{}, err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return Lock{}, err
	}

	me := int64(rc.UserID())
	now := r.clock()

	tx, err := tdb.DB().BeginTx(ctx, nil)
	if err != nil {
		return Lock{}, fmt.Errorf("begin acquire lock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	holder, acquiredAt, lastActivity, held, err := lockRowTx(ctx, tx, polygonID)
	if err != nil {
		return Lock{}, err
	}

	nowStr := now.Format(time.RFC3339)

	if held && holder != me && !lockStale(lastActivity, now) {
		// A different, still-active holder owns the lock. Reject naming the
		// holder (Req 25.4) and leave the lock exactly as it was.
		return Lock{}, ErrLocked{HolderUserID: holder}
	}

	// Preserve acquired_at on a same-user refresh; otherwise (free or reclaimed
	// stale lock) start a fresh acquisition at now.
	newAcquiredAt := nowStr
	if held && holder == me {
		newAcquiredAt = acquiredAt
	}

	// Upsert on the polygon_id primary key: insert when free, overwrite when
	// refreshing our own lock or reclaiming a stale one.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO edit_lock (polygon_id, holder_user_id, acquired_at, last_activity_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(polygon_id) DO UPDATE SET
		     holder_user_id = excluded.holder_user_id,
		     acquired_at = excluded.acquired_at,
		     last_activity_at = excluded.last_activity_at`,
		polygonID, me, newAcquiredAt, nowStr,
	); err != nil {
		return Lock{}, fmt.Errorf("acquire lock: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Lock{}, fmt.Errorf("commit acquire lock: %w", err)
	}

	return Lock{
		HolderUserID:   me,
		AcquiredAt:     newAcquiredAt,
		LastActivityAt: nowStr,
	}, nil
}

// GuardSave reports whether the caller (rc.UserID) may save the Polygon
// identified by polygonRecordID under record-locking rules, without changing
// any lock state (Req 25.4).
//
// A save is permitted when the Polygon is free, when its lock is stale (past
// the 300s window, so effectively free — Req 25.5), or when the caller is the
// active holder. Any other user's save while an active lock is held is rejected
// with ErrLocked naming the holder.
func (r *EditLockRepo) GuardSave(ctx context.Context, rc data.RequestContext, polygonRecordID string) error {
	polygonID, err := r.resolvePolygon(ctx, rc, polygonRecordID)
	if err != nil {
		return err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	holder, _, lastActivity, held, err := lockRow(ctx, tdb.DB(), polygonID)
	if err != nil {
		return err
	}
	if !held {
		return nil
	}

	now := r.clock()
	if lockStale(lastActivity, now) {
		// The holder went inactive past the window; the lock is reclaimable so
		// it does not block a save (Req 25.5).
		return nil
	}
	if holder == int64(rc.UserID()) {
		return nil
	}
	return ErrLocked{HolderUserID: holder}
}

// RefreshActivity slides last_activity_at to now for the Polygon's lock while
// the caller holds it, keeping an actively-edited lock from going stale
// (Req 25.5). Only the holder may refresh: a refresh by a non-holder (or when
// no active lock is held) is rejected with ErrLocked naming the current holder,
// or is a no-op error when free.
//
// It never takes over a lock — that is AcquireLock's job. A stale lock is not
// refreshed here; the holder must re-acquire it.
func (r *EditLockRepo) RefreshActivity(ctx context.Context, rc data.RequestContext, polygonRecordID string) error {
	polygonID, err := r.resolvePolygon(ctx, rc, polygonRecordID)
	if err != nil {
		return err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	me := int64(rc.UserID())
	now := r.clock()

	tx, err := tdb.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin refresh activity: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	holder, _, lastActivity, held, err := lockRowTx(ctx, tx, polygonID)
	if err != nil {
		return err
	}
	if !held {
		return ErrNoLock
	}
	if holder != me || lockStale(lastActivity, now) {
		// Not the caller's active lock: either someone else holds it or the
		// caller's lock has already gone stale and must be re-acquired.
		return ErrLocked{HolderUserID: holder}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE edit_lock SET last_activity_at = ? WHERE polygon_id = ?`,
		now.Format(time.RFC3339), polygonID,
	); err != nil {
		return fmt.Errorf("refresh activity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit refresh activity: %w", err)
	}
	return nil
}

// ReleaseLock releases the caller's explicit hold on the Polygon's lock
// (Req 25.5). Only the holder may release; a release attempted by a non-holder
// is rejected with ErrLocked naming the holder and leaves the lock intact so a
// user cannot free another user's lock. Releasing a free Polygon returns
// ErrNoLock.
func (r *EditLockRepo) ReleaseLock(ctx context.Context, rc data.RequestContext, polygonRecordID string) error {
	polygonID, err := r.resolvePolygon(ctx, rc, polygonRecordID)
	if err != nil {
		return err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	me := int64(rc.UserID())

	tx, err := tdb.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin release lock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	holder, _, _, held, err := lockRowTx(ctx, tx, polygonID)
	if err != nil {
		return err
	}
	if !held {
		return ErrNoLock
	}
	if holder != me {
		// A user may only release its own lock — never another user's.
		return ErrLocked{HolderUserID: holder}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM edit_lock WHERE polygon_id = ?`, polygonID,
	); err != nil {
		return fmt.Errorf("release lock: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit release lock: %w", err)
	}
	return nil
}

// ReleaseOnDisconnect frees the lock held by holderUserID on the Polygon when
// that user's WebSocket connection closes (Req 25.5 — release on holder
// disconnect). It is the entry point the ws layer calls on a disconnect, where
// the holder's identity is known from the connection rather than a request
// context; the tenant is still resolved from rc so the operation stays
// tenant-scoped.
//
// It deletes the lock only if holderUserID actually holds it, so a disconnect
// of a user who was not the holder (e.g. a viewer) frees nothing. A disconnect
// with no lock held is a no-op and returns nil — a closing connection should
// never surface a "no lock" error.
func (r *EditLockRepo) ReleaseOnDisconnect(ctx context.Context, rc data.RequestContext, polygonRecordID string, holderUserID int64) error {
	polygonID, err := r.resolvePolygon(ctx, rc, polygonRecordID)
	if err != nil {
		return err
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	// Delete only the disconnecting user's own lock. Scoping the DELETE by
	// holder_user_id means a disconnect never frees a lock held by someone
	// else, and a no-match (free, or held by another) simply affects no rows.
	if _, err := tdb.DB().ExecContext(ctx,
		`DELETE FROM edit_lock WHERE polygon_id = ? AND holder_user_id = ?`,
		polygonID, holderUserID,
	); err != nil {
		return fmt.Errorf("release lock on disconnect: %w", err)
	}
	return nil
}

// ErrNoLock is returned when an operation that requires an existing lock (a
// refresh or an explicit release) is attempted on a Polygon that is not
// currently locked.
var ErrNoLock = errors.New("polygon is not locked")

// lockStale reports whether a lock whose last_activity_at is lastActivity (an
// RFC3339 timestamp) has passed the 300s inactivity window as of now and is
// therefore reclaimable (Req 25.5). An unparseable timestamp is treated as
// stale so a corrupt row never wedges a Polygon permanently.
func lockStale(lastActivity string, now time.Time) bool {
	ts, err := time.Parse(time.RFC3339, lastActivity)
	if err != nil {
		return true
	}
	return now.Sub(ts) > lockInactivityTimeout
}

// lockRow reads the EDIT_LOCK row for a Polygon surrogate id, reporting whether
// a lock is held and, if so, the holder and timestamps. A missing row means the
// Polygon is free (held == false).
func lockRow(ctx context.Context, db *sql.DB, polygonID int64) (holder int64, acquiredAt, lastActivity string, held bool, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT holder_user_id, acquired_at, last_activity_at FROM edit_lock WHERE polygon_id = ?`,
		polygonID,
	).Scan(&holder, &acquiredAt, &lastActivity)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", "", false, nil
	}
	if err != nil {
		return 0, "", "", false, fmt.Errorf("read edit lock: %w", err)
	}
	return holder, acquiredAt, lastActivity, true, nil
}

// lockRowTx is lockRow inside a transaction, for the read-decide-write paths
// (acquire, refresh, release) that must see a consistent snapshot.
func lockRowTx(ctx context.Context, tx *sql.Tx, polygonID int64) (holder int64, acquiredAt, lastActivity string, held bool, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT holder_user_id, acquired_at, last_activity_at FROM edit_lock WHERE polygon_id = ?`,
		polygonID,
	).Scan(&holder, &acquiredAt, &lastActivity)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", "", false, nil
	}
	if err != nil {
		return 0, "", "", false, fmt.Errorf("read edit lock: %w", err)
	}
	return holder, acquiredAt, lastActivity, true, nil
}
