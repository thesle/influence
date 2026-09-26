package service

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// linkResolverFixture stands up a real, file-backed tenant behind a real
// ConnManager (mirroring the encryption fixture path) so LinkResolver runs over
// the production tenant-routing + PolicyEngine path rather than fakes.
type linkResolverFixture struct {
	resolver *LinkResolver
	rc       data.RequestContext // tenant A viewer
	tenant2  data.RequestContext // tenant B viewer (isolation)
	db       *sql.DB
}

func newLinkResolverFixture(t *testing.T) linkResolverFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	central, err := sql.Open(data.DriverName, filepath.Join(dir, "central.db"))
	if err != nil {
		t.Fatalf("open central: %v", err)
	}
	if err := data.MigrateCentral(ctx, central); err != nil {
		t.Fatalf("migrate central: %v", err)
	}

	pathA := filepath.Join(dir, "tenantA.db")
	pathB := filepath.Join(dir, "tenantB.db")
	insertEncTenant(t, central, 1, "tenant-a-uuid", "A", pathA)
	insertEncTenant(t, central, 2, "tenant-b-uuid", "B", pathB)

	dbA := migrateEncTenantFile(t, pathA)
	migrateEncTenantFile(t, pathB)

	conns := data.NewConnManager(central)
	t.Cleanup(func() { _ = conns.Close() })

	return linkResolverFixture{
		resolver: NewLinkResolver(conns, NewPolicyEngine()),
		rc:       data.NewRequestContext(10, 1, nil, nil),
		tenant2:  data.NewRequestContext(20, 2, nil, nil),
		db:       dbA,
	}
}

// seedSphereRow inserts a Sphere directly into the tenant DB and returns its
// surrogate id.
func seedSphereRow(t *testing.T, db *sql.DB, name string) data.SphereID {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO sphere (record_id, name, created_at) VALUES (?, ?, '2024-01-01T00:00:00Z')`,
		recordid.New().Canonical(), name,
	)
	if err != nil {
		t.Fatalf("seed sphere: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("sphere last id: %v", err)
	}
	return data.SphereID(id)
}

// seedPolygonRow inserts a markdown Polygon in the given Sphere and returns its
// canonical Record_ID.
func seedPolygonRow(t *testing.T, db *sql.DB, sphereID data.SphereID, content string) string {
	t.Helper()
	rid := recordid.New()
	_, err := db.Exec(
		`INSERT INTO polygon (record_id, sphere_id, type, content, author_user_id, created_at, updated_at)
		 VALUES (?, ?, 'markdown', ?, 10, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
		rid.Canonical(), int64(sphereID), content,
	)
	if err != nil {
		t.Fatalf("seed polygon: %v", err)
	}
	return rid.Canonical()
}

// grantAccess builds AuthzData granting the given access on a Sphere via one
// Group, so CanAccessSphere returns that level for that Sphere and AccessNone
// for every other.
func grantAccess(sphere data.SphereID, access data.AccessLevel) AuthzData {
	return AuthzData{
		Grants: []GroupGrant{
			{Group: 1, Sphere: sphere, Access: access},
		},
	}
}

// linkMarkdown builds a stored Cross-Refraction link with the given author label.
func linkMarkdown(label, recordID string) string {
	return "[" + label + "](influence://polygon/" + recordID + ")"
}

// --- Outcome (a): activatable to current name (Req 14.2, 15.3) --------------

func TestResolveContentActivatableUsesCurrentName(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "S1")
	target := seedPolygonRow(t, f.db, sphere, "# Design Notes\n\nbody")

	// The author's label is stale on purpose; resolution must use the target's
	// current name (its first heading), not the stored label.
	content := "see " + linkMarkdown("Old Label", target) + " for details"
	authz := grantAccess(sphere, data.AccessRead)

	got, err := f.resolver.ResolveContent(ctx, f.rc, authz, content)
	if err != nil {
		t.Fatalf("ResolveContent: %v", err)
	}

	want := "see " + linkMarkdown("Design Notes", target) + " for details"
	if got != want {
		t.Errorf("resolved content = %q, want %q", got, want)
	}
	// Activatable links retain the internal-link URL so they can be followed.
	if !strings.Contains(got, "influence://polygon/"+target) {
		t.Errorf("activatable link lost its URL: %q", got)
	}
}

func TestResolveActivatableStructuredOutcome(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "S1")
	target := seedPolygonRow(t, f.db, sphere, "# Roadmap\n\nstuff")

	rl, err := f.resolver.Resolve(ctx, f.rc, grantAccess(sphere, data.AccessRead), target)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rl.State != LinkActivatable || !rl.Activatable {
		t.Fatalf("state = %v activatable=%v, want activatable", rl.State, rl.Activatable)
	}
	if rl.Name != "Roadmap" {
		t.Errorf("name = %q, want %q", rl.Name, "Roadmap")
	}
}

