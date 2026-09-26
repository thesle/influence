package api

// Property-based test for the Admin API scoping and admin-only access guarantees
// (task 27.5, Property 34). It drives requests through the wired router and the
// real middleware chain, exactly as the example tests do, but varies the route
// family, the operation, and the caller across randomized inputs.
//
// It reuses the shared snapshotAdminState / adminMutationSnapshot helpers from
// admin_denial_isolation_test.go (task 27.6) to prove "mutates nothing" and the
// standard testEnv harness (newTestEnv, secondTenantFixture, e.do,
// decodeEnvelopeCode, tenant1GroupID).
//
// Validates: Requirements 32.2, 32.3, 32.4, 32.5, 32.6, 32.7

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/influence/influence/internal/recordid"
	"pgregory.net/rapid"
)

// adminRequest is one concrete Admin_API call: an HTTP method + target + body.
// The generators below build these for each route family so a single property
// can exercise the whole admin surface.
type adminRequest struct {
	family string
	method string
	target string
	body   any
}

// Feature: influence, Property 34: Admin API tenant scoping and admin-only access
//
// Across randomized route families (users, groups, sphere-access), operations,
// and identifiers, this property asserts three guarantees the Admin_API must
// uphold:
//
//	(a) A non-admin caller is ALWAYS refused with ADMIN_REQUIRED (403) on every
//	    admin route family, and the request mutates nothing (Req 32.5). The guard
//	    runs before any handler, so this holds for reads and writes alike.
//	(b) An admin caller's operations only ever affect or return entities in the
//	    caller's own tenant; a reference to an entity in ANOTHER tenant is
//	    rejected with TENANT_ISOLATION (404) (Req 32.6). Listing endpoints never
//	    leak the other tenant's rows (Req 32.2, 32.3, 32.4).
//	(c) Create is transactional: a rejected create (cross-tenant Group) leaves no
//	    partial state — no orphan USER row and no dangling membership (Req 32.7).
func TestProperty34AdminAPIScopingAndAccess(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// A fresh environment per run keeps every run's state independent: the
		// env seeds tenant 1 (users alice/root, groups editors/admins, one Ops
		// grant) and secondTenantFixture adds tenant 2 (group outsiders, user
		// eve). Cross-tenant references below point into tenant 2.
		e := newTestEnv(t)
		foreignGroup, foreignUser := secondTenantFixture(t, e)
		localGroup := int64(tenant1GroupID(t, e))
		foreignSphere := recordid.New().Canonical() // a Record_ID absent from tenant 1

		// -----------------------------------------------------------------
		// (a) Non-admin denial across every route family (Req 32.5).
		// -----------------------------------------------------------------
		// Draw a well-formed request for a randomly chosen family/operation so
		// the denial is exercised over the whole admin surface, including both
		// safe reads and mutating writes. The values reference only tenant 1
		// entities so nothing but the admin guard can be responsible for the
		// rejection.
		denialReq := drawAdminRequest(rt, "denial", denialParams{
			localGroup:  localGroup,
			localSphere: e.sphereRecordID,
			someUserID:  int64(foreignUser), // any id: the guard rejects first
			someGroupID: localGroup,
		})
		before := snapshotAdminState(t, e)
		rec := e.do(t, denialReq.method, denialReq.target, e.sessionToken, denialReq.body)
		if rec.Code != http.StatusForbidden {
			rt.Fatalf("[%s] non-admin status = %d, want 403 (body %q)", denialReq.family, rec.Code, rec.Body.String())
		}
		if code := decodeEnvelopeCode(t, rec); code != codeAdminRequired {
			rt.Fatalf("[%s] non-admin error code = %q, want %q", denialReq.family, code, codeAdminRequired)
		}
		if after := snapshotAdminState(t, e); after != before {
			rt.Fatalf("[%s] non-admin request mutated state: before=%+v after=%+v", denialReq.family, before, after)
		}

		// -----------------------------------------------------------------
		// (b) Admin cross-tenant reference is rejected with TENANT_ISOLATION,
		//     and mutates nothing (Req 32.6). Draw a family whose write can name
		//     a foreign entity, then confirm the wall holds.
		// -----------------------------------------------------------------
		crossReq := drawCrossTenantRequest(rt, crossTenantParams{
			foreignGroup:  int64(foreignGroup),
			foreignUser:   int64(foreignUser),
			foreignSphere: foreignSphere,
			localGroup:    localGroup,
			localSphere:   e.sphereRecordID,
		})
		beforeCross := snapshotAdminState(t, e)
		crossRec := e.do(t, crossReq.method, crossReq.target, e.adminToken, crossReq.body)
		if crossRec.Code != http.StatusNotFound {
			rt.Fatalf("[%s] cross-tenant status = %d, want 404 (body %q)", crossReq.family, crossRec.Code, crossRec.Body.String())
		}
		if code := decodeEnvelopeCode(t, crossRec); code != codeTenantIsolation {
			rt.Fatalf("[%s] cross-tenant error code = %q, want %q", crossReq.family, code, codeTenantIsolation)
		}
		if after := snapshotAdminState(t, e); after != beforeCross {
			rt.Fatalf("[%s] cross-tenant request mutated state: before=%+v after=%+v", crossReq.family, beforeCross, after)
		}

		// An admin's LIST operations must never surface the other tenant's rows
		// (Req 32.2, 32.3, 32.4). Tenant 2 seeded user "eve" and group
		// "outsiders"; neither may appear.
		assertNoForeignRowsLeak(t, rt, e)

		// -----------------------------------------------------------------
		// (c) Create is transactional (Req 32.7): a create naming a foreign
		//     Group is rejected and leaves NO partial state. Randomize the new
		//     username so each run exercises a distinct create.
		// -----------------------------------------------------------------
		username := "u" + rapid.StringOfN(rapid.SampledFrom([]rune("abcdefghijklmnopqrstuvwxyz0123456789")), 3, 12, -1).Draw(rt, "username")
		beforeCreate := snapshotAdminState(t, e)
		createRec := e.do(t, http.MethodPost, "/api/admin/users", e.adminToken, createUserRequest{
			Username: username,
			Password: "Sup3rSecret!pw",
			GroupID:  int64(foreignGroup),
		})
		if createRec.Code != http.StatusNotFound {
			rt.Fatalf("cross-tenant create status = %d, want 404 (body %q)", createRec.Code, createRec.Body.String())
		}
		if code := decodeEnvelopeCode(t, createRec); code != codeTenantIsolation {
			rt.Fatalf("cross-tenant create error code = %q, want %q", code, codeTenantIsolation)
		}
		// No USER row for the attempted username: the insert rolled back.
		var n int
		if err := e.central.QueryRow(`SELECT COUNT(*) FROM "USER" WHERE username = ?`, username).Scan(&n); err != nil {
			t.Fatalf("count created user: %v", err)
		}
		if n != 0 {
			rt.Fatalf("rejected create left %d USER rows for %q, want 0 (rollback)", n, username)
		}
		// The whole mutable picture is byte-for-byte identical: no orphan user,
		// no dangling membership, nothing flipped.
		if after := snapshotAdminState(t, e); after != beforeCreate {
			rt.Fatalf("rejected create left partial state: before=%+v after=%+v", beforeCreate, after)
		}
	})
}

