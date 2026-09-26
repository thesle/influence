package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// TestFooterContentProperty is the property test for the rendered metadata
// footer's content (Requirements 18.1, 18.2, 18.3).
//
// Feature: influence, Property 23: Metadata footer content
//
// Property 23 (from design.md): For all created and subsequently edited
// Polygons, the rendered footer contains the authoring user's display name, the
// creation date, and the last-edit date, each formatted as an ISO 8601 date in
// UTC.
//
// The test seeds a real Central_Directory USER with an arbitrary display name
// and drives arbitrary valid ISO 8601 timestamps (across arbitrary instants and
// arbitrary zone offsets) through the real MetadataFooterBuilder. It asserts
// Build resolves exactly the seeded display name and reduces each timestamp to
// its ISO 8601 calendar date in UTC (YYYY-MM-DD) — the date derived from the
// instant's UTC representation, which is what the footer must show regardless of
// the stored offset.
//
// Validates: Requirements 18.1, 18.2, 18.3
func TestFooterContentProperty(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	b := NewMetadataFooterBuilder(conns)
	ctx := context.Background()

	// A broad alphabet so display names exercise letters, digits, spaces, and
	// punctuation. Display names are stored and resolved verbatim, so any
	// non-empty value must round-trip through the footer unchanged.
	const alphabet = "abcABC 012 .,'-_|é—"

	// A per-iteration counter keeps each seeded username unique, independent of
	// the arbitrary (and possibly repeated) drawn values.
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		displayName := rapid.StringOfN(rapid.SampledFrom([]rune(alphabet)), 1, 40, -1).Draw(rt, "displayName")

		// Draw two arbitrary instants, each as (unix seconds, zone offset). The
		// instant spans a wide range around the epoch; the offset ranges across
		// the real-world UTC offset band. Formatting with a non-UTC offset lets
		// the property exercise the footer's UTC normalization, while the
		// expected date is always derived from the instant's UTC representation.
		createdSec := rapid.Int64Range(0, 4_102_444_800).Draw(rt, "createdSec") // 1970..2100
		updatedSec := rapid.Int64Range(0, 4_102_444_800).Draw(rt, "updatedSec")
		// Offsets are drawn in whole minutes because RFC 3339 only represents
		// zone offsets at minute granularity; a sub-minute offset would not
		// survive formatting and would misrepresent the instant.
		createdOff := rapid.IntRange(-12*60, 14*60).Draw(rt, "createdOffMin") * 60
		updatedOff := rapid.IntRange(-12*60, 14*60).Draw(rt, "updatedOffMin") * 60

		createdTS, wantCreated := iso8601AtOffset(createdSec, createdOff)
		updatedTS, wantUpdated := iso8601AtOffset(updatedSec, updatedOff)

		// Each iteration seeds a fresh USER with a unique username so the row is
		// insertable regardless of the arbitrary (possibly duplicate) display
		// name; the display name is what the footer must resolve.
		username := fmt.Sprintf("author-%d", iter)
		authorID := seedUserWithDisplayName(t, central, tenantID, username, displayName)

		footer, err := b.Build(ctx, authorID, createdTS, updatedTS)
		if err != nil {
			rt.Fatalf("Build(author=%d, created=%q, updated=%q) error = %v", authorID, createdTS, updatedTS, err)
		}

		if footer.AuthorDisplayName != displayName {
			rt.Fatalf("AuthorDisplayName = %q, want resolved display name %q", footer.AuthorDisplayName, displayName)
		}
		if footer.CreatedDate != wantCreated {
			rt.Fatalf("CreatedDate = %q, want %q (UTC date of %q)", footer.CreatedDate, wantCreated, createdTS)
		}
		if footer.LastEditDate != wantUpdated {
			rt.Fatalf("LastEditDate = %q, want %q (UTC date of %q)", footer.LastEditDate, wantUpdated, updatedTS)
		}
	})
}

// iso8601AtOffset renders the instant at unixSec in a fixed zone of offsetSec
// seconds east of UTC as an ISO 8601 (RFC 3339) timestamp, and returns it
// alongside the ISO 8601 calendar date of that same instant in UTC. The footer
// must always report the UTC date, so the returned date is derived from the
// instant's UTC representation regardless of the rendered offset.
func iso8601AtOffset(unixSec int64, offsetSec int) (timestamp, utcDate string) {
	instant := time.Unix(unixSec, 0)
	zone := time.FixedZone("gen", offsetSec)
	timestamp = instant.In(zone).Format(time.RFC3339)
	utcDate = instant.UTC().Format(isoDateLayout)
	return timestamp, utcDate
}
