package ws

import (
	"context"

	"github.com/influence/influence/internal/data"
)

// Mode is the concurrent-editing mode a Polygon operates in, mirroring
// POLYGON.edit_mode (Req 25.1). Every Polygon is in exactly one mode; the two
// are mutually exclusive and never mixed for the same Polygon. The hub uses the
// mode to decide which concurrency path a room follows.
type Mode string

const (
	// ModeLocking is record-locking mode (Req 25.3–25.5).
	ModeLocking Mode = "locking"
	// ModeCollaborative is collaborative CRDT mode (Req 25.2).
	ModeCollaborative Mode = "collaborative"
)

// Valid reports whether m is one of the two supported modes (Req 25.1).
func (m Mode) Valid() bool {
	return m == ModeLocking || m == ModeCollaborative
}

// ModeResolver reports the single edit mode of a Polygon within the caller's
// tenant (Req 25.1). The hub depends only on this narrow interface — not on the
// repository or the database — so room-mode decisions stay decoupled from
// storage and easy to test. The Polygon is addressed by its Record_ID and
// resolved within rc's tenant, so a Polygon in another tenant is not resolvable
// (Req 1.6).
type ModeResolver interface {
	Mode(ctx context.Context, rc data.RequestContext, polygonRecordID string) (Mode, error)
}

// ResolveMode returns the mode of the Polygon addressed by key.PolygonRecordID,
// resolved within rc's tenant via the given resolver. It is the hub's single
// entry point for "which mode is this Polygon in", so a room is always driven
// by exactly one mode (Req 25.1).
//
// The key's tenant is expected to match rc's tenant; ResolveMode always uses
// rc's tenant for the lookup, which is the authenticated one, so a mismatched
// key can never reach another tenant's data.
func ResolveMode(ctx context.Context, resolver ModeResolver, rc data.RequestContext, key RoomKey) (Mode, error) {
	return resolver.Mode(ctx, rc, key.PolygonRecordID)
}
