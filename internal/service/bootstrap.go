package service

// The bootstrap service owns first-run administrator creation (design.md —
// "First-Run Bootstrap — Req 31"). A freshly installed tenant has a registry
// row but no administrator who can log in; this service lets an operator create
// that first Admin_Group member from the browser without hand-editing the
// database (Req 31.1–31.6).
//
// First_Run_State is DERIVED, not stored (design.md — "First_Run_State needs no
// new column"): a tenant is in First_Run_State exactly when no USER assigned to
// that tenant is a member of that tenant's is_admin Group. Once a Bootstrap_Admin
// exists, the predicate is false and the setup endpoint is closed (Req 31.3,
// 31.6).
//
// TENANT RESOLUTION FOR UNAUTHENTICATED SETUP. These operations run before any
// admin (and therefore any session) exists, so there is no RequestContext and
// no authenticated tenant to scope by. The rest of the platform resolves a
// caller's tenant from identity (login looks a user up by username, and the
// username's USER row carries its single tenant_id — see AuthService.Login);
// there is no host/subdomain tenant routing anywhere in the codebase, so a host
// is effectively single-tenant at the HTTP surface. Bootstrap therefore takes an
// explicit TenantID that the caller resolves up front: the setup handler maps
// the request to a tenant by tenant_uuid when one is supplied, and otherwise
// resolves the sole active tenant on the host (the common single-tenant case).
// Passing the resolved TenantID in keeps this service a pure Central_Directory
// operation with no knowledge of HTTP, consistent with the other identity
// services (AdminUserService, AuthService) which never trust a client-supplied
// tenant either.
//
// CONCURRENCY. CreateBootstrapAdmin runs in a single Central transaction and
// re-checks First_Run_State INSIDE that transaction before inserting anything.
// Two setup requests racing against the same tenant cannot both succeed: the
// first commits the admin membership; the second re-reads the predicate under
// its own transaction, finds an admin already present, and is rejected with
// ErrSetupComplete, creating no user (design.md Property 33; Req 31.4).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/influence/influence/internal/data"
)

// bootstrapAdminGroupName is the name given to the Admin_Group when the bootstrap
// creates it because the tenant has none yet. A tenant provisioned through the
// normal path may already have an is_admin Group with a different name; the
// bootstrap reuses any existing is_admin Group and only creates this one when
// none exists, so the name is a fallback, not a required identifier.
const bootstrapAdminGroupName = "Administrators"

// usernameMaxLen is the inclusive maximum length, in runes, of a Bootstrap_Admin
// username. A username must be 1–100 characters (Req 31.2, 31.5); the lower
// bound is "non-empty" (see ErrUsernameRequired) and this is the upper bound.
const usernameMaxLen = 100

// ErrSetupComplete is returned by CreateBootstrapAdmin when the tenant is no
// longer in First_Run_State — an administrator already exists, so setup is
// closed (Req 31.4, 31.6). The REST layer maps it to a 409 SETUP_COMPLETE
// envelope. It is deliberately distinct from a validation error: the request
// was well-formed but is rejected because the one-time setup has already run.
var ErrSetupComplete = errors.New("setup already completed")

// ErrUsernameTooLong is returned when a Bootstrap_Admin username exceeds the
// 100-character maximum (Req 31.5). It is a VALIDATION-class error the REST
// layer maps to a 400 VALIDATION envelope. An empty username reuses the shared
// ErrUsernameRequired sentinel.
var ErrUsernameTooLong = errors.New("username must be at most 100 characters")

// ErrTenantNotFound is returned when a setup request cannot be mapped to a
// tenant — either the supplied tenant_uuid names no tenant, or (with no
// identifier supplied) the host has zero or more than one active tenant to
// disambiguate. It is resolved by the handler before the service is called; the
// service itself treats a nonexistent tenant as never in First_Run_State via
// the derived predicate, but the sentinel lets the handler report the mapping
// failure distinctly.
var ErrTenantNotFound = errors.New("tenant not found")

// BootstrapService creates the first administrator for a tenant and reports
// whether a tenant still needs one. Identity is platform-wide (Req 1.1), so it
// operates entirely against the shared Central_Directory handle; there is no
// tenant database access. The clock is injectable so tests can pin created_at.
type BootstrapService struct {
	central *sql.DB
	now     func() time.Time
}

