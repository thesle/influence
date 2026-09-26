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

package service

// The admin user-lifecycle service owns the write- and read-side of the User
// half of Requirement 32.2: an Admin_Group member lists the Users in their
// Tenant, creates a User, and deactivates or reactivates a User. It is the
// service the admin User routes (GET/POST /api/admin/users and
// .../{id}/deactivate|reactivate) delegate to, keeping those handlers thin.
//
// Where the state lives. Identity is platform-wide, so USER and GROUP_MEMBERSHIP
// live in the Central_Directory (Requirement 1.1); every query here runs against
// conns.Central(). There is no tenant database access — a user's tenant is the
// tenant_id column on its USER row, not a separate database.
//
// Tenant scoping is structural, never from client input (Requirement 32.6). The
// caller's tenant is read from the RequestContext; every read is filtered by
// that tenant_id and every mutation first confirms the target USER row belongs
// to it. A path or body that names a User outside the caller's tenant is
// indistinguishable from a nonexistent one and is rejected with
// ErrUserNotAccessible, which the REST layer maps to the TENANT_ISOLATION
// envelope (Req 1.6, 32.6). The initial Group a created User joins is likewise
// confirmed to live in the caller's tenant before the user is touched.
//
// Two invariants shape create (Requirement 4.1, 32.7):
//
//   - A created User must belong to at least one Group. Create takes the initial
//     Group and inserts the membership in the same transaction as the USER row,
//     so a user never exists with zero memberships.
//
//   - A create that cannot be completed leaves nothing behind. The USER insert
//     and the GROUP_MEMBERSHIP insert run in one transaction; if the membership
//     fails (or the initial Group is not in the caller's tenant, checked inside
//     the transaction) the whole thing rolls back and no USER row survives
//     (Req 32.7).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/influence/influence/internal/data"
)

// ErrUserNotAccessible is returned when an admin operation references a User
// that is not in the caller's tenant (or does not exist). The two cases are
// deliberately indistinguishable so a caller cannot probe for the existence of
// users in other tenants (Req 1.6, 32.6). The REST layer maps it to the
// TENANT_ISOLATION envelope.
var ErrUserNotAccessible = errors.New("user not accessible")

// ErrGroupNotAccessible is returned when a create names an initial Group that is
// not in the caller's tenant (or does not exist). Like ErrUserNotAccessible it
// maps to TENANT_ISOLATION: the initial Group must be a Group the admin's tenant
// owns (Req 32.6).
var ErrGroupNotAccessible = errors.New("group not accessible")

// ErrUsernameRequired is returned when a create is missing a username. A USER
// row requires a non-empty username unique within its tenant, so an empty value
// is a VALIDATION-class rejection.
var ErrUsernameRequired = errors.New("username is required")

// ErrUsernameTaken is returned when a create would duplicate a username within
// the caller's tenant (the USER (tenant_id, username) uniqueness). It is a
// VALIDATION-class error so the REST layer can surface it to the admin.
var ErrUsernameTaken = errors.New("username already exists in this tenant")

// AdminUser is the projection of a Central_Directory USER row the admin User
// listing returns. It carries the identity and lifecycle fields an
// administration page needs and deliberately omits the credential and 2FA
// secret columns.
type AdminUser struct {
	// ID is the Central_Directory USER primary key.
	ID int64
	// Username is unique within the tenant.
	Username string
	// DisplayName is the human-readable name.
	DisplayName string
	// Deactivated reports whether the account is currently deactivated
	// (Req 5.4). A deactivated user retains its account and memberships.
	Deactivated bool
	// CreatedAt is the RFC3339 creation timestamp.
	CreatedAt string
}

// AdminUserService performs the admin User-lifecycle operations of Requirement
// 32.2 against the shared Central_Directory. It holds the Central handle and a
// clock; the clock is injectable so tests can pin created_at deterministically.
type AdminUserService struct {
	central *sql.DB
	now     func() time.Time
}

// NewAdminUserService constructs an AdminUserService over the shared
// Central_Directory handle. Identity is platform-wide (Req 1.1), so it lives in
// Central, not any tenant database. The clock defaults to time.Now.
func NewAdminUserService(conns data.ConnManager) *AdminUserService {
	return &AdminUserService{central: conns.Central(), now: time.Now}
}

