package service

// The auth service owns login against the Central_Directory (design.md — "Auth
// Service"). This task implements the credential-verification and session-
// creation path:
//
//   - Credentials are stored and verified with Argon2id (Requirement 3.4); see
//     password.go for the hashing/verification primitives.
//   - On valid credentials the service establishes an authenticated session
//     (Requirement 3.1) by resolving the user's single tenant (Requirements
//     1.3, 1.4) and inserting a server-side SESSION row keyed by a high-entropy
//     opaque token.
//   - On invalid credentials access is denied with an authentication error
//     (Requirement 3.2); the caller cannot distinguish an unknown username from
//     a wrong password.
//
// Two adjacent concerns are deliberately left for later tasks but have their
// hooks/columns ready:
//
//   - Brute-force lockout (Requirement 3.3, task 5.2) uses USER.failed_attempts
//     and USER.locked_until. This task reads locked_until to refuse a
//     locked account and resets failed_attempts to 0 on success, but does NOT
//     yet increment the counter or set the lock — that is task 5.2's job.
//   - Session expiry and logout (Requirement 3.6/3.5, task 5.3) use
//     SESSION.created_at/last_seen_at, which this task populates; enforcing the
//     30-minute idle / 24-hour absolute bounds and logout deletion is task 5.3.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/influence/influence/internal/data"
)

// ErrInvalidCredentials is the single error returned for any failed login where
// the username is unknown or the password does not match. Collapsing both cases
// into one error avoids revealing whether a username exists (Requirement 3.2).
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrAccountLocked is returned when the account is currently within a
// brute-force lockout window (Requirement 3.3). The full lockout mechanism is
// implemented in task 5.2; this task honors an already-set locked_until so the
// login path is correct once 5.2 sets it.
var ErrAccountLocked = errors.New("account locked")

// ErrAccountDeactivated is returned when a user whose account has been
// deactivated attempts to log in (Requirement 5.4). Such a user retains its
// account and memberships but is denied access.
var ErrAccountDeactivated = errors.New("account deactivated")

// sessionTokenBytes is the number of random bytes in a session token before
// encoding. 32 bytes (256 bits) is high-entropy enough that tokens are
// infeasible to guess (design.md — "an opaque high-entropy value").
const sessionTokenBytes = 32

// Session is the result of a successful login: the opaque token that identifies
// the server-side session, the user it belongs to, and the tenant the user was
// resolved to. The token is what a later task places in an HttpOnly/Secure
// cookie (task 5.3).
//
// State is the session's authentication lifecycle state (Req 33.4, 33.7):
//   - "active"      — a fully authenticated session with no outstanding factor.
//   - "pending_2fa" — the password passed but a second factor is still owed.
//   - "must_enrol"  — the tenant requires 2FA but the user is not yet enrolled;
//     the middleware confines this session to the enrolment + logout routes.
//
// The zero value ("") is treated as "active" by the persistence layer's column
// default so an unset State never accidentally restricts a session.
type Session struct {
	Token     string
	UserID    data.UserID
	TenantID  data.TenantID
	CreatedAt time.Time
	State     string
}

// Session state values persisted in SESSION.state (Req 33.4, 33.7). They match
// the CHECK constraint on the column; SessionStateActive is the default a
// pre-existing row carries.
const (
	SessionStateActive     = "active"
	SessionStatePending2FA = "pending_2fa"
	SessionStateMustEnrol  = "must_enrol"
)

// Clock returns the current time. It is an injection point so tests can control
// "now" deterministically for session timestamps and lockout windows. The zero
// value of AuthService uses the real clock.
type Clock func() time.Time

// AuthService authenticates users against the Central_Directory and creates
// server-side sessions. It depends only on the Central handle from the
// connection manager — identity and sessions live in the Central_Directory, not
// in any tenant database (Requirement 1.1).
type AuthService struct {
	central *sql.DB
	now     Clock
}

// NewAuthService constructs an AuthService over the shared Central_Directory
// handle obtained from the connection manager.
func NewAuthService(conns data.ConnManager) *AuthService {
	return &AuthService{central: conns.Central(), now: time.Now}
}

// withClock overrides the service clock. Used by tests to pin session
// timestamps; not part of the public surface.
func (s *AuthService) withClock(c Clock) *AuthService {
	s.now = c
	return s
}

