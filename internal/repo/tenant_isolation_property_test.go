package repo

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// Feature: influence, Property 1: Tenant isolation
//
// For all pairs of distinct tenants and any content written to each, all of a
// tenant's content and metadata reside only in that tenant's Tenant_Database and
// never in the Central_Directory, and any content request from a user of one
// tenant that references a Record_ID existing only in another tenant is rejected
// as not accessible with both tenants' data left unchanged.
//
// Validates: Requirements 1.2, 1.5, 1.6
//
// The property is exercised over the real production tenant-routing path: two
// file-backed tenants behind a real ConnManager driven by a real
// Central_Directory registry (the twoTenantFixture pattern from base_test.go).
// Each rapid run:
//
//   - generates disjoint batches of Record_IDs seeded as real Sphere rows into
//     tenant A and tenant B respectively (the "content written to each");
//   - asserts every record resolves from its OWNING tenant's context and is
//     ErrNotAccessible from the OTHER tenant's context, across every content
//     RecordType (sphere/facet/polygon) — there is no cross-tenant query path
//     (Req 1.5, 1.6);
//   - asserts the Central_Directory holds no content tables at all, so content
//     and metadata live only in the tenant files (Req 1.2);
//   - asserts a cross-tenant (not-accessible) lookup mutates neither tenant's
//     data (Req 1.6 — "leave all Tenant_Database data unchanged").
func TestTenantIsolationProperty(t *testing.T) {
	f := newTwoTenantFixture(t)
	ctx := context.Background()

	// Content lives only in tenant files: the Central_Directory must contain no
	// content tables. This is invariant across all runs, so assert it once.
	assertCentralHasNoContentTables(t, f.conns.Central())

	// contentTypes is the closed set of RecordTypes that carry a Record_ID.
	// Isolation must hold for every one of them.
	contentTypes := []RecordType{RecordSphere, RecordFacet, RecordPolygon}

	rapid.Check(t, func(rt *rapid.T) {
		// Draw disjoint, non-empty batches of freshly minted Record_IDs for each
		// tenant. Fresh UUIDv4s are globally unique, so a batch seeded into A is
		// absent from B and vice versa — the essence of "existing only in
		// another tenant".
		nA := rapid.IntRange(1, 5).Draw(rt, "recordsInA")
		nB := rapid.IntRange(1, 5).Draw(rt, "recordsInB")

		idsA := make([]recordid.ID, nA)
		for i := range idsA {
			idsA[i] = recordid.New()
			seedSphere(t, f.dbA, idsA[i], rapid.String().Draw(rt, "nameA"))
		}
		idsB := make([]recordid.ID, nB)
		for i := range idsB {
			idsB[i] = recordid.New()
			seedSphere(t, f.dbB, idsB[i], rapid.String().Draw(rt, "nameB"))
		}

		// Snapshot both tenants' row counts before any cross-tenant probing.
		beforeA := countSpheres(t, f.dbA)
		beforeB := countSpheres(t, f.dbB)

		// A caller resolves ONLY its own tenant's records; the other tenant's
		// records are not accessible. A real Sphere was seeded, so resolving as
		// RecordSphere from the owner must succeed.
		for _, id := range idsA {
			if _, err := f.base.resolveRecord(ctx, f.rcA, RecordSphere, id); err != nil {
				rt.Fatalf("owner A cannot resolve its own record %s: %v", id, err)
			}
			// Every content record type, resolved from the NON-owning tenant B,
			// is not accessible — no cross-tenant path for any type.
			for _, typ := range contentTypes {
				_, err := f.base.resolveRecord(ctx, f.rcB, typ, id)
				if !errors.Is(err, ErrNotAccessible) {
					rt.Fatalf("A's record %s (type %s) leaked to tenant B: got %v, want ErrNotAccessible", id, typ, err)
				}
			}
		}
		for _, id := range idsB {
			if _, err := f.base.resolveRecord(ctx, f.rcB, RecordSphere, id); err != nil {
				rt.Fatalf("owner B cannot resolve its own record %s: %v", id, err)
			}
			for _, typ := range contentTypes {
				_, err := f.base.resolveRecord(ctx, f.rcA, typ, id)
				if !errors.Is(err, ErrNotAccessible) {
					rt.Fatalf("B's record %s (type %s) leaked to tenant A: got %v, want ErrNotAccessible", id, typ, err)
				}
			}
		}

		// The not-accessible lookups above are pure reads: neither tenant's data
		// may have changed (Req 1.6 — "leave all Tenant_Database data
		// unchanged").
		if afterA := countSpheres(t, f.dbA); afterA != beforeA {
			rt.Fatalf("tenant A row count changed by cross-tenant lookups: before=%d after=%d", beforeA, afterA)
		}
		if afterB := countSpheres(t, f.dbB); afterB != beforeB {
			rt.Fatalf("tenant B row count changed by cross-tenant lookups: before=%d after=%d", beforeB, afterB)
		}
	})
}

// assertCentralHasNoContentTables confirms the Central_Directory schema contains
// none of the tenant content tables, so content and content metadata reside
// exclusively in tenant files (Req 1.2).
func assertCentralHasNoContentTables(t *testing.T, central *sql.DB) {
	t.Helper()
	contentTables := []string{
		"sphere", "facet", "polygon", "ciphertext", "image_blob",
		"sphere_grant", "tenant_secret", "edit_lock", "user_circle",
		"bookmark", "view_log",
	}
	for _, name := range contentTables {
		var found string
		err := central.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND lower(name) = lower(?)`,
			name,
		).Scan(&found)
		if err == nil {
			t.Fatalf("Central_Directory unexpectedly contains content table %q", name)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("probe central for table %q: %v", name, err)
		}
	}
}
