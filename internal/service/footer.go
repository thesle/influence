package service

// Metadata footer (design § Metadata footer, Requirements 18.1, 18.2, 18.3).
//
// A Polygon carries its provenance in three stored fields, all recorded by the
// tenant-scoped PolygonRepo: author_user_id (the authoring User's Central id)
// and created_at, both set at Create (Req 18.1), and updated_at, refreshed on
// every edit via PolygonRepo.Touch (Req 18.2). Those fields are stored as ISO
// 8601 timestamps in UTC.
//
// This file owns the render side of that provenance: given a Polygon's stored
// author id and its creation/last-edit timestamps, it builds the footer the
// Rich_Renderer appends to a rendered Polygon (Req 18.3). Two things happen
// here that cannot happen in the tenant DB:
//
//   - The author is resolved to a *display name*, not just an id. Identity is
//     platform-wide and lives in the Central_Directory USER table, so the
//     display name is looked up through conns.Central() at render time — the
//     same late-resolution rule that keeps links pointing at current names
//     (design § "Stable identity with mutable names"). Content metadata never
//     stores the display name; it stores only the id.
//
//   - Each timestamp is reduced to an ISO 8601 *date* in UTC (YYYY-MM-DD). The
//     stored values are already UTC, so the footer parses them and re-emits the
//     date portion, leaving the footer stable regardless of the exact stored
//     time-of-day precision (Req 18.3).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/influence/influence/internal/data"
)

// isoDateLayout is the ISO 8601 calendar-date layout (YYYY-MM-DD) used for the
// footer's creation and last-edit dates (Req 18.3).
const isoDateLayout = "2006-01-02"

// MetadataFooter is the rendered provenance of a Polygon: the authoring User's
// display name (resolved from the Central_Directory) together with the creation
// and last-edit dates, each an ISO 8601 date in UTC (Req 18.3).
type MetadataFooter struct {
	// AuthorDisplayName is the authoring User's display name resolved from the
	// Central_Directory at render time (Req 18.3). When the author id cannot be
	// resolved (e.g. the User no longer exists), it falls back to a stable
	// placeholder rather than failing the whole render.
	AuthorDisplayName string
	// CreatedDate is the Polygon's creation date as an ISO 8601 date in UTC
	// (Req 18.1, 18.3).
	CreatedDate string
	// LastEditDate is the Polygon's last-edit date as an ISO 8601 date in UTC
	// (Req 18.2, 18.3).
	LastEditDate string
}

// unknownAuthorDisplayName is the placeholder used when an author id resolves to
// no Central_Directory USER (deleted or never present). It keeps the footer
// renderable rather than turning a missing author into a render failure.
const unknownAuthorDisplayName = "Unknown author"

// MetadataFooterBuilder builds the metadata footer for a rendered Polygon. It
// resolves author display names through the shared Central_Directory handle;
// it never touches tenant content (the timestamps are supplied by the caller,
// which read them from the tenant-scoped PolygonRepo).
type MetadataFooterBuilder struct {
	central *sql.DB
}

// NewMetadataFooterBuilder constructs a MetadataFooterBuilder over a
// ConnManager, taking the shared Central_Directory handle used to resolve
// author display names.
func NewMetadataFooterBuilder(conns data.ConnManager) *MetadataFooterBuilder {
	return &MetadataFooterBuilder{central: conns.Central()}
}

// Build assembles the footer for a Polygon authored by authorUserID and created
// at createdAt / last edited at updatedAt (both ISO 8601 timestamps in UTC as
// stored by PolygonRepo). It resolves the author's display name from the
// Central_Directory (Req 18.3) and reduces each timestamp to an ISO 8601 date
// in UTC (Req 18.1, 18.2, 18.3).
//
// A missing author (no matching USER) resolves to a stable placeholder so the
// footer still renders; any other Central_Directory error is returned. A
// timestamp that cannot be parsed is surfaced as an error rather than silently
// producing a blank or misleading date.
func (b *MetadataFooterBuilder) Build(ctx context.Context, authorUserID int64, createdAt, updatedAt string) (MetadataFooter, error) {
	name, err := b.authorDisplayName(ctx, authorUserID)
	if err != nil {
		return MetadataFooter{}, err
	}

	created, err := isoDateUTC(createdAt)
	if err != nil {
		return MetadataFooter{}, fmt.Errorf("creation date: %w", err)
	}
	edited, err := isoDateUTC(updatedAt)
	if err != nil {
		return MetadataFooter{}, fmt.Errorf("last-edit date: %w", err)
	}

	return MetadataFooter{
		AuthorDisplayName: name,
		CreatedDate:       created,
		LastEditDate:      edited,
	}, nil
}

// authorDisplayName resolves a Central_Directory USER id to its display name.
// Identity is platform-wide, so this reads the shared Central handle, never a
// tenant DB. A missing user yields the unknown-author placeholder so a deleted
// author does not break rendering.
func (b *MetadataFooterBuilder) authorDisplayName(ctx context.Context, authorUserID int64) (string, error) {
	var name string
	err := b.central.QueryRowContext(ctx,
		`SELECT display_name FROM "USER" WHERE id = ?`, authorUserID,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return unknownAuthorDisplayName, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve author display name: %w", err)
	}
	return name, nil
}

// isoDateUTC parses a stored ISO 8601 timestamp (RFC 3339, already UTC) and
// re-emits just its calendar date in UTC as an ISO 8601 date (YYYY-MM-DD),
// per Req 18.3.
func isoDateUTC(ts string) (string, error) {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "", fmt.Errorf("parse ISO 8601 timestamp %q: %w", ts, err)
	}
	return t.UTC().Format(isoDateLayout), nil
}
