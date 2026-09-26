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

// This file implements the Polygon search-index component: it keeps the
// per-tenant FTS5 virtual table `polygon_fts` in sync with Polygon content so
// content search (Req 23) can find Polygons by their text.
//
// The single invariant this component protects is non-leakage of sensitive
// data through the search index (design.md — "Masking at every egress",
// "Search"; Requirements 16.7, 23.2):
//
//   - Stored POLYGON.content may contain Obfuscation_Tokens `|encrypt|{id}|`
//     standing in for encrypted spans; the plaintext lives only as ciphertext
//     in the CIPHERTEXT table. The index is populated with MASKED content —
//     content routed through the masking seam with reveal disabled — so every
//     token becomes a placeholder and NEITHER the plaintext NOR the ciphertext
//     ever enters `polygon_fts`. A non-reveal search therefore cannot match or
//     surface sensitive plaintext, because it simply is not in the index.
//
// Because `polygon_fts` is a virtual table with no foreign key to `polygon`,
// its rows do NOT cascade when a Polygon (or its owning Facet/Sphere) is
// deleted. Keeping the index in sync is this component's job: IndexPolygon
// upserts the masked row on Polygon create/edit, and RemoveFromIndex deletes it
// on Polygon delete, so the index never retains content for a Polygon that no
// longer exists.
//
// Like every repository here, PolygonIndex embeds Base and reaches the database
// only through the caller's own tenant handle, so all reads and writes are
// structurally scoped to rc.TenantID (Req 1.4, 1.5).

import (
	"context"
	"fmt"

	"github.com/influence/influence/internal/data"
)

// MaskFunc masks stored Polygon content for indexing: it replaces every
// Obfuscation_Token with a placeholder so neither plaintext nor ciphertext
// reaches the search index (Req 16.7, 23.2). It is supplied by the caller
// (wired to service.Mask with reveal disabled) so this package stays free of a
// dependency on the encryption/masking service — the masking seam is passed in
// rather than imported.
type MaskFunc func(content string) string

// PolygonIndex maintains the tenant-scoped `polygon_fts` FTS5 index over MASKED
// Polygon content. It embeds Base so every statement is routed to the caller's
// own tenant DB and nowhere else, and holds the masking function that guarantees
// only masked content is ever written to the index.
type PolygonIndex struct {
	Base
	mask MaskFunc
}

// NewPolygonIndex constructs a PolygonIndex over a ConnManager with the masking
// function used to strip sensitive spans before indexing. The mask function is
// the non-leakage guarantee: callers wire it to the masking seam with reveal
// disabled (service.Mask(content, false, nil)). Callers MUST pass a real
// masker; IndexPolygon refuses to index when mask is nil rather than risk
// writing raw content to the index.
func NewPolygonIndex(conns data.ConnManager, mask MaskFunc) *PolygonIndex {
	return &PolygonIndex{Base: NewBase(conns), mask: mask}
}

// IndexPolygon upserts the search-index row for the Polygon identified by
// recordID, indexing the MASKED form of content. It is called after a Polygon is
// created or edited so the index reflects current content.
//
// content is the stored Polygon content (which may contain Obfuscation_Tokens);
// it is routed through the masking function before it touches the index, so the
// row holds only the masked placeholder and never the plaintext or ciphertext
// of an encrypted span (Req 16.7, 23.2). The upsert is expressed as
// delete-then-insert because FTS5 external-content-less tables have no UPDATE-by
// -key affordance keyed on the UNINDEXED record_id column; removing any existing
// row for this Polygon first keeps exactly one index row per Polygon.
//
// The operation is scoped to the caller's own tenant DB via Base (Req 1.4, 1.5).
func (x *PolygonIndex) IndexPolygon(ctx context.Context, rc data.RequestContext, recordID, content string) error {
	if x.mask == nil {
		return fmt.Errorf("polygon index: no mask function configured")
	}

	tdb, err := x.tenant(ctx, rc)
	if err != nil {
		return err
	}

	masked := x.mask(content)

	tx, err := tdb.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin index polygon: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Upsert = clear any existing row for this Polygon, then insert the fresh
	// masked row. record_id is bound as a parameter; content is the masked text.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM polygon_fts WHERE record_id = ?`, recordID,
	); err != nil {
		return fmt.Errorf("clear index row: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO polygon_fts (record_id, content) VALUES (?, ?)`, recordID, masked,
	); err != nil {
		return fmt.Errorf("insert index row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit index polygon: %w", err)
	}
	return nil
}

// RemoveFromIndex deletes the search-index row for the Polygon identified by
// recordID. It is called when a Polygon is deleted (directly, or via a Facet or
// Sphere cascade) so the index does not retain content for a Polygon that no
// longer exists — `polygon_fts` is a virtual table and has no foreign-key
// cascade of its own.
//
// Removing an index row for a Polygon that was never indexed is a no-op, so this
// is safe to call unconditionally on delete. The operation is scoped to the
// caller's own tenant DB via Base (Req 1.4, 1.5).
func (x *PolygonIndex) RemoveFromIndex(ctx context.Context, rc data.RequestContext, recordID string) error {
	tdb, err := x.tenant(ctx, rc)
	if err != nil {
		return err
	}
	if _, err := tdb.DB().ExecContext(ctx,
		`DELETE FROM polygon_fts WHERE record_id = ?`, recordID,
	); err != nil {
		return fmt.Errorf("remove index row: %w", err)
	}
	return nil
}
