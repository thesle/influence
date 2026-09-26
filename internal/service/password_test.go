package service

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestHashPasswordVerifies asserts a password hashed with the default
// parameters verifies against itself (Requirement 3.4).
func TestHashPasswordVerifies(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("correct password should verify")
	}
}

// TestVerifyRejectsWrongPassword asserts a mismatching password does not verify
// and returns no error (a plain non-match, not a parse failure).
func TestVerifyRejectsWrongPassword(t *testing.T) {
	hash, err := HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := VerifyPassword("not-the-password", hash)
	if err != nil {
		t.Fatalf("verify returned error for non-match: %v", err)
	}
	if ok {
		t.Fatal("wrong password should not verify")
	}
}

// TestHashIsOneWayEncoded asserts the encoded hash is the expected PHC-style
// Argon2id form and does not contain the plaintext (one-way storage, Req 3.4).
func TestHashIsOneWayEncoded(t *testing.T) {
	const pw = "unique-plaintext-value-123"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=") {
		t.Fatalf("unexpected hash format: %q", hash)
	}
	if strings.Contains(hash, pw) {
		t.Fatal("encoded hash must not contain the plaintext password")
	}
}

// TestSaltIsRandomPerHash asserts two hashes of the same password differ,
// proving a fresh random salt is used each time.
func TestSaltIsRandomPerHash(t *testing.T) {
	a, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	b, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if a == b {
		t.Fatal("hashing the same password twice must yield different encoded hashes (random salt)")
	}
}

// TestVerifyRejectsMalformedHash asserts a non-Argon2id string is reported as a
// format error rather than silently failing to verify.
func TestVerifyRejectsMalformedHash(t *testing.T) {
	if _, err := VerifyPassword("pw", "not-a-valid-hash"); err == nil {
		t.Fatal("malformed hash should return an error")
	}
}

// TestPasswordRoundTrip is a property test asserting that for any password, the
// hash verifies against the same password and rejects a different one. Uses the
// project's chosen property library (pgregory.net/rapid); a low iteration count
// keeps runtime reasonable since Argon2id is intentionally expensive.
func TestPasswordRoundTrip(t *testing.T) {
	// Argon2id is memory-hard and slow by design; use light params here so the
	// property can run many iterations without the default 64 MiB cost.
	fast := argon2Params{memory: 8 * 1024, time: 1, parallelism: 1, saltLen: 16, keyLen: 32}

	rapid.Check(t, func(rt *rapid.T) {
		pw := rapid.String().Draw(rt, "password")

		hash, err := hashPasswordWith(pw, fast)
		if err != nil {
			rt.Fatalf("hash: %v", err)
		}
		ok, err := VerifyPassword(pw, hash)
		if err != nil {
			rt.Fatalf("verify same: %v", err)
		}
		if !ok {
			rt.Fatalf("password %q should verify against its own hash", pw)
		}

		// A different password must not verify. Guarantee difference by appending.
		ok, err = VerifyPassword(pw+"x", hash)
		if err != nil {
			rt.Fatalf("verify different: %v", err)
		}
		if ok {
			rt.Fatalf("a different password must not verify against hash of %q", pw)
		}
	})
}
