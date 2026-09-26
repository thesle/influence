package repo

import (
	"context"
	"errors"
	"testing"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// These tests exercise per-user Sphere circling and ordering over the real
// production tenant-routing path (the twoTenantFixture from base_test.go):
// file-backed tenants behind a real ConnManager. They verify circle idempotence
// (Req 7.1, 7.2), un-circle round-trip to the alphabetical remainder (Req 7.6),
// persisted per-user reorder (Req 7.4), and the display order — circled above
// the case-insensitive/Record_ID-tie-broken remainder (Req 7.3, 7.5).

// circledSurrogates returns the (user, sphere_id) rows for a user ordered by
// position, so a test can assert the persisted circled set and its order.
func circledSurrogates(t *testing.T, f twoTenantFixture, userID int64) []int64 {
	t.Helper()
	rows, err := f.dbA.Query(
		`SELECT sphere_id FROM user_circle WHERE user_id = ? ORDER BY position ASC`, userID,
	)
	if err != nil {
		t.Fatalf("query circle rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var sid int64
		if err := rows.Scan(&sid); err != nil {
			t.Fatalf("scan circle row: %v", err)
		}
		out = append(out, sid)
	}
	return out
}

func names(spheres []Sphere) []string {
	out := make([]string, len(spheres))
	for i, s := range spheres {
		out[i] = s.Name
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCircleAddsToList confirms circling a Sphere adds a USER_CIRCLE row for the
// caller (Req 7.1) and does not affect any other tenant.
func TestCircleAddsToList(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	rid := recordid.New()
	seedSphere(t, f.dbA, rid, "Engineering")

	if err := repo.Circle(ctx, f.rcA, rid); err != nil {
		t.Fatalf("circle: %v", err)
	}
	if got := countRows(t, f.dbA, "user_circle"); got != 1 {
		t.Errorf("tenant A user_circle count = %d, want 1", got)
	}
	if got := countRows(t, f.dbB, "user_circle"); got != 0 {
		t.Errorf("tenant B user_circle count = %d, want 0", got)
	}
}

// TestCircleIdempotent confirms circling a Sphere already circled leaves the
// list unchanged (Req 7.2).
func TestCircleIdempotent(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	rid := recordid.New()
	seedSphere(t, f.dbA, rid, "Ops")

	if err := repo.Circle(ctx, f.rcA, rid); err != nil {
		t.Fatalf("first circle: %v", err)
	}
	before := circledSurrogates(t, f, int64(f.rcA.UserID()))

	if err := repo.Circle(ctx, f.rcA, rid); err != nil {
		t.Fatalf("second circle: %v", err)
	}
	after := circledSurrogates(t, f, int64(f.rcA.UserID()))

	if len(before) != 1 || len(after) != 1 || before[0] != after[0] {
		t.Errorf("idempotent circle changed the list: before=%v after=%v", before, after)
	}
}

// TestCircleIsPerUser confirms circling is scoped to a single user: user A's
// circling does not appear for user B in the same tenant.
func TestCircleIsPerUser(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	rid := recordid.New()
	seedSphere(t, f.dbA, rid, "Shared")

	if err := repo.Circle(ctx, f.rcA, rid); err != nil {
		t.Fatalf("circle for A: %v", err)
	}

	// A second user in the same tenant A.
	rcOther := data.NewRequestContext(11, 1, nil, nil)
	if got := circledSurrogates(t, f, int64(rcOther.UserID())); len(got) != 0 {
		t.Errorf("other user should have no circled spheres, got %v", got)
	}

	ordered, err := repo.ListOrdered(ctx, rcOther)
	if err != nil {
		t.Fatalf("list ordered for other user: %v", err)
	}
	// The other user sees the sphere, but in the alphabetical remainder (not
	// pinned) — order is still just the one sphere here.
	if len(ordered) != 1 {
		t.Errorf("other user ordered list len = %d, want 1", len(ordered))
	}
}

// TestUnCircleReturnsToRemainder confirms un-circling removes the USER_CIRCLE
// row and the Sphere returns to the alphabetical remainder (Req 7.6).
func TestUnCircleReturnsToRemainder(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	pinned := recordid.New()
	seedSphere(t, f.dbA, pinned, "Zebra")
	seedSphere(t, f.dbA, recordid.New(), "Apple")

	if err := repo.Circle(ctx, f.rcA, pinned); err != nil {
		t.Fatalf("circle: %v", err)
	}

	// While circled, Zebra is first (pinned above the remainder).
	ordered, err := repo.ListOrdered(ctx, f.rcA)
	if err != nil {
		t.Fatalf("list ordered: %v", err)
	}
	if got := names(ordered); !equalStrings(got, []string{"Zebra", "Apple"}) {
		t.Fatalf("while circled: order = %v, want [Zebra Apple]", got)
	}

	// Un-circle returns Zebra to the alphabetical remainder.
	if err := repo.UnCircle(ctx, f.rcA, pinned); err != nil {
		t.Fatalf("uncircle: %v", err)
	}
	if got := countRows(t, f.dbA, "user_circle"); got != 0 {
		t.Errorf("user_circle count after uncircle = %d, want 0", got)
	}
	ordered, err = repo.ListOrdered(ctx, f.rcA)
	if err != nil {
		t.Fatalf("list ordered after uncircle: %v", err)
	}
	if got := names(ordered); !equalStrings(got, []string{"Apple", "Zebra"}) {
		t.Errorf("after uncircle: order = %v, want [Apple Zebra]", got)
	}
}

// TestUnCircleNotCircledIsNoOp confirms un-circling a Sphere that was never
// circled changes nothing and does not error.
func TestUnCircleNotCircledIsNoOp(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	rid := recordid.New()
	seedSphere(t, f.dbA, rid, "Never")

	if err := repo.UnCircle(ctx, f.rcA, rid); err != nil {
		t.Fatalf("uncircle non-circled: %v", err)
	}
	if got := countRows(t, f.dbA, "user_circle"); got != 0 {
		t.Errorf("user_circle count = %d, want 0", got)
	}
}

// TestListOrderedRemainderCaseInsensitive confirms the non-circled remainder is
// ordered ascending by name case-insensitively (Req 7.5).
func TestListOrderedRemainderCaseInsensitive(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	for _, name := range []string{"banana", "Apple", "cherry"} {
		seedSphere(t, f.dbA, recordid.New(), name)
	}
	ordered, err := repo.ListOrdered(ctx, f.rcA)
	if err != nil {
		t.Fatalf("list ordered: %v", err)
	}
	if got := names(ordered); !equalStrings(got, []string{"Apple", "banana", "cherry"}) {
		t.Errorf("remainder order = %v, want [Apple banana cherry]", got)
	}
}

// TestListOrderedRemainderRecordIDTieBreak confirms equal names (case-insensitive)
// are tie-broken by canonical Record_ID (Req 7.5).
func TestListOrderedRemainderRecordIDTieBreak(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	// Two spheres whose names compare equal under NOCASE. The canonical
	// Record_ID string decides the order.
	rid1 := recordid.New()
	rid2 := recordid.New()
	seedSphere(t, f.dbA, rid1, "same")
	seedSphere(t, f.dbA, rid2, "SAME")

	ordered, err := repo.ListOrdered(ctx, f.rcA)
	if err != nil {
		t.Fatalf("list ordered: %v", err)
	}
	if len(ordered) != 2 {
		t.Fatalf("ordered len = %d, want 2", len(ordered))
	}
	// Expected order is by canonical Record_ID ascending.
	wantFirst := rid1.Canonical()
	if rid2.Canonical() < rid1.Canonical() {
		wantFirst = rid2.Canonical()
	}
	if ordered[0].RecordID.Canonical() != wantFirst {
		t.Errorf("tie-break first = %s, want %s", ordered[0].RecordID.Canonical(), wantFirst)
	}
}

// TestListOrderedCircledAboveRemainder confirms circled Spheres appear in the
// user's persisted order above the alphabetical remainder (Req 7.3, 7.4, 7.5).
func TestListOrderedCircledAboveRemainder(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	// Remainder (not circled): Apple, Mango.
	seedSphere(t, f.dbA, recordid.New(), "Mango")
	seedSphere(t, f.dbA, recordid.New(), "Apple")

	// Circle Zebra then Yak — circled group keeps insertion order until reordered.
	zebra := recordid.New()
	yak := recordid.New()
	seedSphere(t, f.dbA, zebra, "Zebra")
	seedSphere(t, f.dbA, yak, "Yak")

	if err := repo.Circle(ctx, f.rcA, zebra); err != nil {
		t.Fatalf("circle zebra: %v", err)
	}
	if err := repo.Circle(ctx, f.rcA, yak); err != nil {
		t.Fatalf("circle yak: %v", err)
	}

	ordered, err := repo.ListOrdered(ctx, f.rcA)
	if err != nil {
		t.Fatalf("list ordered: %v", err)
	}
	// Circled (Zebra, Yak — insertion order) above alphabetical remainder
	// (Apple, Mango).
	want := []string{"Zebra", "Yak", "Apple", "Mango"}
	if got := names(ordered); !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestReorderPersistsUserOrder confirms Reorder rewrites the circled positions
// and ListOrdered reflects the new order (Req 7.4).
func TestReorderPersistsUserOrder(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	a := recordid.New()
	b := recordid.New()
	c := recordid.New()
	seedSphere(t, f.dbA, a, "Alpha")
	seedSphere(t, f.dbA, b, "Beta")
	seedSphere(t, f.dbA, c, "Gamma")

	for _, rid := range []recordid.ID{a, b, c} {
		if err := repo.Circle(ctx, f.rcA, rid); err != nil {
			t.Fatalf("circle: %v", err)
		}
	}

	// Reverse the order.
	if err := repo.Reorder(ctx, f.rcA, []recordid.ID{c, b, a}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	ordered, err := repo.ListOrdered(ctx, f.rcA)
	if err != nil {
		t.Fatalf("list ordered: %v", err)
	}
	if got := names(ordered); !equalStrings(got, []string{"Gamma", "Beta", "Alpha"}) {
		t.Errorf("reordered = %v, want [Gamma Beta Alpha]", got)
	}
}

// TestReorderRejectsNonPermutation confirms an order that is not exactly the
// currently circled set is rejected with ErrValidation and leaves the stored
// order unchanged (Req 7.4).
func TestReorderRejectsNonPermutation(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	a := recordid.New()
	b := recordid.New()
	notCircled := recordid.New()
	seedSphere(t, f.dbA, a, "Alpha")
	seedSphere(t, f.dbA, b, "Beta")
	seedSphere(t, f.dbA, notCircled, "Gamma")

	if err := repo.Circle(ctx, f.rcA, a); err != nil {
		t.Fatalf("circle a: %v", err)
	}
	if err := repo.Circle(ctx, f.rcA, b); err != nil {
		t.Fatalf("circle b: %v", err)
	}
	before := circledSurrogates(t, f, int64(f.rcA.UserID()))

	// Wrong length (missing an entry).
	if err := repo.Reorder(ctx, f.rcA, []recordid.ID{a}); !errors.Is(err, ErrValidation) {
		t.Errorf("short reorder: got %v, want ErrValidation", err)
	}
	// Includes a sphere that is not circled.
	if err := repo.Reorder(ctx, f.rcA, []recordid.ID{a, notCircled}); !errors.Is(err, ErrValidation) {
		t.Errorf("non-circled reorder: got %v, want ErrValidation", err)
	}
	// Duplicate entry.
	if err := repo.Reorder(ctx, f.rcA, []recordid.ID{a, a}); !errors.Is(err, ErrValidation) {
		t.Errorf("duplicate reorder: got %v, want ErrValidation", err)
	}

	after := circledSurrogates(t, f, int64(f.rcA.UserID()))
	if len(before) != len(after) {
		t.Fatalf("rejected reorder changed circled set size: before=%v after=%v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("rejected reorder changed order: before=%v after=%v", before, after)
		}
	}
}

// TestCircleCrossTenantNotAccessible confirms circling a Sphere owned by another
// tenant yields ErrNotAccessible and changes nothing (Req 1.6).
func TestCircleCrossTenantNotAccessible(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	rid := recordid.New()
	seedSphere(t, f.dbB, rid, "OnlyB")

	if err := repo.Circle(ctx, f.rcA, rid); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant circle: got %v, want ErrNotAccessible", err)
	}
	if got := countRows(t, f.dbA, "user_circle"); got != 0 {
		t.Errorf("tenant A user_circle count = %d, want 0", got)
	}
	if got := countRows(t, f.dbB, "user_circle"); got != 0 {
		t.Errorf("tenant B user_circle count = %d, want 0", got)
	}
}

// TestUnCircleCrossTenantNotAccessible confirms un-circling a Sphere owned by
// another tenant yields ErrNotAccessible (Req 1.6).
func TestUnCircleCrossTenantNotAccessible(t *testing.T) {
	f := newTwoTenantFixture(t)
	repo := &CircleRepo{Base: f.base}
	ctx := context.Background()

	rid := recordid.New()
	seedSphere(t, f.dbB, rid, "OnlyB")

	if err := repo.UnCircle(ctx, f.rcA, rid); !errors.Is(err, ErrNotAccessible) {
		t.Fatalf("cross-tenant uncircle: got %v, want ErrNotAccessible", err)
	}
}
