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

// This file implements the Sphere half of the Sphere/Facet/Polygon service
// (design.md — "Sphere / Facet / Polygon Service", Requirements 6.1, 6.2, 6.3,
// 6.4, 6.5). A Sphere is a top-level workspace within a tenant; content is
// organized into Spheres, each identified by an immutable Record_ID.
//
// Two invariants live here and nowhere else:
//
//   - Name validation. A Sphere name is 1–100 characters and unique within the
//     tenant. Empty, over-length, or duplicate names are rejected with
//     ErrValidation and the tenant's Sphere set is left unchanged (Req 6.4).
//     The check runs inside the same transaction as the insert so a concurrent
//     creator cannot slip a duplicate past the read (the sphere.name uniqueness
//     is a service-layer rule, not a DB constraint — the schema only makes
//     record_id UNIQUE).
//
//   - Cascade delete. Deleting a Sphere removes every Facet and Polygon it
//     contains (Req 6.5). The tenant schema declares ON DELETE CASCADE from
//     sphere to facet/polygon (and onward), and tenant pools open with
//     foreign_keys = ON, so a single DELETE of the sphere row cascades. The
//     delete runs in one transaction so the whole cascade is atomic.
//
// Like every repository here, SphereRepo embeds Base and reaches the database
// only through the caller's own tenant handle, so all reads and writes are
// structurally scoped to rc.TenantID (Req 1.4, 1.5).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// ErrValidation is returned when an input fails a service-layer validation rule
// — for a Sphere, a name that is empty, longer than 100 characters, or a
// duplicate of an existing Sphere in the tenant (Req 6.4). It maps to the
// VALIDATION error envelope (design.md — "Validation (400)"). A rejected
// operation leaves the affected record set unchanged.
//
// It lives in the repo package so sibling repositories (Facet, Polygon) share a
// single validation sentinel rather than each defining their own.
var ErrValidation = errors.New("validation failed")

// sphereNameMaxLen is the inclusive upper bound on a Sphere name length in
// characters (runes). Names are 1..sphereNameMaxLen (Req 6.2, 6.4).
const sphereNameMaxLen = 100

// Sphere is a top-level workspace as returned to callers. It carries the
// external Record_ID (never the internal surrogate id), the display name, and
// the creation timestamp.
type Sphere struct {
	RecordID  recordid.ID
	Name      string
	CreatedAt string
}

// SphereRepo is the tenant-scoped repository for Spheres. It embeds Base so it
// shares the single tenant-routing gateway; it holds no state of its own.
type SphereRepo struct {
	Base
}

// NewSphereRepo constructs a SphereRepo over a ConnManager.
func NewSphereRepo(conns data.ConnManager) *SphereRepo {
	return &SphereRepo{Base: NewBase(conns)}
}