// denialParams carries the tenant-1 identifiers used to build a well-formed
// admin request for the non-admin denial case.
type denialParams struct {
	localGroup  int64
	localSphere string
	someUserID  int64
	someGroupID int64
}

// drawAdminRequest builds a random, well-formed admin request across all route
// families. label distinguishes the draw for rapid's shrinker.
func drawAdminRequest(rt *rapid.T, label string, p denialParams) adminRequest {
	family := rapid.SampledFrom([]string{"users", "groups", "sphere-access"}).Draw(rt, label+"-family")
	switch family {
	case "users":
		op := rapid.SampledFrom([]string{"list", "create", "deactivate", "reactivate"}).Draw(rt, label+"-users-op")
		switch op {
		case "list":
			return adminRequest{family, http.MethodGet, "/api/admin/users", nil}
		case "create":
			return adminRequest{family, http.MethodPost, "/api/admin/users", createUserRequest{Username: "x", Password: "Sup3rSecret!pw", GroupID: p.localGroup}}
		case "deactivate":
			return adminRequest{family, http.MethodPost, fmt.Sprintf("/api/admin/users/%d/deactivate", p.someUserID), nil}
		default: // reactivate
			return adminRequest{family, http.MethodPost, fmt.Sprintf("/api/admin/users/%d/reactivate", p.someUserID), nil}
		}
	case "groups":
		op := rapid.SampledFrom([]string{"list", "create", "add", "remove"}).Draw(rt, label+"-groups-op")
		switch op {
		case "list":
			return adminRequest{family, http.MethodGet, "/api/admin/groups", nil}
		case "create":
			return adminRequest{family, http.MethodPost, "/api/admin/groups", createGroupRequest{Name: "x"}}
		case "add":
			return adminRequest{family, http.MethodPost, fmt.Sprintf("/api/admin/groups/%d/members", p.someGroupID), groupMemberRequest{UserID: p.someUserID}}
		default: // remove
			return adminRequest{family, http.MethodDelete, fmt.Sprintf("/api/admin/groups/%d/members/%d", p.someGroupID, p.someUserID), nil}
		}
	default: // sphere-access
		op := rapid.SampledFrom([]string{"list", "grant", "revoke"}).Draw(rt, label+"-sphere-op")
		switch op {
		case "list":
			return adminRequest{family, http.MethodGet, "/api/admin/sphere-access", nil}
		case "grant":
			return adminRequest{family, http.MethodPut, "/api/admin/sphere-access", grantSphereAccessRequest{SphereRecordID: p.localSphere, GroupID: p.localGroup, Access: "read"}}
		default: // revoke
			return adminRequest{family, http.MethodDelete, "/api/admin/sphere-access", revokeSphereAccessRequest{SphereRecordID: p.localSphere, GroupID: p.localGroup}}
		}
	}
}

