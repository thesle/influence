package repo

import (
	"context"
	"errors"
	"testing"

	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// Feature: influence, Property 8: Polygon type validity
//
// For all type values supplied at Polygon creation, creation succeeds if and
// only if the type is one of Markdown_Page, Folder_Polygon, Tabular_Polygon, or
// Whiteboard_Polygon; an invalid type creates no Polygon.
//
// Validates: Requirements 10.1, 10.3
//
// The property is exercised over the real production tenant-routing path: a
// file-backed tenant behind a real ConnManager driven by a real
// Central_Directory registry (the twoTenantFixture pattern from base_test.go),
// with a PolygonRepo bound to that ConnManager. A Sphere is seeded once into
// tenant A so every Create has a valid, accessible target Sphere — this isolates
// the type-validity decision from Sphere resolution.
//
// Each rapid run draws a type value that is either one of the four valid types
// or an arbitrary string, and asserts:
//
//   - a valid type yields a stored Polygon of exactly that type (Req 10.1),
//     increasing the tenant's Polygon count by one;
//   - an invalid type is rejected with ErrValidation and creates nothing — the
//     Polygon count is unchanged (Req 10.3);
//
// so Create succeeds iff the type is valid.
func TestPolygonTypeValidityProperty(t *testing.T) {
	f := newPolygonFixture(t)
	ctx := context.Background()

	sphereRID := recordid.New()
	seedSphere(t, f.dbA, sphereRID, "Sphere One")

	// The closed set of the four supported types (Req 10.1). Membership here is
	// the oracle: Create must succeed for exactly these values and no others.
	validTypes := []PolygonType{PolygonMarkdown, PolygonFolder, PolygonTabular, PolygonWhiteboard}
	validSet := map[PolygonType]struct{}{}
	for _, v := range validTypes {
		validSet[v] = struct{}{}
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Draw a candidate type: either one of the four valid values or an
		// arbitrary string. The arbitrary branch covers the empty string, casing
		// variants, and unrelated strings — anything outside the closed set.
		var typ PolygonType
		if rapid.Bool().Draw(rt, "useValidType") {
			typ = validTypes[rapid.IntRange(0, len(validTypes)-1).Draw(rt, "validIndex")]
		} else {
			typ = PolygonType(rapid.String().Draw(rt, "arbitraryType"))
		}
		_, isValid := validSet[typ]

		before := countPolygons(t, f.dbA)
		got, err := f.repo.Create(ctx, f.rcA, sphereRID.Canonical(), typ, 10, CreateOptions{})
		after := countPolygons(t, f.dbA)

		if isValid {
			// Valid type: creation succeeds and stores a Polygon of that type.
			if err != nil {
				rt.Fatalf("valid type %q: got error %v, want success", typ, err)
			}
			if got.Type != typ {
				rt.Fatalf("valid type %q: stored type = %q, want %q", typ, got.Type, typ)
			}
			if _, perr := recordid.Parse(got.RecordID); perr != nil {
				rt.Fatalf("valid type %q: assigned record id %q is not valid: %v", typ, got.RecordID, perr)
			}
			if after != before+1 {
				rt.Fatalf("valid type %q: polygon count %d -> %d, want +1", typ, before, after)
			}
		} else {
			// Invalid type: rejected with ErrValidation, nothing created.
			if !errors.Is(err, ErrValidation) {
				rt.Fatalf("invalid type %q: got %v, want ErrValidation", typ, err)
			}
			if after != before {
				rt.Fatalf("invalid type %q rejection changed polygon count: before=%d after=%d", typ, before, after)
			}
		}
	})
}
