package service

import (
	"context"
	"testing"

	"github.com/influence/influence/internal/data"
	"github.com/influence/influence/internal/recordid"
)

// revealFixture extends the encrypt fixture path with a real PolicyEngine so
// RevealToken exercises the production tenant-routing, key-unwrap, and per-Sphere
// permission checks rather than fakes.
type revealFixture struct {
	encryptFixture
	policy PolicyEngine
}

func newRevealFixture(t *testing.T) revealFixture {
	t.Helper()
	return revealFixture{
		encryptFixture: newEncryptFixture(t),
		policy:         NewPolicyEngine(),
	}
}

// sphereIDOf returns the surrogate sphere id owning the given Polygon Record_ID.
func sphereIDOf(t *testing.T, f revealFixture, polyRID recordid.ID) data.SphereID {
	t.Helper()
	var sid data.SphereID
	if err := f.db.QueryRow(
		`SELECT sphere_id FROM polygon WHERE record_id = ?`, polyRID.Canonical(),
	).Scan(&sid); err != nil {
		t.Fatalf("read sphere id: %v", err)
	}
	return sid
}

// revealAuthz builds AuthzData granting read+reveal on the given Sphere via one
// Group, so CanReveal returns true for that Sphere and false for every other.
func revealAuthz(sphere data.SphereID, reveal bool) AuthzData {
	return AuthzData{
		Grants: []GroupGrant{
			{Group: 1, Sphere: sphere, Access: data.AccessRead, Reveal: reveal},
		},
	}
}

// encryptOneSpan encrypts the first occurrence of secret within content and
// returns the token id and the Polygon Record_ID.
func encryptOneSpan(t *testing.T, f revealFixture, content, secret string) (string, recordid.ID) {
	t.Helper()
	polyRID := seedPolygon(t, f.db, content)
	start := indexOf(t, content, secret)
	res, err := f.svc.EncryptSpan(context.Background(), f.rc, polyRID.Canonical(), start, start+len(secret))
	if err != nil {
		t.Fatalf("EncryptSpan: %v", err)
	}
	return res.TokenID, polyRID
}

func indexOf(t *testing.T, s, sub string) int {
	t.Helper()
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	t.Fatalf("substring %q not found in %q", sub, s)
	return -1
}

// TestRevealTokenReturnsPlaintextForPermittedSphere verifies a caller with
// Reveal_Permission for the token's owning Sphere gets exactly that token's
// plaintext back (Req 16.4).
func TestRevealTokenReturnsPlaintextForPermittedSphere(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	tokenID, polyRID := encryptOneSpan(t, f, "public SECRET public", "SECRET")
	sphere := sphereIDOf(t, f, polyRID)

	plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, revealAuthz(sphere, true), tokenID)
	if err != nil {
		t.Fatalf("RevealToken: %v", err)
	}
	if plain != "SECRET" {
		t.Errorf("revealed plaintext = %q, want %q", plain, "SECRET")
	}
}

// TestRevealTokenRevealsOnlyRequestedToken verifies revealing one token has no
// effect on any other token: a second token in the same Polygon is untouched
// and still requires its own reveal (Req 16.4 — others stay masked).
func TestRevealTokenRevealsOnlyRequestedToken(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	// Encrypt two spans in the same Polygon.
	const content = "alpha ONE beta TWO gamma"
	polyRID := seedPolygon(t, f.db, content)
	s1 := indexOf(t, content, "ONE")
	r1, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), s1, s1+len("ONE"))
	if err != nil {
		t.Fatalf("first EncryptSpan: %v", err)
	}
	updated := readContent(t, f.db, polyRID)
	s2 := indexOf(t, updated, "TWO")
	r2, err := f.svc.EncryptSpan(ctx, f.rc, polyRID.Canonical(), s2, s2+len("TWO"))
	if err != nil {
		t.Fatalf("second EncryptSpan: %v", err)
	}
	sphere := sphereIDOf(t, f, polyRID)
	authz := revealAuthz(sphere, true)

	// Revealing token 1 returns only "ONE".
	got1, err := f.svc.RevealToken(ctx, f.rc, f.policy, authz, r1.TokenID)
	if err != nil {
		t.Fatalf("RevealToken(1): %v", err)
	}
	if got1 != "ONE" {
		t.Errorf("token 1 revealed = %q, want %q", got1, "ONE")
	}

	// Token 2 is unaffected: its own reveal returns "TWO", and the stored
	// content still contains token 2's placeholder (nothing was mutated).
	got2, err := f.svc.RevealToken(ctx, f.rc, f.policy, authz, r2.TokenID)
	if err != nil {
		t.Fatalf("RevealToken(2): %v", err)
	}
	if got2 != "TWO" {
		t.Errorf("token 2 revealed = %q, want %q", got2, "TWO")
	}

	stored := readContent(t, f.db, polyRID)
	if want := obfuscationToken(r2.TokenID); !contains(stored, want) {
		t.Errorf("stored content %q lost token 2 placeholder %q", stored, want)
	}
}