// Create validates and stores a new Sphere in the caller's tenant, returning
// the created Sphere with its freshly minted Record_ID.
//
// The name must be 1–100 characters and must not duplicate an existing Sphere
// name in the tenant; otherwise Create returns ErrValidation and leaves the
// tenant's Sphere set unchanged (Req 6.2, 6.4). The uniqueness check and the
// insert run inside one transaction so the rejection/insert decision is made
// against a consistent view.
func (r *SphereRepo) Create(ctx context.Context, rc data.RequestContext, name string) (Sphere, error) {
	// Length validation before touching the database — a rejected name must
	// change nothing (Req 6.4).
	if n := len([]rune(name)); n < 1 || n > sphereNameMaxLen {
		return Sphere{}, fmt.Errorf("%w: sphere name must be 1-%d characters", ErrValidation, sphereNameMaxLen)
	}

	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return Sphere{}, err
	}

	tx, err := tdb.DB().BeginTx(ctx, nil)
	if err != nil {
		return Sphere{}, fmt.Errorf("begin create sphere: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Uniqueness within the tenant (Req 6.4). The comparison is exact on the
	// stored name; the check is inside the transaction so it and the insert see
	// the same snapshot.
	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM sphere WHERE name = ?`, name).Scan(&existing)
	switch {
	case err == nil:
		return Sphere{}, fmt.Errorf("%w: a sphere named %q already exists", ErrValidation, name)
	case errors.Is(err, sql.ErrNoRows):
		// No duplicate — proceed.
	default:
		return Sphere{}, fmt.Errorf("check sphere name uniqueness: %w", err)
	}

	id := recordid.New()
	createdAt := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sphere (record_id, name, created_at) VALUES (?, ?, ?)`,
		id.Canonical(), name, createdAt,
	); err != nil {
		return Sphere{}, fmt.Errorf("insert sphere: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Sphere{}, fmt.Errorf("commit create sphere: %w", err)
	}

	return Sphere{RecordID: id, Name: name, CreatedAt: createdAt}, nil
}

// Get returns the Sphere identified by recordID within the caller's tenant. A
// Record_ID absent from this tenant — whether nonexistent or owned by another
// tenant — yields ErrNotAccessible (Req 1.6), indistinguishable by design.
func (r *SphereRepo) Get(ctx context.Context, rc data.RequestContext, recordID recordid.ID) (Sphere, error) {
	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return Sphere{}, err
	}

	var name, createdAt string
	err = tdb.DB().QueryRowContext(ctx,
		`SELECT name, created_at FROM sphere WHERE record_id = ?`, recordID.Canonical(),
	).Scan(&name, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Sphere{}, ErrNotAccessible
	}
	if err != nil {
		return Sphere{}, fmt.Errorf("get sphere: %w", err)
	}
	return Sphere{RecordID: recordID, Name: name, CreatedAt: createdAt}, nil
}

// List returns all Spheres in the caller's tenant ordered ascending by name
// using case-insensitive comparison, with the canonical Record_ID as a stable
// tie-break. (Per-user circling/ordering — Req 7 — is layered on in task 8.2;
// this is the plain tenant-wide listing.)
func (r *SphereRepo) List(ctx context.Context, rc data.RequestContext) ([]Sphere, error) {
	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return nil, err
	}

	rows, err := tdb.DB().QueryContext(ctx,
		`SELECT record_id, name, created_at FROM sphere ORDER BY name COLLATE NOCASE ASC, record_id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list spheres: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Sphere
	for rows.Next() {
		var canonical, name, createdAt string
		if err := rows.Scan(&canonical, &name, &createdAt); err != nil {
			return nil, fmt.Errorf("scan sphere: %w", err)
		}
		id, err := recordid.Parse(canonical)
		if err != nil {
			return nil, fmt.Errorf("parse sphere record id: %w", err)
		}
		out = append(out, Sphere{RecordID: id, Name: name, CreatedAt: createdAt})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spheres: %w", err)
	}
	return out, nil
}

// Delete removes the Sphere identified by recordID together with all Facets and
// Polygons it contains (Req 6.5). The cascade is performed by the schema's
// ON DELETE CASCADE rules (foreign keys are enabled on tenant pools); the whole
// operation runs in a single transaction so the cascade is atomic.
//
// A Record_ID absent from the caller's tenant yields ErrNotAccessible and
// changes nothing (Req 1.6).
func (r *SphereRepo) Delete(ctx context.Context, rc data.RequestContext, recordID recordid.ID) error {
	tdb, err := r.tenant(ctx, rc)
	if err != nil {
		return err
	}

	tx, err := tdb.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete sphere: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM sphere WHERE record_id = ?`, recordID.Canonical())
	if err != nil {
		return fmt.Errorf("delete sphere: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete sphere rows affected: %w", err)
	}
	if affected == 0 {
		// Absent from this tenant — nonexistent or owned by another tenant.
		// Nothing was deleted; report not accessible (Req 1.6).
		return ErrNotAccessible
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete sphere: %w", err)
	}
	return nil
}
