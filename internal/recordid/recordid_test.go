package recordid

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"
)

func TestNewProducesDistinctIDs(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := New()
		c := id.Canonical()
		if _, dup := seen[c]; dup {
			t.Fatalf("New() produced a duplicate canonical id %q", c)
		}
		seen[c] = struct{}{}
	}
}

func TestNewIsUUIDv4(t *testing.T) {
	id := New()
	if got := id.UUID().Version(); got != 4 {
		t.Fatalf("expected UUID version 4, got %d", got)
	}
}

func TestCanonicalIsHyphenatedUUID(t *testing.T) {
	id := New()
	c := id.Canonical()
	if len(c) != 36 {
		t.Fatalf("canonical form %q has length %d, want 36", c, len(c))
	}
	if _, err := uuid.Parse(c); err != nil {
		t.Fatalf("canonical form %q is not a valid UUID: %v", c, err)
	}
}

func TestShortHasFixedWidthAndAlphabet(t *testing.T) {
	for i := 0; i < 1000; i++ {
		s := New().Short()
		if len(s) != shortLen {
			t.Fatalf("short form %q has length %d, want %d", s, len(s), shortLen)
		}
		for _, r := range s {
			if !strings.ContainsRune(alphabet, r) {
				t.Fatalf("short form %q contains out-of-alphabet char %q", s, r)
			}
		}
	}
}

func TestShortRoundTrips(t *testing.T) {
	for i := 0; i < 1000; i++ {
		id := New()
		got, err := ParseShort(id.Short())
		if err != nil {
			t.Fatalf("ParseShort(%q): %v", id.Short(), err)
		}
		if got.Canonical() != id.Canonical() {
			t.Fatalf("round-trip mismatch: got %q, want %q", got.Canonical(), id.Canonical())
		}
	}
}

func TestCanonicalRoundTrips(t *testing.T) {
	id := New()
	got, err := Parse(id.Canonical())
	if err != nil {
		t.Fatalf("Parse(%q): %v", id.Canonical(), err)
	}
	if got != id {
		t.Fatalf("Parse round-trip mismatch: got %v, want %v", got, id)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse("not-a-uuid"); err == nil {
		t.Fatal("Parse accepted an invalid canonical string")
	}
}

func TestParseShortErrors(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"ambiguous char 0": "0000000000000000000000",
		"ambiguous char l": "llllllllllllllllllllll",
		"overflow":         strings.Repeat("z", 30),
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseShort(s); err == nil {
				t.Fatalf("ParseShort(%q) accepted an invalid short id", s)
			}
		})
	}
}

func TestZeroUUIDShortForm(t *testing.T) {
	// The all-zero UUID must still encode to a full-width, decodable short id.
	var zero ID
	s := zero.Short()
	if len(s) != shortLen {
		t.Fatalf("zero short form %q has length %d, want %d", s, len(s), shortLen)
	}
	got, err := ParseShort(s)
	if err != nil {
		t.Fatalf("ParseShort(%q): %v", s, err)
	}
	if got != zero {
		t.Fatalf("zero round-trip mismatch: got %v, want zero", got)
	}
}

// Feature: influence, Property 9: Record_ID uniqueness and immutability
//
// Property 9 (design.md): "For all sequences of record creations and subsequent
// edits/renames, every assigned Record_ID is unique among records of the same
// type in the tenant and never changes for the lifetime of the record it
// identifies."
//
// This package is the mint for Record_IDs: New() is the only way to create one
// and there is deliberately no mutation API. So at this layer the property has
// two observable halves that we can exercise directly:
//
//   - Uniqueness: across many independent draws, no two minted IDs collide (on
//     the canonical form, the short form, or the raw UUID bytes). The tenant
//     schema's UNIQUE record_id column enforces per-type uniqueness at the
//     storage layer; here we assert the generator makes collisions negligible.
//   - Immutability: an ID never changes. Since there is no setter, "unchanging"
//     means an ID's encodings are stable — the same ID always renders to the
//     same canonical and short forms, and both forms round-trip back to the same
//     ID no matter how many times they are re-read.
//
// Validates: Requirements 15.4, 15.5
func TestRecordIDUniquenessAndImmutabilityProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Draw a batch of IDs to mint in this run. Minimum 100 iterations is
		// satisfied by rapid's default check count; each check mints a batch so
		// uniqueness is exercised well beyond 100 IDs overall.
		n := rapid.IntRange(2, 64).Draw(rt, "count")

		seenCanonical := make(map[string]struct{}, n)
		seenShort := make(map[string]struct{}, n)
		seenUUID := make(map[uuid.UUID]struct{}, n)

		for i := 0; i < n; i++ {
			id := New()

			// --- Uniqueness (Req 15.4): no collisions across draws. ---
			c := id.Canonical()
			s := id.Short()
			u := id.UUID()

			if _, dup := seenCanonical[c]; dup {
				rt.Fatalf("duplicate canonical Record_ID minted: %q", c)
			}
			if _, dup := seenShort[s]; dup {
				rt.Fatalf("duplicate short Record_ID minted: %q", s)
			}
			if _, dup := seenUUID[u]; dup {
				rt.Fatalf("duplicate underlying UUID minted: %v", u)
			}
			seenCanonical[c] = struct{}{}
			seenShort[s] = struct{}{}
			seenUUID[u] = struct{}{}

			// --- Immutability (Req 15.5): encodings are stable and the ID's
			// two external forms round-trip back to the same ID. Re-render and
			// re-parse several times; the value must never drift. ---
			for rep := 0; rep < 3; rep++ {
				if got := id.Canonical(); got != c {
					rt.Fatalf("canonical form changed across renders: got %q, want %q", got, c)
				}
				if got := id.Short(); got != s {
					rt.Fatalf("short form changed across renders: got %q, want %q", got, s)
				}

				fromCanonical, err := Parse(c)
				if err != nil {
					rt.Fatalf("Parse(%q): %v", c, err)
				}
				if fromCanonical != id {
					rt.Fatalf("canonical round-trip changed the ID: got %v, want %v", fromCanonical, id)
				}

				fromShort, err := ParseShort(s)
				if err != nil {
					rt.Fatalf("ParseShort(%q): %v", s, err)
				}
				if fromShort != id {
					rt.Fatalf("short round-trip changed the ID: got %v, want %v", fromShort, id)
				}
			}
		}
	})
}
