package service

import (
	"context"
	"database/sql"
	"testing"
)

// seedUserWithDisplayName inserts a Central USER with an explicit display_name
// (distinct from username) and returns its id, so footer tests can assert the
// footer shows the resolved display name rather than the login name.
func seedUserWithDisplayName(t *testing.T, central *sql.DB, tenantID int64, username, displayName string) int64 {
	t.Helper()
	res, err := central.Exec(
		`INSERT INTO "USER"(tenant_id, username, display_name, password_hash, created_at)
		 VALUES(?,?,?,?,?)`,
		tenantID, username, displayName, "hash", "2024-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// TestFooterResolvesDisplayNameAndISO8601Dates confirms the footer shows the
// author's resolved Central display name and both dates as ISO 8601 dates in
// UTC (Req 18.1, 18.2, 18.3).
func TestFooterResolvesDisplayNameAndISO8601Dates(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	authorID := seedUserWithDisplayName(t, central, tenantID, "alice", "Alice Anderson")

	b := NewMetadataFooterBuilder(conns)

	// Stored timestamps are full ISO 8601 UTC timestamps; the footer reduces
	// each to its ISO 8601 date in UTC.
	created := "2024-01-02T03:04:05Z"
	updated := "2024-06-07T08:09:10Z"

	footer, err := b.Build(context.Background(), authorID, created, updated)
	if err != nil {
		t.Fatalf("build footer: %v", err)
	}
	if footer.AuthorDisplayName != "Alice Anderson" {
		t.Errorf("author display name = %q, want %q", footer.AuthorDisplayName, "Alice Anderson")
	}
	if footer.CreatedDate != "2024-01-02" {
		t.Errorf("creation date = %q, want %q", footer.CreatedDate, "2024-01-02")
	}
	if footer.LastEditDate != "2024-06-07" {
		t.Errorf("last-edit date = %q, want %q", footer.LastEditDate, "2024-06-07")
	}
}

// TestFooterDatesNormalizedToUTC confirms a stored timestamp with a non-UTC
// offset is normalized to its UTC calendar date, so the footer date is always
// in UTC even when the wall-clock date differs by zone (Req 18.3).
func TestFooterDatesNormalizedToUTC(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	authorID := seedUserWithDisplayName(t, central, tenantID, "bob", "Bob")

	b := NewMetadataFooterBuilder(conns)

	// 2024-03-10T23:30:00-05:00 is 2024-03-11T04:30:00Z in UTC — the UTC date
	// rolls over to the 11th.
	created := "2024-03-10T23:30:00-05:00"
	footer, err := b.Build(context.Background(), authorID, created, created)
	if err != nil {
		t.Fatalf("build footer: %v", err)
	}
	if footer.CreatedDate != "2024-03-11" {
		t.Errorf("creation date = %q, want %q (UTC)", footer.CreatedDate, "2024-03-11")
	}
	if footer.LastEditDate != "2024-03-11" {
		t.Errorf("last-edit date = %q, want %q (UTC)", footer.LastEditDate, "2024-03-11")
	}
}

// TestFooterUnknownAuthorFallsBack confirms an author id with no matching USER
// resolves to a stable placeholder rather than failing the render (Req 18.3).
func TestFooterUnknownAuthorFallsBack(t *testing.T) {
	conns := newTestConns(t)

	b := NewMetadataFooterBuilder(conns)

	footer, err := b.Build(context.Background(), 9999, "2024-01-02T03:04:05Z", "2024-01-02T03:04:05Z")
	if err != nil {
		t.Fatalf("build footer: %v", err)
	}
	if footer.AuthorDisplayName != unknownAuthorDisplayName {
		t.Errorf("author display name = %q, want %q", footer.AuthorDisplayName, unknownAuthorDisplayName)
	}
	if footer.CreatedDate != "2024-01-02" {
		t.Errorf("creation date = %q, want %q", footer.CreatedDate, "2024-01-02")
	}
}

// TestFooterRejectsUnparseableTimestamp confirms a stored value that is not an
// ISO 8601 timestamp is surfaced as an error rather than a blank/misleading
// date.
func TestFooterRejectsUnparseableTimestamp(t *testing.T) {
	conns := newTestConns(t)
	central := conns.Central()
	tenantID := seedTenant(t, central, "uuid-a", "Org A")
	authorID := seedUserWithDisplayName(t, central, tenantID, "carol", "Carol")

	b := NewMetadataFooterBuilder(conns)

	if _, err := b.Build(context.Background(), authorID, "not-a-timestamp", "2024-01-02T03:04:05Z"); err == nil {
		t.Fatal("expected error for unparseable creation timestamp, got nil")
	}
}
