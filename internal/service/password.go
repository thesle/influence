package service

// Password hashing for Influence uses Argon2id, the memory-hard, side-channel
// resistant password hashing function recommended for new applications
// (Requirement 3.4 — credentials are stored using a one-way password hash).
//
// A stored credential is a self-describing, PHC-style encoded string that
// carries the algorithm, its version, the cost parameters, the per-password
// random salt, and the derived key:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<base64 salt>$<base64 hash>
//
// Encoding the parameters and salt alongside the hash means verification never
// needs to consult an external parameter store, and the cost parameters can be
// raised over time without invalidating existing hashes: an older hash verifies
// against the parameters embedded in it. Verification recomputes the derived
// key from the presented password using the stored salt/params and compares in
// constant time, so it is one-way — the plaintext is never recoverable from the
// stored form.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/argon2"
)

// The password policy is defined once here and reused by first-run bootstrap,
// admin user-creation, and any future self-service password change, so
// Requirements 31.2, 31.5, and 32.2 all share a single validator rather than
// each re-deriving the rules (design.md — "The password policy is defined once
// (minimum length and basic strength) and reused ...").
//
// The policy is intentionally modest: a minimum length plus a basic strength
// check (a mix of character classes). Length is counted in runes, not bytes, so
// a passphrase of multi-byte characters is measured by how many characters the
// user actually typed. A password that fails the policy is rejected with
// ErrPasswordPolicy, which the REST layer maps to the standard VALIDATION error
// envelope; the message names the specific rule that failed so a caller can
// surface it verbatim.

const (
	// passwordMinLen is the inclusive minimum password length in runes. Twelve
	// characters is a common floor for a length-first policy and pairs well with
	// the basic strength requirement below.
	passwordMinLen = 12
	// passwordMaxLen is the inclusive maximum password length in runes. An upper
	// bound guards the (deliberately expensive) Argon2id hash from unbounded
	// input while staying far above any realistic passphrase.
	passwordMaxLen = 128
	// passwordMinClasses is the number of distinct character classes (lower,
	// upper, digit, symbol) a password must draw from to satisfy the basic
	// strength check.
	passwordMinClasses = 2
)

// ErrPasswordPolicy is returned by ValidatePassword when a password does not
// meet the platform password policy. It is a VALIDATION-class error: the REST
// layer maps it to a 400 VALIDATION envelope (Req 31.5, 32.2). The wrapped
// message names the specific rule that failed.
var ErrPasswordPolicy = errors.New("password does not meet policy")

// ValidatePassword reports whether password satisfies the platform password
// policy. It returns nil when the password is acceptable and an error wrapping
// ErrPasswordPolicy, describing the first rule that failed, otherwise.
//
// This is the single shared entry point for password policy enforcement: the
// first-run bootstrap (Req 31.2, 31.5), admin user-creation (Req 32.2), and any
// future self-service password change call it so the rules stay in one place.
// It performs only policy validation and never hashes — callers pass an
// accepted password to HashPassword separately.
//
// The policy is:
//   - length between passwordMinLen and passwordMaxLen runes (inclusive), and
//   - at least passwordMinClasses distinct character classes among lowercase,
//     uppercase, digit, and other (symbol/punctuation/space) characters.
func ValidatePassword(password string) error {
	n := len([]rune(password))
	if n < passwordMinLen {
		return fmt.Errorf("%w: password must be at least %d characters", ErrPasswordPolicy, passwordMinLen)
	}
	if n > passwordMaxLen {
		return fmt.Errorf("%w: password must be at most %d characters", ErrPasswordPolicy, passwordMaxLen)
	}

	var hasLower, hasUpper, hasDigit, hasOther bool
	for _, r := range password {
		switch {
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsDigit(r):
			hasDigit = true
		default:
			hasOther = true
		}
	}

	classes := 0
	for _, present := range []bool{hasLower, hasUpper, hasDigit, hasOther} {
		if present {
			classes++
		}
	}
	if classes < passwordMinClasses {
		return fmt.Errorf(
			"%w: password must include at least %d different character types (lowercase, uppercase, digit, or symbol)",
			ErrPasswordPolicy, passwordMinClasses,
		)
	}

	return nil
}

// argon2Params are the Argon2id cost parameters used when hashing a new
// password. They are embedded in every encoded hash so a stored credential
// remains verifiable even if these defaults change later.
type argon2Params struct {
	// memory is the amount of memory used by the algorithm, in KiB.
	memory uint32
	// time is the number of passes over the memory.
	time uint32
	// parallelism is the number of lanes (threads) used.
	parallelism uint8
	// saltLen is the length in bytes of the random salt generated per password.
	saltLen uint32
	// keyLen is the length in bytes of the derived key (the hash output).
	keyLen uint32
}

// defaultArgon2Params are sensible interactive-login defaults: 64 MiB of
// memory, 3 iterations, and 2 lanes. These meet current OWASP guidance for
// Argon2id and can be raised over time without breaking existing hashes.
var defaultArgon2Params = argon2Params{
	memory:      64 * 1024,
	time:        3,
	parallelism: 2,
	saltLen:     16,
	keyLen:      32,
}

// ErrInvalidHash is returned when a stored credential is not a well-formed
// Argon2id PHC-style encoded hash. It signals a corrupt or foreign credential
// rather than a wrong password.
var ErrInvalidHash = errors.New("invalid argon2id hash format")

// ErrIncompatibleVersion is returned when a stored hash was produced by a
// different Argon2 version than this build supports.
var ErrIncompatibleVersion = errors.New("incompatible argon2 version")

// HashPassword derives an Argon2id hash of the password using the default cost
// parameters and a fresh random salt, and returns it in the PHC-style encoded
// form described in this file. The returned string is what is stored in
// USER.password_hash (Requirement 3.4).
func HashPassword(password string) (string, error) {
	return hashPasswordWith(password, defaultArgon2Params)
}

// hashPasswordWith is HashPassword with explicit parameters, factored out so
// tests can exercise alternate cost settings deterministically.
func hashPasswordWith(password string, p argon2Params) (string, error) {
	salt := make([]byte, p.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.parallelism, p.keyLen)

	b64 := base64.RawStdEncoding
	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		p.memory, p.time, p.parallelism,
		b64.EncodeToString(salt),
		b64.EncodeToString(key),
	)
	return encoded, nil
}

// VerifyPassword reports whether password matches the given PHC-style encoded
// Argon2id hash. It recomputes the derived key using the salt and parameters
// embedded in the hash and compares in constant time. A false result with a nil
// error means the password simply does not match; a non-nil error means the
// stored hash could not be parsed and no comparison was possible.
func VerifyPassword(password, encoded string) (bool, error) {
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	computed := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.parallelism, p.keyLen)

	// Constant-time comparison prevents leaking match progress via timing.
	if subtle.ConstantTimeCompare(computed, key) == 1 {
		return true, nil
	}
	return false, nil
}

// decodeHash parses a PHC-style Argon2id encoded hash into its parameters,
// salt, and derived key. It rejects any string that is not exactly the format
// HashPassword produces.
func decodeHash(encoded string) (argon2Params, []byte, []byte, error) {
	var p argon2Params

	// Format: $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return p, nil, nil, ErrIncompatibleVersion
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.parallelism); err != nil {
		return p, nil, nil, ErrInvalidHash
	}

	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, ErrInvalidHash
	}

	p.saltLen = uint32(len(salt))
	p.keyLen = uint32(len(key))
	return p, salt, key, nil
}
