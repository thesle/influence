package repo

// This file implements per-user Sphere circling and ordering (design.md —
// "Sphere / Facet / Polygon Service" and the left-menu ordering rule,
// Requirements 7.1, 7.2, 7.3, 7.4, 7.5, 7.6). Circling is personalization: it
// is scoped to a single (user, tenant) pair and lives entirely in the tenant's
// USER_CIRCLE table, so an exported tenant carries each user's pinned/ordered
// Spheres with it.
//
// Four behaviors live here:
//
//   - Circle (Req 7.1, 7.2). Adding a Sphere to the user's Circled_Sphere list
//     is idempotent: circling a Sphere already circled leaves the list
//     unchanged. A newly circled Sphere is appended after the user's existing
//     circled entries (it takes the next position), preserving the order the
//     user has already established.
//
//   - UnCircle (Req 7.6). Removing a Sphere from the list returns it to the
//     alphabetical remainder — there is no separate "remainder" store; a Sphere
//     is in the remainder precisely because it has no USER_CIRCLE row, so the
//     DELETE is all that is required. Un-circling a Sphere that was not circled
//     is a no-op.
//
//   - Reorder (Req 7.4). Persisting a user-specified order rewrites the
//     positions of the user's circled Spheres to match the given Record_ID
//     sequence. The sequence must be exactly the user's current circled set (a
//     permutation) — anything else is a validation error and the stored order
//     is left unchanged.
//
//   - ListOrdered (Req 7.3, 7.5). The display listing returns the user's
//     circled Spheres in their persisted position order ABOVE the non-circled
//     remainder, which is sorted ascending by name using case-insensitive
//     comparison with the canonical Record_ID as a stable tie-break.
//
// Everything is tenant-scoped through Base.tenant and keyed by the caller's
// UserID, so one user's circling never affects another's and never crosses a
// tenant boundary (Req 1.4, 1.5).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// CircleRepo is the tenant-scoped repository for per-user Sphere circling and
// ordering. It embeds Base so it shares the single tenant-routing gateway and
// the Record_ID resolution path; it holds no state of its own.
type CircleRepo struct {
	Base
}

// NewCircleRepo constructs a CircleRepo over a ConnManager.
func NewCircleRepo(conns data.ConnManager) *CircleRepo {
	return &CircleRepo{Base: NewBase(conns)}
}

// Circle adds the identified Sphere to the caller's Circled_Sphere list within
// the caller's tenant (Req 7.1). It is idempotent: if the Sphere is already
// circled by this user the list is left unchanged (Req 7.2). A newly circled
// Sphere is appended after the user's existing circled entries.
//
// A Record_ID absent from the caller's tenant yields ErrNotAccessible and
// changes nothing (Req 1.6).
func (r *CircleRepo) Circle(ctx context.Context, rc data.RequestContext, sphereRecordID recordid.ID) error {
	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	tx, err := tdb.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin circle: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	sphereID, err := r.resolveSphereTx(ctx, tx, sphereRecordID)
	if err != nil {
		return err
	}

	userID := int64(rc.UserID())

	// Idempotence (Req 7.2): if a row already exists for (user, sphere), leave
	// the list untouched and return without change.
	var existing int64
	err = tx.QueryRowContext(ctx,
		`SELECT position FROM user_circle WHERE user_id = ? AND sphere_id = ?`,
		userID, sphereID,
	).Scan(&existing)
	switch {
	case err == nil:
		// Already circled — no change, commit the (empty) transaction.
		return tx.Commit()
	case errors.Is(err, sql.ErrNoRows):
		// Not circled yet — fall through to append.
	default:
		return fmt.Errorf("check existing circle: %w", err)
	}

	// Append after the user's existing circled entries. COALESCE handles the
	// empty-list case (no rows -> start at position 0).
	var nextPos int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position)+1, 0) FROM user_circle WHERE user_id = ?`,
		userID,
	).Scan(&nextPos); err != nil {
		return fmt.Errorf("compute next circle position: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_circle (user_id, sphere_id, position) VALUES (?, ?, ?)`,
		userID, sphereID, nextPos,
	); err != nil {
		return fmt.Errorf("insert circle: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit circle: %w", err)
	}
	return nil
}

