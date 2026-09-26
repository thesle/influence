package service

// Property-based coverage for the first-run bootstrap (design.md — "First-Run
// Bootstrap — Req 31"; Property 33). These run against the same real in-memory
// Central_Directory harness the unit tests use (newTestConns / seedTenant), so
// the derived First_Run_State predicate and the single-transaction create run
// over production SQL rather than a fake.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/influence/influence/internal/data"
	"pgregory.net/rapid"
)

// Feature: influence, Property 33: First-run bootstrap idempotence and exclusivity
//
// TestProperty33FirstRunBootstrap drives randomized sequences of setup attempts
// (some concurrent, some sequential; some well-formed, some rejected for a weak
// password or invalid username) at a fresh tenant and asserts the bootstrap's
// idempotence and exclusivity contract holds across every input:
//
//   (a) At most ONE Bootstrap_Admin is ever created for a tenant. After the
//       first successful CreateBootstrapAdmin, First_Run_State is false and every
//       later create is rejected with ErrSetupComplete and creates no user
//       (Req 31.4, 31.6).
//   (b) Concurrent attempts against a fresh tenant yield exactly one success —
//       the in-transaction re-check makes racers mutually exclusive (Req 31.4).
//   (c) A rejected create (weak password, invalid username, or setup-complete)
//       leaves the user set unchanged and the tenant's first-run state
//       consistent (Req 31.2).
//   (d) Once created, the admin is resolvable and first-run reports false
//       (Req 31.3).
func TestProperty33FirstRunBootstrap(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	svc := NewBootstrapService(conns)
	ctx := context.Background()

	// A monotonically increasing counter gives each rapid iteration its own
	// tenant. newTestConns keys the shared in-memory DB by t.Name(), so all
	// iterations run against ONE database; scoping every attempt and assertion
	// to a fresh tenant keeps iterations independent while still exercising the
	// production SQL and the real (tenant_id, username) uniqueness.
	var tenantSeq int

	rapid.Check(t, func(rt *rapid.T) {
		tenantSeq++
		uuid := fmt.Sprintf("p33-%d", tenantSeq)
		tenantID := seedTenant(t, central, uuid, "Org "+uuid)
		tenant := data.TenantID(tenantID)

		// A fresh tenant must start in First_Run_State (needed = true).
		needed, err := svc.FirstRunState(ctx, tenant)
		if err != nil {
			rt.Fatalf("initial FirstRunState: %v", err)
		}
		if !needed {
			rt.Fatalf("fresh tenant %d is not in First_Run_State", tenantID)
		}

		// Generate a batch of attempts. Each attempt is either well-formed (a
		// valid unique username + a policy-compliant password) or deliberately
		// invalid in exactly one dimension so a rejection class is exercised.
		nAttempts := rapid.IntRange(1, 6).Draw(rt, "nAttempts")
		concurrent := rapid.Bool().Draw(rt, "concurrent")

		attempts := make([]bootstrapAttempt, nAttempts)
		for i := range attempts {
			attempts[i] = drawAttempt(rt, i)
		}

		var (
			mu           sync.Mutex
			validSuccess int // successful creates from well-formed attempts
			setupClosed  int // rejections because setup was already complete
		)

		record := func(a bootstrapAttempt, err error) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				// Only a well-formed attempt may ever succeed.
				if a.wantReject {
					rt.Errorf("invalid attempt (%s) unexpectedly succeeded", a.rejectKind)
					return
				}
				validSuccess++
			case errors.Is(err, ErrSetupComplete):
				setupClosed++
			case a.wantReject:
				// A validation rejection: it must carry the sentinel matching the
				// specific defect the attempt injected (Req 31.2).
				if !errors.Is(err, a.wantErr) {
					rt.Errorf("invalid attempt (%s) rejected with %v, want %v", a.rejectKind, err, a.wantErr)
				}
			default:
				// A well-formed attempt that neither succeeded nor lost the race
				// nor hit setup-complete is an unexpected failure.
				rt.Errorf("well-formed attempt failed unexpectedly: %v", err)
			}
		}

		run := func(a bootstrapAttempt) {
			_, err := svc.CreateBootstrapAdmin(ctx, tenant, a.username, a.password)
			record(a, err)
		}

		if concurrent {
			var wg sync.WaitGroup
			start := make(chan struct{})
			for _, a := range attempts {
				wg.Add(1)
				go func(a bootstrapAttempt) {
					defer wg.Done()
					<-start
					run(a)
				}(a)
			}
			close(start)
			wg.Wait()
		} else {
			for _, a := range attempts {
				run(a)
			}
		}

		// (a) + (b): at most one admin ever exists for the tenant. Whether or not
		// any well-formed attempt was present, the tenant can hold no more than a
		// single admin user, and exactly the ones we could create.
		anyValid := false
		for _, a := range attempts {
			if !a.wantReject {
				anyValid = true
				break
			}
		}

		users := countUsersInTenant(t, central, tenantID)
		admins := countAdminMembersDB(t, central, tenantID)

		if anyValid {
			// Exactly one well-formed attempt wins; the rest (valid or invalid)
			// create nothing.
			if validSuccess != 1 {
				rt.Fatalf("valid successes = %d, want exactly 1 (attempts=%+v concurrent=%v)", validSuccess, attempts, concurrent)
			}
			if users != 1 {
				rt.Fatalf("user count = %d, want exactly 1", users)
			}
			if admins != 1 {
				rt.Fatalf("admin members = %d, want exactly 1", admins)
			}
			// (d): the tenant is now out of First_Run_State and the admin is
			// resolvable as such.
			needed, err = svc.FirstRunState(ctx, tenant)
			if err != nil {
				rt.Fatalf("post-create FirstRunState: %v", err)
			}
			if needed {
				rt.Fatalf("tenant %d still in First_Run_State after a successful create", tenantID)
			}
		} else {
			// (c): every attempt was invalid, so nothing was created and the
			// tenant remains in First_Run_State.
			if validSuccess != 0 {
				rt.Fatalf("valid successes = %d with no well-formed attempt, want 0", validSuccess)
			}
			if users != 0 {
				rt.Fatalf("user count = %d after only-invalid attempts, want 0", users)
			}
			if admins != 0 {
				rt.Fatalf("admin members = %d after only-invalid attempts, want 0", admins)
			}
			needed, err = svc.FirstRunState(ctx, tenant)
			if err != nil {
				rt.Fatalf("FirstRunState after invalid-only attempts: %v", err)
			}
			if !needed {
				rt.Fatalf("tenant %d left First_Run_State despite no successful create", tenantID)
			}
		}

		// (a): once setup is complete (a valid create won), a fresh sequential
		// attempt must be rejected with ErrSetupComplete and add no user.
		if anyValid {
			_, err := svc.CreateBootstrapAdmin(ctx, tenant, "late-"+uuid, goodBootstrapPassword)
			if !errors.Is(err, ErrSetupComplete) {
				rt.Fatalf("post-completion create error = %v, want ErrSetupComplete", err)
			}
			if got := countUsersInTenant(t, central, tenantID); got != 1 {
				rt.Fatalf("post-completion user count = %d, want 1 (rejected create must persist nothing)", got)
			}
		}
	})
}