// --- Outcome (b): deleted / unresolvable -> unavailable (Req 14.3) ----------

func TestResolveContentDeletedTargetUnavailable(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "S1")
	target := seedPolygonRow(t, f.db, sphere, "# Doomed\n\nbody")
	authz := grantAccess(sphere, data.AccessRead)

	// Delete the target so its Record_ID no longer resolves.
	if _, err := f.db.Exec(`DELETE FROM polygon WHERE record_id = ?`, target); err != nil {
		t.Fatalf("delete target: %v", err)
	}

	content := "link: " + linkMarkdown("Doomed", target)
	got, err := f.resolver.ResolveContent(ctx, f.rc, authz, content)
	if err != nil {
		t.Fatalf("ResolveContent: %v", err)
	}

	// Non-activatable: URL dropped, marked unavailable.
	if strings.Contains(got, "influence://polygon/") {
		t.Errorf("unavailable link must not carry a URL: %q", got)
	}
	if !strings.Contains(got, "unavailable") {
		t.Errorf("unavailable link must be marked unavailable: %q", got)
	}
}

func TestResolveContentUnknownRecordIDUnavailable(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	// A well-formed Record_ID that was never stored resolves to unavailable.
	missing := recordid.New().Canonical()
	content := linkMarkdown("Ghost", missing)

	got, err := f.resolver.ResolveContent(ctx, f.rc, AuthzData{Admin: true}, content)
	if err != nil {
		t.Fatalf("ResolveContent: %v", err)
	}
	if strings.Contains(got, "influence://polygon/") || !strings.Contains(got, "unavailable") {
		t.Errorf("unknown target should be non-activatable unavailable, got %q", got)
	}
}

func TestResolveMalformedRecordIDUnavailable(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	rl, err := f.resolver.Resolve(ctx, f.rc, AuthzData{Admin: true}, "not-a-uuid")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rl.State != LinkUnavailable {
		t.Errorf("malformed id state = %v, want unavailable", rl.State)
	}
}

// --- Outcome (c): inaccessible Sphere -> name withheld (Req 14.4) -----------

func TestResolveContentInaccessibleSphereWithholdsName(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	// Target lives in a Sphere the viewer has NO grant for.
	secretSphere := seedSphereRow(t, f.db, "Secret")
	const secretName = "Top Secret Plans"
	target := seedPolygonRow(t, f.db, secretSphere, "# "+secretName+"\n\nclassified")

	// Viewer is granted access to a DIFFERENT sphere only.
	otherSphere := seedSphereRow(t, f.db, "Public")
	authz := grantAccess(otherSphere, data.AccessRead)

	content := "ref " + linkMarkdown("Top Secret Plans", target)
	got, err := f.resolver.ResolveContent(ctx, f.rc, authz, content)
	if err != nil {
		t.Fatalf("ResolveContent: %v", err)
	}

	// Non-activatable, marked inaccessible, and the name is WITHHELD.
	if strings.Contains(got, "influence://polygon/") {
		t.Errorf("inaccessible link must not carry a URL: %q", got)
	}
	if !strings.Contains(got, "inaccessible") {
		t.Errorf("inaccessible link must be marked inaccessible: %q", got)
	}
	if strings.Contains(got, secretName) {
		t.Errorf("inaccessible link leaked the target name %q in %q", secretName, got)
	}
}

func TestResolveInaccessibleStructuredOutcomeHasNoName(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	secretSphere := seedSphereRow(t, f.db, "Secret")
	target := seedPolygonRow(t, f.db, secretSphere, "# Confidential\n\nx")

	// No grants at all → no access to the owning Sphere.
	rl, err := f.resolver.Resolve(ctx, f.rc, AuthzData{}, target)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rl.State != LinkInaccessible {
		t.Fatalf("state = %v, want inaccessible", rl.State)
	}
	if rl.Activatable {
		t.Errorf("inaccessible link must not be activatable")
	}
	if rl.Name != "" {
		t.Errorf("inaccessible outcome must withhold name, got %q", rl.Name)
	}
}

// --- Rename preserves link (same Record_ID, updated name) (Req 15.1, 15.3) --