// TestRevealTokenDeniedWithoutPermission verifies a caller lacking
// Reveal_Permission for the Sphere is denied with ErrRevealDenied and no
// plaintext is produced (Req 16.5).
func TestRevealTokenDeniedWithoutPermission(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	tokenID, polyRID := encryptOneSpan(t, f, "keep SENSITIVE hidden", "SENSITIVE")
	sphere := sphereIDOf(t, f, polyRID)

	// Grant read but NOT reveal on the owning Sphere.
	plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, revealAuthz(sphere, false), tokenID)
	if err != ErrRevealDenied {
		t.Fatalf("error = %v, want ErrRevealDenied", err)
	}
	if plain != "" {
		t.Errorf("denied reveal returned plaintext %q, want empty", plain)
	}
}

// TestRevealTokenDeniedForDifferentSpherePermission verifies reveal is strictly
// per-Sphere: reveal permission on some OTHER Sphere does not permit revealing a
// token whose owning Polygon lives in this Sphere (Req 16.6).
func TestRevealTokenDeniedForDifferentSpherePermission(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	tokenID, polyRID := encryptOneSpan(t, f, "x SECRET y", "SECRET")
	owningSphere := sphereIDOf(t, f, polyRID)

	// Grant reveal on a DIFFERENT sphere id only.
	otherSphere := owningSphere + 999
	authz := revealAuthz(otherSphere, true)

	plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, authz, tokenID)
	if err != ErrRevealDenied {
		t.Fatalf("error = %v, want ErrRevealDenied (reveal is per-Sphere)", err)
	}
	if plain != "" {
		t.Errorf("cross-sphere reveal returned plaintext %q, want empty", plain)
	}
}

// TestRevealTokenMissingCiphertextFails verifies an unknown token id keeps the
// content masked and returns ErrRevealFailed with no partial plaintext
// (Req 16.8).
func TestRevealTokenMissingCiphertextFails(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	// A well-formed but unstored token id, with a permissive authz so the
	// failure is attributable to the missing ciphertext, not permission.
	authz := AuthzData{Admin: true}
	plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, authz, recordid.New().Canonical())
	if err != ErrRevealFailed {
		t.Fatalf("error = %v, want ErrRevealFailed", err)
	}
	if plain != "" {
		t.Errorf("missing-ciphertext reveal returned %q, want empty", plain)
	}
}

// TestRevealTokenUndecryptableCiphertextFails verifies a stored-but-corrupt
// ciphertext (GCM authentication failure) keeps content masked and returns
// ErrRevealFailed with no partial plaintext (Req 16.8). The permission check is
// admin-permissive so the failure is attributable to decryption.
func TestRevealTokenUndecryptableCiphertextFails(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	tokenID, _ := encryptOneSpan(t, f, "a SECRET b", "SECRET")

	// Corrupt the stored ciphertext so GCM authentication fails on Open.
	if _, err := f.db.Exec(
		`UPDATE ciphertext SET ciphertext = ? WHERE token_id = ?`,
		[]byte("this-is-not-valid-gcm-ciphertext"), tokenID,
	); err != nil {
		t.Fatalf("corrupt ciphertext: %v", err)
	}

	plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, AuthzData{Admin: true}, tokenID)
	if err != ErrRevealFailed {
		t.Fatalf("error = %v, want ErrRevealFailed", err)
	}
	if plain != "" {
		t.Errorf("undecryptable reveal returned %q, want empty", plain)
	}
}

// TestRevealTokenAdminCanReveal verifies an Admin_Group member is permitted to
// reveal via the implicit per-Sphere reveal grant (Req 4.7 flowing into 16.4).
func TestRevealTokenAdminCanReveal(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	tokenID, _ := encryptOneSpan(t, f, "p SECRET q", "SECRET")

	plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, AuthzData{Admin: true}, tokenID)
	if err != nil {
		t.Fatalf("RevealToken(admin): %v", err)
	}
	if plain != "SECRET" {
		t.Errorf("admin revealed = %q, want %q", plain, "SECRET")
	}
}

// TestRevealTokenDeactivatedDenied verifies a deactivated user is denied reveal
// even with an otherwise-granting authz (Req 5.4 flowing into 16.5).
func TestRevealTokenDeactivatedDenied(t *testing.T) {
	f := newRevealFixture(t)
	ctx := context.Background()

	tokenID, polyRID := encryptOneSpan(t, f, "m SECRET n", "SECRET")
	sphere := sphereIDOf(t, f, polyRID)

	authz := revealAuthz(sphere, true)
	authz.Deactivated = true

	plain, err := f.svc.RevealToken(ctx, f.rc, f.policy, authz, tokenID)
	if err != ErrRevealDenied {
		t.Fatalf("error = %v, want ErrRevealDenied for deactivated user", err)
	}
	if plain != "" {
		t.Errorf("deactivated reveal returned %q, want empty", plain)
	}
}

// contains is a tiny substring helper kept local to avoid pulling strings into
// the test's import set alongside the fixture helpers.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