// clock returns the effective clock, defaulting to time.Now when unset.
func (s *AuthService) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// Login verifies a username/password against the Central_Directory and, on
// success, resolves the user's single tenant and creates a server-side session
// (Requirements 3.1, 3.2, 3.4, 1.3, 1.4).
//
// Failure modes, all returning a specific error and creating no session:
//   - unknown username or wrong password → ErrInvalidCredentials (Req 3.2)
//   - account within an active lockout window → ErrAccountLocked (Req 3.3)
//   - deactivated account → ErrAccountDeactivated (Req 5.4)
func (s *AuthService) Login(ctx context.Context, username, password string) (Session, error) {
	now := s.clock()

	var (
		userID       int64
		tenantID     int64
		passwordHash string
		lockedUntil  sql.NullString
		deactivated  int
		enrolled     int
	)
	err := s.central.QueryRowContext(ctx,
		`SELECT id, tenant_id, password_hash, locked_until, deactivated, two_factor_enrolled
		   FROM "USER" WHERE username = ?`,
		username,
	).Scan(&userID, &tenantID, &passwordHash, &lockedUntil, &deactivated, &enrolled)
	if errors.Is(err, sql.ErrNoRows) {
		// Unknown username. Return the same error as a wrong password so the
		// two are indistinguishable (Req 3.2).
		return Session{}, ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, fmt.Errorf("lookup user: %w", err)
	}

	// A deactivated user is denied regardless of credential correctness
	// (Req 5.4); the account and memberships are retained in the table.
	if deactivated != 0 {
		return Session{}, ErrAccountDeactivated
	}

	// Honor an existing lockout window. Setting the window is task 5.2; this
	// path ensures a locked account cannot log in even with the right password.
	if locked, err := lockActive(lockedUntil, now); err != nil {
		return Session{}, err
	} else if locked {
		return Session{}, ErrAccountLocked
	}

	ok, err := VerifyPassword(password, passwordHash)
	if err != nil {
		// A corrupt/foreign stored hash is treated as an authentication
		// failure rather than surfacing internal hash-format details.
		return Session{}, ErrInvalidCredentials
	}
	if !ok {
		// Wrong password. Record the consecutive failure and arm the lockout
		// window on the threshold failure (Req 3.3, task 5.2), then deny
		// access (Req 3.2). A lockout bookkeeping error is surfaced so the
		// caller does not mistake it for a plain credential mismatch.
		if lockErr := s.registerFailedAttempt(ctx, userID, now); lockErr != nil {
			return Session{}, lockErr
		}
		return Session{}, ErrInvalidCredentials
	}

	// Success: clear any accumulated failure count (Req 3.3 reset-on-success)
	// and establish the session (Req 3.1). The single tenant is taken from the
	// user's own record (Req 1.3, 1.4) — never from client input.
	//
	// If the tenant's Two_Factor_Policy is required and this user has not yet
	// enrolled, the session is created in the restricted 'must_enrol' state: a
	// correct password does not grant a normal session, and the middleware
	// confines the session to the enrolment + logout routes until the user
	// enrols (Req 33.7, 33.8). Otherwise the session is 'active'.
	state := SessionStateActive
	if enrolled == 0 {
		required, err := s.tenantPolicyRequired(ctx, tenantID)
		if err != nil {
			return Session{}, err
		}
		if required {
			state = SessionStateMustEnrol
		}
	}

	session, err := s.createSession(ctx, userID, tenantID, now, state)
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

// tenantPolicyRequired reports whether the tenant's Two_Factor_Policy requires
// 2FA. An absent TWO_FACTOR_POLICY row means the policy is optional (Req 33.7),
// so a missing row reads as not-required rather than an error.
func (s *AuthService) tenantPolicyRequired(ctx context.Context, tenantID int64) (bool, error) {
	var required int
	err := s.central.QueryRowContext(ctx,
		`SELECT required FROM "TWO_FACTOR_POLICY" WHERE tenant_id = ?`,
		tenantID,
	).Scan(&required)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup two-factor policy: %w", err)
	}
	return required != 0, nil
}

// createSession clears the user's failure counter and inserts a fresh SESSION
// row within a single transaction, returning the session. The token is a
// high-entropy opaque value; created_at and last_seen_at start equal (the
// sliding/expiry logic is task 5.3).
func (s *AuthService) createSession(ctx context.Context, userID, tenantID int64, now time.Time, state string) (Session, error) {
	token, err := newSessionToken()
	if err != nil {
		return Session{}, err
	}
	ts := now.UTC().Format(time.RFC3339Nano)

	tx, err := s.central.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("begin session tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Reset-on-success (Req 3.3): a successful login clears the consecutive
	// failure counter so a later failure starts from zero.
	if _, err := tx.ExecContext(ctx,
		`UPDATE "USER" SET failed_attempts = 0, locked_until = NULL WHERE id = ?`,
		userID,
	); err != nil {
		return Session{}, fmt.Errorf("reset failure counter: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "SESSION"(token, user_id, created_at, last_seen_at, state) VALUES(?,?,?,?,?)`,
		token, userID, ts, ts, state,
	); err != nil {
		return Session{}, fmt.Errorf("insert session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit session: %w", err)
	}

	return Session{
		Token:     token,
		UserID:    data.UserID(userID),
		TenantID:  data.TenantID(tenantID),
		CreatedAt: now.UTC(),
		State:     state,
	}, nil
}

// lockActive reports whether an account is currently locked given its stored
// locked_until value and the current time. A NULL/empty value means no lock.
// The stored timestamp is RFC3339; an unparseable value is treated as a lock so
// a corrupt lock field fails closed rather than granting access.
func lockActive(lockedUntil sql.NullString, now time.Time) (bool, error) {
	if !lockedUntil.Valid || lockedUntil.String == "" {
		return false, nil
	}
	until, err := time.Parse(time.RFC3339Nano, lockedUntil.String)
	if err != nil {
		// Fail closed: an unreadable lock timestamp keeps the account locked.
		return true, nil
	}
	return now.Before(until), nil
}

// newSessionToken returns a URL-safe, high-entropy opaque session token.
func newSessionToken() (string, error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