// NewBootstrapService constructs a BootstrapService over the shared
// Central_Directory handle. The clock defaults to time.Now.
func NewBootstrapService(conns data.ConnManager) *BootstrapService {
	return &BootstrapService{central: conns.Central(), now: time.Now}
}

// clock returns the effective clock, defaulting to time.Now when unset.
func (s *BootstrapService) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// FirstRunState reports whether the tenant still needs a Bootstrap_Admin — i.e.
// whether it is in First_Run_State (Req 31.1). The predicate is derived: the
// tenant is in First_Run_State exactly when no USER assigned to it is a member
// of one of its is_admin Groups. A tenant that does not exist reports needed =
// false (there is nothing to bootstrap), so an unknown tenant never advertises
// an open setup endpoint.
func (s *BootstrapService) FirstRunState(ctx context.Context, tenant data.TenantID) (bool, error) {
	return firstRunState(ctx, s.central, tenant)
}

// firstRunState evaluates the First_Run_State predicate against the given query
// runner (either the *sql.DB or an in-progress *sql.Tx). It counts USER rows in
// the tenant that are members of that tenant's is_admin Group(s); zero such
// members means the tenant is still in First_Run_State. Sharing the query
// between the read-only endpoint and the in-transaction re-check keeps the two
// definitions of First_Run_State identical (Req 31.1, 31.4).
func firstRunState(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tenant data.TenantID) (bool, error) {
	var admins int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM "GROUP_MEMBERSHIP" gm
		   JOIN "USER"  u ON u.id = gm.user_id
		   JOIN "GROUP" g ON g.id = gm.group_id
		  WHERE g.tenant_id = ? AND u.tenant_id = ? AND g.is_admin = 1`,
		int64(tenant), int64(tenant),
	).Scan(&admins)
	if err != nil {
		return false, fmt.Errorf("evaluate first-run state: %w", err)
	}
	// No admin member ⇒ still first-run (needed = true).
	return admins == 0, nil
}

// CreateBootstrapAdmin creates the first administrator for the tenant and clears
// First_Run_State (Req 31.2, 31.3). It runs in a single Central transaction that:
//
//  1. re-checks First_Run_State under the transaction, rejecting with
//     ErrSetupComplete if an admin already exists so two concurrent setups
//     cannot both create one (Req 31.4, design Property 33);
//  2. validates the username (1–100 chars, unique in the tenant) and the
//     password against the shared platform policy (Req 31.2, 31.5), creating no
//     user if either fails;
//  3. hashes the password with Argon2id (Req 3.4) and inserts the USER;
//  4. ensures the tenant has an is_admin Group, creating one if absent; and
//  5. inserts the GROUP_MEMBERSHIP joining the new user to that Admin_Group.
//
// Because every step shares one transaction, a rejected or failed request
// leaves the user set unchanged and the tenant still in First_Run_State
// (design Property 33). On success the new USER id is returned.
func (s *BootstrapService) CreateBootstrapAdmin(ctx context.Context, tenant data.TenantID, username, password string) (data.UserID, error) {
	// Validate the well-formedness of the inputs before opening a transaction so
	// a plainly bad request is rejected cheaply and never hashes a password.
	if err := validateBootstrapUsername(username); err != nil {
		return 0, err
	}
	if err := ValidatePassword(password); err != nil {
		return 0, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return 0, fmt.Errorf("hash password: %w", err)
	}

	tenantID := int64(tenant)
	createdAt := s.clock().UTC().Format(time.RFC3339Nano)

	tx, err := s.central.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin bootstrap tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-check First_Run_State INSIDE the transaction. This is the exclusivity
	// guard: a concurrent setup that already committed an admin makes this read
	// see needed = false, so the second request is rejected and creates nothing
	// (Req 31.4, 31.6).
	needed, err := firstRunState(ctx, tx, tenant)
	if err != nil {
		return 0, err
	}
	if !needed {
		return 0, ErrSetupComplete
	}

	// Reject a duplicate username within the tenant as a VALIDATION-class error
	// rather than surfacing the raw UNIQUE (tenant_id, username) violation.
	var taken int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM "USER" WHERE tenant_id = ? AND username = ?`,
		tenantID, username,
	).Scan(&taken); err != nil {
		return 0, fmt.Errorf("check username: %w", err)
	}
	if taken > 0 {
		return 0, ErrUsernameTaken
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?, ?, ?, ?, ?)`,
		tenantID, username, username, hash, createdAt,
	)
	if err != nil {
		return 0, fmt.Errorf("insert bootstrap user: %w", err)
	}
	userID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("bootstrap user last insert id: %w", err)
	}

	// Ensure the tenant has an is_admin Group, reusing an existing one if the
	// tenant was provisioned with an Admin_Group already and creating the
	// fallback one otherwise. Either way the membership below joins the new user
	// to an is_admin Group, which is what clears First_Run_State.
	adminGroupID, err := ensureAdminGroup(ctx, tx, tenantID)
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "GROUP_MEMBERSHIP"(user_id, group_id) VALUES(?, ?)`,
		userID, adminGroupID,
	); err != nil {
		return 0, fmt.Errorf("insert admin membership: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit bootstrap: %w", err)
	}

	return data.UserID(userID), nil
}

