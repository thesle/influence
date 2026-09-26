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

// This file establishes the foundational repository pattern that every content
// repository (Sphere, Facet, Polygon, and the records they own) builds on. The
// pattern makes tenant isolation structural rather than a per-query convention
// (design.md — "Connection Manager (Tenant Routing)", Requirements 1.4, 1.5,
// 1.6):
//
//   - A repository never opens a database itself. It holds a data.ConnManager
//     and, for every operation, resolves the tenant handle from the caller's
//     RequestContext via ConnManager.Tenant. The manager returns a handle ONLY
//     for ctx.TenantID, so a repository can never reach another tenant's file.
//
//   - Records are addressed externally by Record_ID (a UUIDv4 / Short-UUID).
//     The base helper resolves a Record_ID to its internal surrogate id by
//     querying ONLY the caller's tenant DB. A Record_ID that belongs to another
//     tenant is simply absent from this tenant's tables, so the lookup finds
//     nothing and returns ErrNotAccessible (Req 1.6) — there is no cross-tenant
//     query path and, being a pure lookup, no data changes.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// ErrNotAccessible is returned when a request references a record that is not
// present in the caller's authenticated tenant — whether because it does not
// exist at all or because it lives in another tenant. The two cases are
// deliberately indistinguishable so the response never reveals that a record
// exists elsewhere (Req 1.6). Handlers map this to the TENANT_ISOLATION error
// envelope (design.md — "Tenant isolation (403/404 `TENANT_ISOLATION`)").
var ErrNotAccessible = errors.New("record not accessible in this tenant")

// RecordType names the content record kinds that carry a Record_ID. Each maps
// to the tenant table whose UNIQUE record_id column the base helper searches.
// Keeping the set closed here means a lookup can only ever target a known
// tenant-local table — there is no way to point a resolve at an arbitrary table
// or another tenant's data.
type RecordType string

const (
	// RecordSphere addresses the tenant's "sphere" table.
	RecordSphere RecordType = "sphere"
	// RecordFacet addresses the tenant's "facet" table.
	RecordFacet RecordType = "facet"
	// RecordPolygon addresses the tenant's "polygon" table.
	RecordPolygon RecordType = "polygon"
)

// recordTable maps a RecordType to the tenant table it lives in. It is a fixed
// allow-list: only these tables can be targeted by a Record_ID resolve, so the
// table name fed into a query is always a trusted internal constant, never
// client input.
var recordTable = map[RecordType]string{
	RecordSphere:  "sphere",
	RecordFacet:   "facet",
	RecordPolygon: "polygon",
}

// Base is the foundation every content repository embeds. It owns nothing but a
// ConnManager; all tenant scoping flows through the RequestContext passed to
// each call. Concrete repositories (added in later CRUD tasks) embed Base and
// use tenant / resolveRecord to run their tenant-scoped queries.
type Base struct {
	conns data.ConnManager
}

// NewBase constructs a Base over a ConnManager. Content repositories embed the
// returned value (or construct it via their own constructors) so they share the
// single tenant-routing gateway.
func NewBase(conns data.ConnManager) Base {
	return Base{conns: conns}
}

// tenant resolves the caller's own tenant handle and nothing else. It is the
// only way a repository reaches a database: the handle is for rc.TenantID,
// resolved by the ConnManager from the authenticated context, never from client
// input (Req 1.4, 1.5). A ConnManager error (e.g. an unknown/inactive tenant)
// is translated to ErrNotAccessible so callers cannot tell a missing tenant
// from a missing record.
func (b Base) tenant(ctx context.Context, rc data.RequestContext) (*data.TenantDB, error) {
	tdb, err := b.conns.Tenant(ctx, rc)
	if err != nil {
		if errors.Is(err, data.ErrTenantNotFound) {
			return nil, ErrNotAccessible
		}
		return nil, fmt.Errorf("resolve tenant: %w", err)
	}
	return tdb, nil
}

// resolveRecord translates an external Record_ID into the internal surrogate
// primary key of a record of the given type, searching ONLY the caller's tenant
// DB. It is the shared entry point content repositories use to turn a URL/API
// Record_ID into a row they can operate on.
//
// Isolation is structural: the query runs against rc.TenantID's database and
// filters by the UNIQUE record_id column. A Record_ID that exists only in
// another tenant is absent here, so the query returns no rows and the method
// returns ErrNotAccessible (Req 1.6). The lookup is read-only, so a
// not-accessible reference changes no data in either tenant.
func (b Base) resolveRecord(ctx context.Context, rc data.RequestContext, typ RecordType, id recordid.ID) (int64, error) {
	return b.resolveRecordCanonical(ctx, rc, typ, id.Canonical())
}

// resolveRecordCanonical is resolveRecord for a Record_ID already in its
// canonical string form (as stored in the record_id column) — for example when
// a caller has read the value straight out of the database. It shares the exact
// same tenant-scoped, allow-listed query path.
func (b Base) resolveRecordCanonical(ctx context.Context, rc data.RequestContext, typ RecordType, canonical string) (int64, error) {
	table, ok := recordTable[typ]
	if !ok {
		// An unknown record type can never be a valid tenant-local table, so it
		// is treated as not accessible rather than executing any query.
		return 0, fmt.Errorf("%w: unknown record type %q", ErrNotAccessible, typ)
	}

	tdb, err := b.tenant(ctx, rc)
	if err != nil {
		return 0, err
	}

	// table comes from the fixed recordTable allow-list above (never client
	// input); the record_id value is always bound as a parameter.
	query := fmt.Sprintf(`SELECT id FROM %s WHERE record_id = ?`, table)

	var surrogate int64
	err = tdb.DB().QueryRowContext(ctx, query, canonical).Scan(&surrogate)
	if errors.Is(err, sql.ErrNoRows) {
		// Absent from this tenant — either nonexistent or owned by another
		// tenant. Indistinguishable by design (Req 1.6). No data changed.
		return 0, ErrNotAccessible
	}
	if err != nil {
		return 0, fmt.Errorf("resolve %s record: %w", typ, err)
	}
	return surrogate, nil
}