// bootstrapAttempt is a single generated setup request together with its
// expected verdict when evaluated against a fresh tenant. wantReject marks an
// attempt that is invalid in exactly one dimension; wantErr is the sentinel that
// rejection must wrap, and rejectKind labels the defect for diagnostics.
type bootstrapAttempt struct {
	username   string
	password   string
	wantReject bool
	wantErr    error
	rejectKind string
}

// drawAttempt generates one setup attempt. It biases toward well-formed requests
// (so races and idempotence are exercised) while still producing each rejection
// class — weak password, empty username, and over-long username — so property
// (c) sees real validation rejections. Usernames of well-formed attempts embed
// the batch index i to stay unique within the tenant, isolating the exclusivity
// guarantee from the (tenant_id, username) uniqueness rule.
func drawAttempt(rt *rapid.T, i int) bootstrapAttempt {
	kind := rapid.SampledFrom([]string{
		"valid", "valid", "valid", // weight valid heavier
		"weak-password", "empty-username", "long-username",
	}).Draw(rt, "kind")

	switch kind {
	case "weak-password":
		return bootstrapAttempt{
			username:   fmt.Sprintf("user-%d", i),
			password:   drawWeakPassword(rt),
			wantReject: true,
			wantErr:    ErrPasswordPolicy,
			rejectKind: kind,
		}
	case "empty-username":
		return bootstrapAttempt{
			username:   "",
			password:   goodBootstrapPassword,
			wantReject: true,
			wantErr:    ErrUsernameRequired,
			rejectKind: kind,
		}
	case "long-username":
		return bootstrapAttempt{
			username:   strings.Repeat("a", 101),
			password:   goodBootstrapPassword,
			wantReject: true,
			wantErr:    ErrUsernameTooLong,
			rejectKind: kind,
		}
	default: // "valid"
		return bootstrapAttempt{
			username: fmt.Sprintf("admin-%d-%s", i, rapid.StringMatching(`[a-z]{1,8}`).Draw(rt, "uname")),
			password: goodBootstrapPassword,
		}
	}
}

// drawWeakPassword produces a password that fails ValidatePassword for a reason
// other than the value we treat as valid: either too short or drawn from a
// single character class. It is verified to actually violate the policy so the
// generator can never accidentally emit an acceptable password.
func drawWeakPassword(rt *rapid.T) string {
	pw := rapid.OneOf(
		// Too short (< 12 runes) but two classes.
		rapid.StringMatching(`[a-z]{1,5}[A-Z]{1,5}`),
		// Long enough but a single class (lowercase only) — fails the strength check.
		rapid.StringMatching(`[a-z]{12,20}`),
	).Draw(rt, "weakPassword")
	if ValidatePassword(pw) == nil {
		// Guard: if a generated string happened to satisfy the policy, fall back
		// to an unambiguously weak one so the attempt's expected verdict holds.
		return "short"
	}
	return pw
}

// countUsersInTenant returns the number of USER rows assigned to the tenant.
func countUsersInTenant(t *testing.T, central *sql.DB, tenantID int64) int {
	t.Helper()
	var n int
	if err := central.QueryRow(`SELECT COUNT(*) FROM "USER" WHERE tenant_id = ?`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}
