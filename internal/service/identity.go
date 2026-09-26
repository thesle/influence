package service

// Identity lookup (design § "API Surface — Identity_Endpoint", Requirement
// 32.1, 33.7). The Identity_Endpoint (GET /api/me) answers "who am I, and what
// is my security state" for the current session. Most of the answer already
// travels on the request:
//
//   - The user id and tenant id come from the immutable RequestContext the
//     middleware chain attached (identity is established at session validation).
//   - Admin_Group membership is the AuthzData.Admin flag the Authz stage loaded
//     from the Central_Directory GROUP rows; the handler passes it through
//     rather than re-deriving it here.
//
// What is NOT already on the request is the caller's display name and the two
// Two-Factor_Authentication states, all of which live in the Central_Directory:
//
//   - USER.display_name and USER.two_factor_enrolled — per-user identity and
//     2FA enrolment (Req 32.1, 33.2).
//   - TWO_FACTOR_POLICY.required — the caller's tenant 2FA policy; an absent row
//     means the policy is optional, i.e. not required (Req 33.7, 33.8).
//
// Identity is platform-wide, so this service reads the shared Central handle
// (conns.Central()) and never touches a tenant database — the same rule the
// AuthService and MetadataFooterBuilder follow.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/influence/influence/internal/data"
)

// Identity is the resolved identity + security state for the current session,
// as returned by the Identity_Endpoint (Req 32.1). The user id, display name,
// and admin flag describe who the caller is; the two 2FA fields describe the
// caller's second-factor state and their tenant's second-factor requirement.
type Identity struct {
	// UserID is the authenticated caller's Central_Directory USER id.
	UserID int64
	// DisplayName is the caller's display name resolved from the
	// Central_Directory (Req 32.1).
	DisplayName string
	// IsAdmin reports whether the caller is a member of the Admin_Group
	// (Req 32.1). It is supplied by the caller from the middleware-loaded
	// AuthzData rather than re-queried here.
	IsAdmin bool
	// TwoFactorEnrolled reports whether the caller has completed Two-Factor
	// Authentication enrolment (Req 33.2).
	TwoFactorEnrolled bool
	// TwoFactorRequired reports whether the caller's tenant requires Two-Factor
	// Authentication (Req 33.7, 33.8). An absent policy row means optional.
	TwoFactorRequired bool
}

// IdentityService resolves the current caller's identity and 2FA state from the
// shared Central_Directory. It holds only the Central handle; every lookup is
// keyed by the caller's own user/tenant id from the RequestContext, so it never
// reaches another tenant's data.
type IdentityService struct {
	central *sql.DB
}

// NewIdentityService constructs an IdentityService over the shared
// Central_Directory handle obtained from the connection manager. Identity is
// platform-wide (Req 1.1), so it lives in Central, not any tenant database.
func NewIdentityService(conns data.ConnManager) *IdentityService {
	return &IdentityService{central: conns.Central()}
}

// Me resolves the caller's identity for the Identity_Endpoint. The user and
// tenant ids come from rc; admin is the middleware-loaded Admin_Group flag; the
// display name and 2FA enrolment come from the USER row and the 2FA requirement
// from the tenant's TWO_FACTOR_POLICY row.
//
// A missing USER row (a session whose user has since disappeared) is a genuine
// error rather than a silent default — the caller reached here through a
// validated session, so the row should exist. An absent TWO_FACTOR_POLICY row
// is NOT an error: it means the tenant policy is optional, so required is false.
func (s *IdentityService) Me(ctx context.Context, rc data.RequestContext, admin bool) (Identity, error) {
	displayName, enrolled, err := s.loadUser(ctx, rc.UserID())
	if err != nil {
		return Identity{}, err
	}

	required, err := s.loadTenantTwoFactorRequired(ctx, rc.TenantID())
	if err != nil {
		return Identity{}, err
	}

	return Identity{
		UserID:            int64(rc.UserID()),
		DisplayName:       displayName,
		IsAdmin:           admin,
		TwoFactorEnrolled: enrolled,
		TwoFactorRequired: required,
	}, nil
}

// loadUser reads the caller's display name and 2FA enrolment flag from the
// Central_Directory USER row. A missing row is surfaced as an error: the caller
// authenticated through a validated session, so an absent USER is an
// inconsistency, not an expected default.
func (s *IdentityService) loadUser(ctx context.Context, userID data.UserID) (displayName string, enrolled bool, err error) {
	var enrolledInt int
	err = s.central.QueryRowContext(ctx,
		`SELECT display_name, two_factor_enrolled FROM "USER" WHERE id = ?`,
		int64(userID),
	).Scan(&displayName, &enrolledInt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("resolve identity: no USER row for id %d", int64(userID))
	}
	if err != nil {
		return "", false, fmt.Errorf("load user identity: %w", err)
	}
	return displayName, enrolledInt != 0, nil
}

// loadTenantTwoFactorRequired reads the tenant's Two_Factor_Policy. The policy
// is a single per-tenant row keyed by tenant_id; its absence means the policy is
// optional (Req 33.7, 33.8), so a missing row yields required=false rather than
// an error.
func (s *IdentityService) loadTenantTwoFactorRequired(ctx context.Context, tenantID data.TenantID) (bool, error) {
	var requiredInt int
	err := s.central.QueryRowContext(ctx,
		`SELECT required FROM "TWO_FACTOR_POLICY" WHERE tenant_id = ?`,
		int64(tenantID),
	).Scan(&requiredInt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load tenant two-factor policy: %w", err)
	}
	return requiredInt != 0, nil
}