func TestRenamePreservesLinkResolvesToUpdatedName(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "S1")
	target := seedPolygonRow(t, f.db, sphere, "# Original Title\n\nbody")
	authz := grantAccess(sphere, data.AccessRead)

	content := "go to " + linkMarkdown("Original Title", target)

	// Before rename: resolves to the original name.
	before, err := f.resolver.ResolveContent(ctx, f.rc, authz, content)
	if err != nil {
		t.Fatalf("ResolveContent before: %v", err)
	}
	if !strings.Contains(before, linkMarkdown("Original Title", target)) {
		t.Fatalf("before rename did not resolve to original name: %q", before)
	}

	// Rename the target by editing its first heading. The Record_ID is
	// untouched, so the stored link (which references the Record_ID) is
	// unchanged; only the resolved name should differ (Req 15.1, 15.3).
	if _, err := f.db.Exec(
		`UPDATE polygon SET content = ? WHERE record_id = ?`,
		"# Renamed Title\n\nbody", target,
	); err != nil {
		t.Fatalf("rename target: %v", err)
	}

	after, err := f.resolver.ResolveContent(ctx, f.rc, authz, content)
	if err != nil {
		t.Fatalf("ResolveContent after: %v", err)
	}

	// Same reference (Record_ID) — the URL is identical — but the visible name
	// tracks the rename.
	wantAfter := "go to " + linkMarkdown("Renamed Title", target)
	if after != wantAfter {
		t.Errorf("after rename resolved = %q, want %q", after, wantAfter)
	}
	if !strings.Contains(after, "influence://polygon/"+target) {
		t.Errorf("rename must preserve the stored Record_ID reference: %q", after)
	}
}

// --- Cross-tenant isolation: another tenant cannot resolve the target -------

func TestResolveContentCrossTenantIsUnavailable(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	sphere := seedSphereRow(t, f.db, "S1")
	target := seedPolygonRow(t, f.db, sphere, "# In Tenant A\n\nbody")

	// A viewer in tenant B (which has no such Polygon) sees it as unavailable —
	// nothing about tenant A leaks (Req 1.5, 1.6, 14.3).
	content := linkMarkdown("In Tenant A", target)
	got, err := f.resolver.ResolveContent(ctx, f.tenant2, AuthzData{Admin: true}, content)
	if err != nil {
		t.Fatalf("ResolveContent: %v", err)
	}
	if strings.Contains(got, "In Tenant A") == false && !strings.Contains(got, "unavailable") {
		t.Errorf("cross-tenant target should be unavailable, got %q", got)
	}
	if strings.Contains(got, "influence://polygon/") {
		t.Errorf("cross-tenant link must not be activatable: %q", got)
	}
}

// --- Multiple references and pass-through ------------------------------------

func TestResolveContentMultipleReferencesMixedStates(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	okSphere := seedSphereRow(t, f.db, "OK")
	okTarget := seedPolygonRow(t, f.db, okSphere, "# Visible Page\n\nbody")

	secretSphere := seedSphereRow(t, f.db, "Secret")
	secretTarget := seedPolygonRow(t, f.db, secretSphere, "# Hidden\n\nbody")

	missing := recordid.New().Canonical()

	authz := grantAccess(okSphere, data.AccessRead)
	content := strings.Join([]string{
		"a " + linkMarkdown("x", okTarget),
		"b " + linkMarkdown("y", secretTarget),
		"c " + linkMarkdown("z", missing),
		"plain text, no links here",
	}, "\n")

	got, err := f.resolver.ResolveContent(ctx, f.rc, authz, content)
	if err != nil {
		t.Fatalf("ResolveContent: %v", err)
	}

	if !strings.Contains(got, linkMarkdown("Visible Page", okTarget)) {
		t.Errorf("accessible target not resolved to current name: %q", got)
	}
	if strings.Contains(got, "Hidden") {
		t.Errorf("inaccessible target leaked its name: %q", got)
	}
	if !strings.Contains(got, "inaccessible") {
		t.Errorf("inaccessible target not marked: %q", got)
	}
	if !strings.Contains(got, "unavailable") {
		t.Errorf("missing target not marked unavailable: %q", got)
	}
	if !strings.Contains(got, "plain text, no links here") {
		t.Errorf("non-link content was not preserved: %q", got)
	}
}

func TestResolveContentNoLinksPassThrough(t *testing.T) {
	f := newLinkResolverFixture(t)
	ctx := context.Background()

	const content = "# Heading\n\nJust ordinary [markdown](https://example.com) content."
	got, err := f.resolver.ResolveContent(ctx, f.rc, AuthzData{}, content)
	if err != nil {
		t.Fatalf("ResolveContent: %v", err)
	}
	if got != content {
		t.Errorf("content with no internal links changed: got %q, want %q", got, content)
	}
}

// --- PolygonName derivation --------------------------------------------------

func TestPolygonNameDerivation(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"first heading", "# Title\n\nbody", "Title"},
		{"deeper first heading", "## Sub First\n\nbody", "Sub First"},
		{"heading after preamble", "intro text\n\n# Real Title\n\nbody", "Real Title"},
		{"no heading falls back to first line", "just a line\nmore", "just a line"},
		{"empty content", "", "Untitled"},
		{"whitespace only", "   \n\t\n", "Untitled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PolygonName(tc.content); got != tc.want {
				t.Errorf("PolygonName(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}