// clock returns the effective clock, defaulting to time.Now when unset.
func (s *AdminUserService) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// ListUsers returns every User in the caller's tenant, ordered by username for a
// stable listing (Req 32.2). The tenant is taken from the RequestContext, never
// from client input, so the result can only ever contain the caller's own
// tenant's users (Req 32.6).
func (s *AdminUserService) ListUsers(ctx context.Context, rc data.RequestContext) ([]AdminUser, error) {
	rows, err := s.central.QueryContext(ctx,
		`SELECT id, username, display_name, deactivated, created_at
		   FROM "USER" WHERE tenant_id = ? ORDER BY username`,
		int64(rc.TenantID()),
	)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AdminUser
	for rows.Next() {
		var (
			u          AdminUser
			deactivate int
		)
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &deactivate, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.Deactivated = deactivate != 0
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return out, nil
}

// CreateUser creates a User in the caller's tenant with the given username,
// display name, and initial password, and adds it to initialGroup in the same
// transaction (Req 32.2, 4.1, 32.7).
//
// The password is validated against the shared platform policy
// (ValidatePassword) and hashed with the shared Argon2id hasher (HashPassword)
// before the row is written, so no plaintext is stored (Req 3.4, 32.2). The
// display name defaults to the username when empty.
//
// The whole operation is transactional (Req 32.7): the initial Group is
// confirmed to live in the caller's tenant, the USER row is inserted, and the
// GROUP_MEMBERSHIP edge is inserted, all in one transaction. If any step fails —
// the Group is not in the tenant, the username is taken, or the membership
// insert errors — the transaction rolls back and no USER row survives. Because
// the membership is written in the same transaction, a created user is never
// left with zero Group memberships (Req 4.1).
func (s *AdminUserService) CreateUser(ctx context.Context, rc data.RequestContext, username, displayName, password string, initialGroup data.GroupID) (AdminUser, error) {
	if username == "" {
		return AdminUser{}, ErrUsernameRequired
	}
	if displayName == "" {
		displayName = username
	}

	// Validate the password against the shared policy before doing any work, so
	// a weak password is rejected with VALIDATION and never hashed.
	if err := ValidatePassword(password); err != nil {
		return AdminUser{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return AdminUser{}, fmt.Errorf("hash password: %w", err)
	}

	tenantID := int64(rc.TenantID())
	createdAt := s.clock().UTC().Format(time.RFC3339Nano)

	tx, err := s.central.BeginTx(ctx, nil)
	if err != nil {
		return AdminUser{}, fmt.Errorf("begin create user tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The initial Group must belong to the caller's tenant. Checking it inside
	// the transaction means a Group outside the tenant (or a nonexistent one)
	// aborts the create before the USER row is committed (Req 32.6, 32.7).
	var groupTenant int64
	err = tx.QueryRowContext(ctx,
		`SELECT tenant_id FROM "GROUP" WHERE id = ?`,
		int64(initialGroup),
	).Scan(&groupTenant)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && groupTenant != tenantID) {
		return AdminUser{}, ErrGroupNotAccessible
	}
	if err != nil {
		return AdminUser{}, fmt.Errorf("check initial group: %w", err)
	}

	// Reject a duplicate username within the tenant as a VALIDATION-class error
	// rather than surfacing the raw UNIQUE (tenant_id, username) violation as an
	// internal error. Checking inside the transaction keeps the create atomic.
	var taken int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM "USER" WHERE tenant_id = ? AND username = ?`,
		tenantID, username,
	).Scan(&taken); err != nil {
		return AdminUser{}, fmt.Errorf("check username: %w", err)
	}
	if taken > 0 {
		return AdminUser{}, ErrUsernameTaken
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?, ?, ?, ?, ?)`,
		tenantID, username, displayName, hash, createdAt,
	)
	if err != nil {
		return AdminUser{}, fmt.Errorf("insert user: %w", err)
	}
	userID, err := res.LastInsertId()
	if err != nil {
		return AdminUser{}, fmt.Errorf("user last insert id: %w", err)
	}

	// Add the initial membership in the same transaction so the created user
	// satisfies the at-least-one-Group invariant atomically (Req 4.1, 32.7).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "GROUP_MEMBERSHIP"(user_id, group_id) VALUES(?, ?)`,
		userID, int64(initialGroup),
	); err != nil {
		return AdminUser{}, fmt.Errorf("insert initial membership: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return AdminUser{}, fmt.Errorf("commit create user: %w", err)
	}

	return AdminUser{
		ID:          userID,
		Username:    username,
		DisplayName: displayName,
		Deactivated: false,
		CreatedAt:   createdAt,
	}, nil
}

// Deactivate marks a User in the caller's tenant as deactivated (Req 5.4). The
// account record and Group memberships are retained; only the deactivated flag
// changes, which the PolicyEngine reads to deny the user every Sphere and the
// admin pages. A User outside the caller's tenant (or nonexistent) is rejected
// with ErrUserNotAccessible before any write (Req 32.6). Deactivating an already
// deactivated user is an idempotent success.
func (s *AdminUserService) Deactivate(ctx context.Context, rc data.RequestContext, userID int64) error {
	return s.setDeactivated(ctx, rc, userID, true)
}

// Reactivate clears the deactivated flag on a User in the caller's tenant,
// restoring the access the user's Group memberships confer (Req 5.4). Like
// Deactivate it is tenant-scoped and idempotent; a User outside the tenant is
// rejected with ErrUserNotAccessible.
func (s *AdminUserService) Reactivate(ctx context.Context, rc data.RequestContext, userID int64) error {
	return s.setDeactivated(ctx, rc, userID, false)
}

// setDeactivated is the shared toggle behind Deactivate/Reactivate. It scopes
// the UPDATE to the caller's tenant with a tenant_id predicate so a User in
// another tenant is never touched, and inspects the affected-row count to
// distinguish "not in this tenant" (zero rows) from a successful (possibly
// no-op) update.
func (s *AdminUserService) setDeactivated(ctx context.Context, rc data.RequestContext, userID int64, deactivated bool) error {
	flag := 0
	if deactivated {
		flag = 1
	}
	res, err := s.central.ExecContext(ctx,
		`UPDATE "USER" SET deactivated = ? WHERE id = ? AND tenant_id = ?`,
		flag, userID, int64(rc.TenantID()),
	)
	if err != nil {
		return fmt.Errorf("update user deactivated: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		// Either the user does not exist or belongs to another tenant. Both are
		// reported identically so a caller cannot probe for cross-tenant users
		// (Req 1.6, 32.6). A same-value update still affects the row in SQLite,
		// so a zero count here is never a benign idempotent no-op.
		return ErrUserNotAccessible
	}
	return nil
}
