package service

import (
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestValidatePasswordAcceptsPolicyCompliant asserts a password that meets the
// length and basic-strength policy is accepted (Req 31.2, 32.2).
func TestValidatePasswordAcceptsPolicyCompliant(t *testing.T) {
	valid := []string{
		"correctHorse1",        // lower + upper + digit, 13 chars
		"aaaaaaaaaaaA",         // lower + upper, exactly 12 chars (min boundary)
		"passphrase-with-dash", // lower + symbol
		"UPPER lower 9",        // three classes incl. space as "other"
		"пароль-Пароль1",       // multi-byte runes, mixed classes
	}
	for _, pw := range valid {
		if err := ValidatePassword(pw); err != nil {
			t.Errorf("ValidatePassword(%q) = %v, want nil", pw, err)
		}
	}
}

// TestValidatePasswordRejectsTooShort asserts a password shorter than the
// minimum length is rejected with ErrPasswordPolicy and mutates nothing (the
// caller creates no User — Req 31.5, 32.2).
func TestValidatePasswordRejectsTooShort(t *testing.T) {
	// 11 chars, spans two classes but is one under the 12-char minimum.
	err := ValidatePassword("Abcdefghij1")
	if !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("ValidatePassword(too short) = %v, want ErrPasswordPolicy", err)
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Errorf("error message %q should name the minimum-length rule", err.Error())
	}
}

// TestValidatePasswordRejectsEmpty asserts the empty password is rejected.
func TestValidatePasswordRejectsEmpty(t *testing.T) {
	if err := ValidatePassword(""); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("ValidatePassword(\"\") = %v, want ErrPasswordPolicy", err)
	}
}

// TestValidatePasswordRejectsTooLong asserts a password over the maximum length
// is rejected, guarding the expensive hash from unbounded input.
func TestValidatePasswordRejectsTooLong(t *testing.T) {
	// 129 chars (one over the 128 max), mixed classes so only length can fail.
	pw := strings.Repeat("aB1", 43) // 129 runes
	err := ValidatePassword(pw)
	if !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("ValidatePassword(too long) = %v, want ErrPasswordPolicy", err)
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("error message %q should name the maximum-length rule", err.Error())
	}
}

// TestValidatePasswordRejectsSingleClass asserts a long-enough password drawn
// from a single character class fails the basic strength check.
func TestValidatePasswordRejectsSingleClass(t *testing.T) {
	// 16 lowercase-only chars: long enough, but only one class.
	err := ValidatePassword("aaaaaaaaaaaaaaaa")
	if !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("ValidatePassword(single class) = %v, want ErrPasswordPolicy", err)
	}
	if !strings.Contains(err.Error(), "character types") {
		t.Errorf("error message %q should name the strength rule", err.Error())
	}
}

// TestValidatePasswordLengthCountsRunes asserts length is measured in runes, not
// bytes: a 12-rune multi-byte password is accepted even though it exceeds 12
// bytes.
func TestValidatePasswordLengthCountsRunes(t *testing.T) {
	// 11 multi-byte lowercase runes + one uppercase = 12 runes, two classes.
	pw := "αααααααααααА"
	if got := len([]rune(pw)); got != 12 {
		t.Fatalf("test fixture has %d runes, want 12", got)
	}
	if err := ValidatePassword(pw); err != nil {
		t.Errorf("ValidatePassword(%q) = %v, want nil (length is rune-counted)", pw, err)
	}
}

// TestValidatePasswordPolicyProperty is a property check over the validator's
// contract: a password is accepted if and only if it is within the length
// bounds AND draws from at least the required number of character classes. It
// reconstructs the expected verdict independently of the implementation so the
// two must agree for every generated input.
func TestValidatePasswordPolicyProperty(t *testing.T) {
	// A rune alphabet that spans all four classes plus a multi-byte letter, so
	// generated strings exercise the class-counting and rune-length logic.
	alphabet := []rune("abYZ09-_ π")

	rapid.Check(t, func(rt *rapid.T) {
		runes := rapid.SliceOfN(rapid.SampledFrom(alphabet), 0, 140).Draw(rt, "runes")
		pw := string(runes)

		n := len([]rune(pw))
		var hasLower, hasUpper, hasDigit, hasOther bool
		for _, r := range pw {
			switch {
			case r >= 'a' && r <= 'z', r == 'π':
				hasLower = true
			case r >= 'A' && r <= 'Z':
				hasUpper = true
			case r >= '0' && r <= '9':
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

		wantOK := n >= passwordMinLen && n <= passwordMaxLen && classes >= passwordMinClasses
		gotErr := ValidatePassword(pw)
		gotOK := gotErr == nil

		if gotOK != wantOK {
			rt.Fatalf("ValidatePassword(%q): got ok=%v (err=%v), want ok=%v (len=%d classes=%d)",
				pw, gotOK, gotErr, wantOK, n, classes)
		}
		if !gotOK && !errors.Is(gotErr, ErrPasswordPolicy) {
			rt.Fatalf("rejection error %v does not wrap ErrPasswordPolicy", gotErr)
		}
	})
}