// crossTenantParams carries the foreign (tenant 2) and local (tenant 1)
// identifiers used to build a mutating admin request that references an entity
// outside the caller's tenant.
type crossTenantParams struct {
	foreignGroup  int64
	foreignUser   int64
	foreignSphere string
	localGroup    int64
	localSphere   string
}

// drawCrossTenantRequest builds a random mutating admin request whose target
// references an entity in the OTHER tenant, which must be rejected with
// TENANT_ISOLATION. Every family that can name a foreign entity is covered.
func drawCrossTenantRequest(rt *rapid.T, p crossTenantParams) adminRequest {
	kind := rapid.SampledFrom([]string{
		"create-foreign-group",
		"deactivate-foreign-user",
		"reactivate-foreign-user",
		"add-foreign-group",
		"add-foreign-user",
		"remove-foreign-group",
		"grant-foreign-group",
		"grant-foreign-sphere",
		"revoke-foreign-sphere",
	}).Draw(rt, "cross-kind")
	switch kind {
	case "create-foreign-group":
		return adminRequest{"users", http.MethodPost, "/api/admin/users", createUserRequest{Username: "x", Password: "Sup3rSecret!pw", GroupID: p.foreignGroup}}
	case "deactivate-foreign-user":
		return adminRequest{"users", http.MethodPost, fmt.Sprintf("/api/admin/users/%d/deactivate", p.foreignUser), nil}
	case "reactivate-foreign-user":
		return adminRequest{"users", http.MethodPost, fmt.Sprintf("/api/admin/users/%d/reactivate", p.foreignUser), nil}
	case "add-foreign-group":
		return adminRequest{"groups", http.MethodPost, fmt.Sprintf("/api/admin/groups/%d/members", p.foreignGroup), groupMemberRequest{UserID: p.foreignUser}}
	case "add-foreign-user":
		return adminRequest{"groups", http.MethodPost, fmt.Sprintf("/api/admin/groups/%d/members", p.localGroup), groupMemberRequest{UserID: p.foreignUser}}
	case "remove-foreign-group":
		return adminRequest{"groups", http.MethodDelete, fmt.Sprintf("/api/admin/groups/%d/members/%d", p.foreignGroup, p.foreignUser), nil}
	case "grant-foreign-group":
		return adminRequest{"sphere-access", http.MethodPut, "/api/admin/sphere-access", grantSphereAccessRequest{SphereRecordID: p.localSphere, GroupID: p.foreignGroup, Access: "read"}}
	case "grant-foreign-sphere":
		return adminRequest{"sphere-access", http.MethodPut, "/api/admin/sphere-access", grantSphereAccessRequest{SphereRecordID: p.foreignSphere, GroupID: p.localGroup, Access: "read"}}
	default: // revoke-foreign-sphere
		return adminRequest{"sphere-access", http.MethodDelete, "/api/admin/sphere-access", revokeSphereAccessRequest{SphereRecordID: p.foreignSphere, GroupID: p.localGroup}}
	}
}

// assertNoForeignRowsLeak confirms the admin list endpoints return only the
// caller's tenant rows: the tenant-2 user "eve" and group "outsiders" seeded by
// secondTenantFixture must never appear (Req 32.2, 32.3, 32.6).
func assertNoForeignRowsLeak(t *testing.T, rt *rapid.T, e *testEnv) {
	t.Helper()
	// Users listing.
	uRec := e.do(t, http.MethodGet, "/api/admin/users", e.adminToken, nil)
	if uRec.Code != http.StatusOK {
		rt.Fatalf("list users status = %d, want 200 (body %q)", uRec.Code, uRec.Body.String())
	}
	if strings.Contains(uRec.Body.String(), `"username":"eve"`) {
		rt.Fatalf("users listing leaked tenant-2 user: %s", uRec.Body.String())
	}
	// Groups listing.
	gRec := e.do(t, http.MethodGet, "/api/admin/groups", e.adminToken, nil)
	if gRec.Code != http.StatusOK {
		rt.Fatalf("list groups status = %d, want 200 (body %q)", gRec.Code, gRec.Body.String())
	}
	if strings.Contains(gRec.Body.String(), `"name":"outsiders"`) {
		rt.Fatalf("groups listing leaked tenant-2 group: %s", gRec.Body.String())
	}
}