// UnCircle removes the identified Sphere from the caller's Circled_Sphere list,
// returning it to the caller's alphabetical remainder (Req 7.6). Because a
// Sphere is in the remainder precisely when it has no USER_CIRCLE row, deleting
// the row is the whole operation. Un-circling a Sphere that was not circled is a
// no-op (the list is already in the desired state).
//
// A Record_ID absent from the caller's tenant yields ErrNotAccessible and
// changes nothing (Req 1.6).
func (r *CircleRepo) UnCircle(ctx context.Context, rc data.RequestContext, sphereRecordID recordid.ID) error {
	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	tx, err := tdb.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin uncircle: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	sphereID, err := r.resolveSphereTx(ctx, tx, sphereRecordID)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM user_circle WHERE user_id = ? AND sphere_id = ?`,
		int64(rc.UserID()), sphereID,
	); err != nil {
		return fmt.Errorf("delete circle: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit uncircle: %w", err)
	}
	return nil
}

// Reorder persists a user-specified order for the caller's circled Spheres
// (Req 7.4). orderedSphereRecordIDs must be exactly the caller's current
// circled set, each Record_ID appearing once — that is, a permutation of the
// currently circled Spheres. Any missing entry, extra entry, duplicate, or
// unknown/not-circled Record_ID is a validation error (ErrValidation) and the
// stored order is left unchanged.
//
// On success the position of each circled Sphere is set to its index in
// orderedSphereRecordIDs, so a subsequent ListOrdered returns the circled group
// in exactly this order.
func (r *CircleRepo) Reorder(ctx context.Context, rc data.RequestContext, orderedSphereRecordIDs []recordid.ID) error {
	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	tx, err := tdb.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reorder: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	userID := int64(rc.UserID())

	// The set of Spheres currently circled by this user (surrogate id -> true).
	currentlyCircled := map[int64]bool{}
	rows, err := tx.QueryContext(ctx,
		`SELECT sphere_id FROM user_circle WHERE user_id = ?`, userID,
	)
	if err != nil {
		return fmt.Errorf("load current circle set: %w", err)
	}
	for rows.Next() {
		var sid int64
		if err := rows.Scan(&sid); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan circle set: %w", err)
		}
		currentlyCircled[sid] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate circle set: %w", err)
	}
	_ = rows.Close()

	// The requested order must be exactly a permutation of the current set:
	// same length, no duplicates, every entry currently circled.
	if len(orderedSphereRecordIDs) != len(currentlyCircled) {
		return fmt.Errorf("%w: reorder must list exactly the currently circled spheres", ErrValidation)
	}

	seen := map[int64]bool{}
	surrogates := make([]int64, len(orderedSphereRecordIDs))
	for i, rid := range orderedSphereRecordIDs {
		sid, err := r.resolveSphereTx(ctx, tx, rid)
		if err != nil {
			// A Record_ID not in this tenant surfaces as ErrNotAccessible; any
			// other resolution error propagates as-is.
			return err
		}
		if !currentlyCircled[sid] {
			return fmt.Errorf("%w: sphere %s is not in the circled list", ErrValidation, rid)
		}
		if seen[sid] {
			return fmt.Errorf("%w: sphere %s listed more than once", ErrValidation, rid)
		}
		seen[sid] = true
		surrogates[i] = sid
	}

	// Rewrite positions to match the requested order.
	for pos, sid := range surrogates {
		if _, err := tx.ExecContext(ctx,
			`UPDATE user_circle SET position = ? WHERE user_id = ? AND sphere_id = ?`,
			int64(pos), userID, sid,
		); err != nil {
			return fmt.Errorf("update circle position: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reorder: %w", err)
	}
	return nil
}

// ListOrdered returns the caller's accessible Spheres in display order: the
// caller's circled Spheres in their persisted position order (Req 7.3, 7.4)
// followed by the non-circled remainder sorted ascending by name using
// case-insensitive comparison with the canonical Record_ID as a stable
// tie-break (Req 7.5).
//
// The listing is tenant-wide (every Sphere in the caller's tenant); per-Sphere
// access filtering (Req 4/21) is applied by the caller/service layer that holds
// the RequestContext grants. Circling is a pure ordering concern here.
func (r *CircleRepo) ListOrdered(ctx context.Context, rc data.RequestContext) ([]Sphere, error) {
	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return nil, err
	}

	// A single query returns every Sphere with its circle position (NULL when
	// not circled by this user). Ordering rule:
	//   1. circled rows first (position IS NOT NULL) ordered by position;
	//   2. then the remainder by name COLLATE NOCASE, record_id.
	// The CASE expression sorts non-circled rows (position IS NULL) after
	// circled ones deterministically.
	rows, err := tdb.DB().QueryContext(ctx,
		`SELECT s.record_id, s.name, s.created_at, uc.position AS position
		   FROM sphere s
		   LEFT JOIN user_circle uc
		     ON uc.sphere_id = s.id AND uc.user_id = ?
		  ORDER BY
		     CASE WHEN uc.position IS NULL THEN 1 ELSE 0 END ASC,
		     uc.position ASC,
		     s.name COLLATE NOCASE ASC,
		     s.record_id ASC`,
		int64(rc.UserID()),
	)
	if err != nil {
		return nil, fmt.Errorf("list ordered spheres: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Sphere
	for rows.Next() {
		var canonical, name, createdAt string
		var position sql.NullInt64
		if err := rows.Scan(&canonical, &name, &createdAt, &position); err != nil {
			return nil, fmt.Errorf("scan ordered sphere: %w", err)
		}
		id, err := recordid.Parse(canonical)
		if err != nil {
			return nil, fmt.Errorf("parse sphere record id: %w", err)
		}
		out = append(out, Sphere{RecordID: id, Name: name, CreatedAt: createdAt})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ordered spheres: %w", err)
	}
	return out, nil
}

// resolveSphereTx resolves a Sphere Record_ID to its surrogate id within the
// given transaction, returning ErrNotAccessible when the Record_ID is absent
// from the caller's tenant (Req 1.6). Running the lookup inside the same
// transaction as the mutation keeps the read and write on one consistent
// snapshot.
func (r *CircleRepo) resolveSphereTx(ctx context.Context, tx *sql.Tx, sphereRecordID recordid.ID) (int64, error) {
	var sphereID int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM sphere WHERE record_id = ?`, sphereRecordID.Canonical(),
	).Scan(&sphereID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotAccessible
	}
	if err != nil {
		return 0, fmt.Errorf("resolve sphere: %w", err)
	}
	return sphereID, nil
}