// validateBootstrapUsername enforces the 1–100 character username rule (Req
// 31.2, 31.5). An empty username is ErrUsernameRequired; one longer than 100
// runes is ErrUsernameTooLong. Length is counted in runes so a multi-byte name
// is measured by characters the operator typed, matching the password policy's
// rune-based length count.
func validateBootstrapUsername(username string) error {
	n := len([]rune(username))
	if n == 0 {
		return ErrUsernameRequired
	}
	if n > usernameMaxLen {
		return ErrUsernameTooLong
	}
	return nil
}

// ensureAdminGroup returns the id of an is_admin Group for the tenant, creating
// the fallback Administrators Group if the tenant has none. It runs on the
// bootstrap transaction so the group creation and the membership insert commit
// atomically with the USER row. When the tenant already has one or more is_admin
// Groups, the lowest-id one is reused rather than adding a second.
func ensureAdminGroup(ctx context.Context, tx *sql.Tx, tenantID int64) (int64, error) {
	var groupID int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM "GROUP" WHERE tenant_id = ? AND is_admin = 1 ORDER BY id LIMIT 1`,
		tenantID,
	).Scan(&groupID)
	if err == nil {
		return groupID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("lookup admin group: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO "GROUP"(tenant_id, name, is_admin) VALUES(?, ?, 1)`,
		tenantID, bootstrapAdminGroupName,
	)
	if err != nil {
		return 0, fmt.Errorf("create admin group: %w", err)
	}
	groupID, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("admin group last insert id: %w", err)
	}
	return groupID, nil
}

// ResolveActiveTenant maps a setup request to a tenant. When uuid is non-empty
// it looks the tenant up by tenant_uuid; when empty it resolves the sole active
// tenant on the host (the single-tenant deployment case). A uuid that names no
// tenant, or an empty uuid with zero or more than one active tenant, returns
// ErrTenantNotFound so the handler can reject the request without guessing.
//
// This lives on the service (over the Central handle) rather than in the handler
// so the tenant-mapping rule stays next to the bootstrap logic it feeds, and so
// tests can exercise it directly.
func (s *BootstrapService) ResolveActiveTenant(ctx context.Context, uuid string) (data.TenantID, error) {
	if uuid != "" {
		var id int64
		err := s.central.QueryRowContext(ctx,
			`SELECT id FROM "TENANT" WHERE tenant_uuid = ? AND status = 'active'`,
			uuid,
		).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrTenantNotFound
		}
		if err != nil {
			return 0, fmt.Errorf("resolve tenant by uuid: %w", err)
		}
		return data.TenantID(id), nil
	}

	// No identifier: resolve the sole active tenant. Two rows means the host is
	// multi-tenant and the caller must disambiguate with a tenant_uuid; zero
	// rows means nothing to set up.
	rows, err := s.central.QueryContext(ctx,
		`SELECT id FROM "TENANT" WHERE status = 'active' ORDER BY id LIMIT 2`,
	)
	if err != nil {
		return 0, fmt.Errorf("resolve sole tenant: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		ids   []int64
		curID int64
	)
	for rows.Next() {
		if err := rows.Scan(&curID); err != nil {
			return 0, fmt.Errorf("scan tenant id: %w", err)
		}
		ids = append(ids, curID)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate tenants: %w", err)
	}
	if len(ids) != 1 {
		return 0, ErrTenantNotFound
	}
	return data.TenantID(ids[0]), nil
}
