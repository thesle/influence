// Package recordid generates and encodes Record_IDs — the immutable external
// identifiers carried by every UI-visible record (Sphere, Facet, Polygon).
//
// A Record_ID is a UUIDv4 stored canonically (the 36-char hyphenated form) with
// a Short-UUID as its URL-facing form: the same 128 bits encoded in base57 over
// an alphabet that omits visually ambiguous characters (0/O, 1/I/l). Both forms
// describe the same underlying value, so a Short-UUID round-trips back to the
// canonical UUID it was derived from (Requirements 15.2, 15.4).
//
// IDs are assigned at creation and never mutate (Req 15.5); nothing in this
// package offers a way to change an existing ID. Uniqueness per record type
// within a tenant is enforced by the UNIQUE record_id columns in the tenant
// schema; the 122 random bits of a UUIDv4 make collisions negligible (Req 15.4).
package recordid

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/google/uuid"
)

// alphabet is the 57-character base57 set used by the Short-UUID form. It
// excludes the characters that are easily confused in a URL — 0 and O, 1 and I
// and l — so a copied or hand-typed short ID is less likely to be misread.
const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base is the radix of the Short-UUID encoding: the number of symbols in the
// alphabet.
const base = 57

// shortLen is the fixed width of a Short-UUID. base57 of 128 bits needs at most
// ceil(128 * ln2 / ln57) = 22 symbols; encodings are left-padded to this width
// so every Short-UUID has the same length regardless of leading zero-value
// symbols.
const shortLen = 22

// bigBase holds the radix as a *big.Int for the encode/decode arithmetic.
var bigBase = big.NewInt(base)

// ID is a Record_ID: a UUIDv4 together with helpers to render it in its two
// forms. The canonical form is what the database stores; the short form is what
// URLs carry (Req 15.2).
type ID struct {
	uuid uuid.UUID
}

// New generates a fresh Record_ID backed by a random UUIDv4. It is the only way
// to mint an ID; there is deliberately no setter, so an assigned ID cannot be
// mutated for the lifetime of the record it identifies (Req 15.5).
func New() ID {
	return ID{uuid: uuid.New()}
}

// Canonical returns the 36-character hyphenated UUID string stored in the
// database (Req 15.4).
func (id ID) Canonical() string {
	return id.uuid.String()
}

// Short returns the URL-facing Short-UUID: the same 128 bits encoded in base57,
// left-padded to a fixed 22-character width (Req 15.2).
func (id ID) Short() string {
	// Interpret the 16 UUID bytes as a big-endian 128-bit integer, then emit
	// base57 digits least-significant first and reverse.
	n := new(big.Int).SetBytes(id.uuid[:])
	buf := make([]byte, 0, shortLen)
	mod := new(big.Int)
	for n.Sign() > 0 {
		n.DivMod(n, bigBase, mod)
		buf = append(buf, alphabet[mod.Int64()])
	}
	// Left-pad with the zero-value symbol to the fixed width.
	for len(buf) < shortLen {
		buf = append(buf, alphabet[0])
	}
	// Reverse into most-significant-first order.
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// String returns the canonical form so an ID prints usefully in logs.
func (id ID) String() string {
	return id.Canonical()
}

// UUID exposes the underlying UUID value for callers that need the raw bytes.
func (id ID) UUID() uuid.UUID {
	return id.uuid
}

// Parse rebuilds an ID from its canonical UUID string — for example when
// reading a record_id back out of the database.
func Parse(canonical string) (ID, error) {
	u, err := uuid.Parse(canonical)
	if err != nil {
		return ID{}, fmt.Errorf("recordid: parse canonical %q: %w", canonical, err)
	}
	return ID{uuid: u}, nil
}

// ParseShort rebuilds an ID from its Short-UUID form — for example when routing
// a URL back to a record. It is the inverse of Short: for any ID id,
// ParseShort(id.Short()) yields id.
func ParseShort(short string) (ID, error) {
	if short == "" {
		return ID{}, fmt.Errorf("recordid: empty short id")
	}
	n := new(big.Int)
	for _, r := range short {
		idx := strings.IndexRune(alphabet, r)
		if idx < 0 {
			return ID{}, fmt.Errorf("recordid: invalid character %q in short id %q", r, short)
		}
		n.Mul(n, bigBase)
		n.Add(n, big.NewInt(int64(idx)))
	}
	b := n.Bytes()
	if len(b) > 16 {
		return ID{}, fmt.Errorf("recordid: short id %q overflows 128 bits", short)
	}
	var u uuid.UUID
	// Right-align the big-endian bytes into the 16-byte UUID.
	copy(u[16-len(b):], b)
	return ID{uuid: u}, nil
}
