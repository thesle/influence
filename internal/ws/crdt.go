// Influence — a self-hostable documentation platform.
// Copyright (C) 2026  Conrad Smith
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package ws

import (
	"sort"
)

// This file implements the collaborative-mode merge itself (Req 25.2, task
// 18.4): a text CRDT over which concurrent edits converge. It is the advanced,
// optional path — the minimal viable product ships record-locking only
// (Req 25.3–25.5) — so nothing here is required for a working deployment; it is
// the substrate collaborative rooms use when a Polygon is in collaborative
// mode.
//
// The CRDT is an RGA (Replicated Growable Array) of runes: a replicated,
// growable sequence where every element carries a globally unique, stable id
// and inserts name the element they follow. Its defining property is
// convergence — applying the same set of operations in any order yields the
// identical document (the "strong eventual consistency" of a CRDT). That is
// what lets the hub simply broadcast each local operation to the other editors
// of a Polygon and rely on every replica arriving at the same text regardless
// of the order operations land in (Req 25.2). The in-memory Apply is immediate;
// the "within 5 seconds" bound of Req 25.2 is therefore purely a
// transport/latency concern of the broadcast, not of the merge — a delivered
// operation converges the receiving replica synchronously.
//
// How convergence is achieved:
//
//   - Insert(id, after, run) adds an element with a unique id, positioned
//     immediately after the element named by after (or at the document start
//     when after is the zero OpID / RootID). When two concurrent inserts name
//     the SAME after element, they are ordered among themselves by a fixed
//     total order on their ids (OpID.Less), so every replica breaks the tie the
//     same way. This is the RGA rule that makes insert commutative.
//   - Delete(id) tombstones the element with that id. Deleting is idempotent
//     and commutes with everything: a tombstone simply hides the element from
//     the materialized string. Deleting an id that has not yet arrived is
//     recorded so a later-arriving insert of that id is born already deleted
//     (delete/insert commute regardless of arrival order).
//
// Because insert is commutative (via the total order) and delete is an
// idempotent, order-independent tombstone, ANY interleaving of a given
// operation set produces the same tree and therefore the same materialized
// document — the property the unit tests exercise directly.

// OpID uniquely and stably identifies one CRDT element (one inserted run). It
// pairs the originating replica's id with a per-replica monotonically
// increasing sequence number, which together are globally unique without
// coordination. OpID is comparable and safe to use as a map key.
type OpID struct {
	// Origin identifies the replica (editor/connection) that created the
	// element. Any stable per-replica string works; the hub can use a client
	// id. Distinct replicas MUST use distinct Origins for ids to be unique.
	Origin string
	// Seq is a per-Origin, strictly increasing sequence number. The pair
	// (Origin, Seq) is globally unique.
	Seq uint64
}

// RootID is the zero OpID. An insert whose After equals RootID is inserted at
// the very start of the document; it is never a real element's id (a real
// element always has a non-empty Origin).
var RootID = OpID{}

// IsRoot reports whether id addresses the document start rather than a real
// element.
func (id OpID) IsRoot() bool { return id.Origin == "" && id.Seq == 0 }

// Less defines the fixed total order used to break ties between concurrent
// inserts that share the same After element. Any deterministic total order
// works as long as every replica uses the same one; this compares Seq first
// (higher Seq sorts earlier, so a later-numbered concurrent insert wins the
// position closest to the anchor) then Origin as a stable final tiebreaker.
// The exact ordering is unimportant — only that it is total and identical on
// all replicas, which is what guarantees convergence.
func (id OpID) Less(other OpID) bool {
	if id.Seq != other.Seq {
		return id.Seq > other.Seq
	}
	return id.Origin < other.Origin
}

// OpKind is the kind of a CRDT operation.
type OpKind uint8

const (
	// OpInsert inserts a run of runes after a named element.
	OpInsert OpKind = iota
	// OpDelete tombstones an existing element by id.
	OpDelete
)

// Op is a single CRDT operation broadcast between replicas. It is
// self-contained: applying the same Op on any replica, in any order relative to
// other Ops, has the same effect, which is what makes it safe to fan out over
// the hub and apply on arrival.
type Op struct {
	Kind OpKind
	// ID is the id of the element this op concerns: the id assigned to the new
	// element for an insert, or the id of the element to tombstone for a delete.
	ID OpID
	// After is the id of the element the new element follows (insert only).
	// RootID means "insert at the document start".
	After OpID
	// Value is the run of text inserted (insert only). A run is stored as a
	// single element so a multi-character paste is one operation; RGA
	// convergence is unaffected by run length because a run is an atomic
	// element in this implementation.
	Value string
}

// element is one node in the RGA sequence: an inserted run plus its position
// metadata and tombstone flag.
type element struct {
	id      OpID
	after   OpID
	value   string
	deleted bool
}

// Doc is a single replica's CRDT document. It is NOT safe for concurrent use;
// callers (a room's collaborative loop) serialize access, mirroring how the Hub
// serializes room mutation. A Doc materializes to a plain string via String,
// which is what a Snapshotter persists to POLYGON.content.
type Doc struct {
	// elements holds every element ever inserted, including tombstoned ones,
	// keyed by id for O(1) lookup during apply.
	elements map[OpID]*element
	// children maps an anchor id (an element's id, or RootID for the document
	// start) to the ids of elements inserted directly after it. Order within a
	// child slice is maintained by OpID.Less so materialization is a stable,
	// convergent pre-order walk.
	children map[OpID][]OpID
	// deletedPending records tombstones for ids not yet inserted, so a delete
	// that arrives before its insert still takes effect when the insert lands
	// (delete/insert commute).
	deletedPending map[OpID]bool
	// origin is this replica's id, used to stamp locally-created ops.
	origin string
	// seq is this replica's monotonic op counter.
	seq uint64
}

// NewDoc constructs an empty document for the replica identified by origin.
// Origin must be unique per replica (e.g. a connection/client id) so ids minted
// by NewInsert/NewDelete are globally unique.
func NewDoc(origin string) *Doc {
	return &Doc{
		elements:       make(map[OpID]*element),
		children:       make(map[OpID][]OpID),
		deletedPending: make(map[OpID]bool),
		origin:         origin,
	}
}

// nextID mints the next globally-unique id for a locally-originated op.
func (d *Doc) nextID() OpID {
	d.seq++
	return OpID{Origin: d.origin, Seq: d.seq}
}

// NewInsert builds (but does not apply) a local insert of value positioned
// after the element named by after (RootID for the document start). The
// returned Op carries a freshly minted, unique id; the caller applies it
// locally with Apply and broadcasts it to the room's other editors. Splitting
// mint-from-apply lets a caller obtain the op to broadcast and apply it in one
// place.
func (d *Doc) NewInsert(after OpID, value string) Op {
	return Op{Kind: OpInsert, ID: d.nextID(), After: after, Value: value}
}

// NewDelete builds (but does not apply) a local delete of the element with id.
func (d *Doc) NewDelete(id OpID) Op {
	return Op{Kind: OpDelete, ID: id}
}

// Apply applies op to the document. It is the single convergence point: applying
// the same op set in any order yields the same document. Apply is idempotent —
// replaying an op already seen is a no-op — so a duplicated broadcast cannot
// corrupt the document.
func (d *Doc) Apply(op Op) {
	switch op.Kind {
	case OpInsert:
		d.applyInsert(op)
	case OpDelete:
		d.applyDelete(op)
	}
}

func (d *Doc) applyInsert(op Op) {
	if _, seen := d.elements[op.ID]; seen {
		return // idempotent: already applied.
	}
	el := &element{id: op.ID, after: op.After, value: op.Value}
	// Honor a tombstone that arrived before this insert (delete/insert commute).
	if d.deletedPending[op.ID] {
		el.deleted = true
		delete(d.deletedPending, op.ID)
	}
	d.elements[op.ID] = el

	// Insert into the anchor's child list at the position dictated by the fixed
	// total order, so all replicas place concurrent siblings identically.
	siblings := d.children[op.After]
	idx := sort.Search(len(siblings), func(i int) bool {
		return op.ID.Less(siblings[i])
	})
	siblings = append(siblings, OpID{})
	copy(siblings[idx+1:], siblings[idx:])
	siblings[idx] = op.ID
	d.children[op.After] = siblings
}

func (d *Doc) applyDelete(op Op) {
	el, ok := d.elements[op.ID]
	if !ok {
		// The insert has not arrived yet; remember the tombstone so the insert
		// is born deleted when it lands.
		d.deletedPending[op.ID] = true
		return
	}
	el.deleted = true // idempotent: setting an already-set flag is harmless.
}

// String materializes the current converged document as plain text: a pre-order
// walk of the RGA tree from the document start, emitting each live element's
// run and skipping tombstoned ones. Two replicas that have applied the same op
// set return byte-identical strings from String (the convergence property).
func (d *Doc) String() string {
	var b []byte
	d.walk(RootID, &b)
	return string(b)
}

// walk appends the live text of the subtree anchored at id, in convergent
// order, to b. It recurses depth-first: an element's own text, then the text of
// everything inserted after it.
func (d *Doc) walk(anchor OpID, b *[]byte) {
	for _, childID := range d.children[anchor] {
		el := d.elements[childID]
		if !el.deleted {
			*b = append(*b, el.value...)
		}
		d.walk(childID, b)
	}
}

// Snapshot is the converged document text at the moment of the call, suitable
// for persisting to POLYGON.content. It is String by another name, provided so
// the periodic-snapshot hook reads as a snapshot rather than a stringify.
func (d *Doc) Snapshot() string { return d.String() }

// Snapshotter persists a converged document snapshot for a Polygon. It is the
// hook by which collaborative rooms periodically write the merged text back to
// POLYGON.content (Req 25.2). This package deliberately does NOT run a timer or
// hold a repository: it exposes only the seam. Production wiring (a repo-backed
// Snapshotter and a ticker driving SnapshotRoom) lives with the server so the
// CRDT and the room bookkeeping stay storage-agnostic and unit-testable, and so
// tests need not stand up a running timer.
type Snapshotter interface {
	// Snapshot persists content as the current text of the Polygon addressed by
	// key. It is called with the materialized Doc.Snapshot output.
	Snapshot(key RoomKey, content string) error
}

// SnapshotFunc adapts a function to the Snapshotter interface.
type SnapshotFunc func(key RoomKey, content string) error

// Snapshot calls f.
func (f SnapshotFunc) Snapshot(key RoomKey, content string) error { return f(key, content) }

// SnapshotDoc materializes doc and hands it to snap for persistence against
// key. It is the one-call helper a periodic snapshot timer (or a test) invokes;
// it performs no scheduling of its own so the caller owns the cadence. Passing
// a nil Snapshotter is a no-op, letting a collaborative room run without
// persistence configured.
func SnapshotDoc(snap Snapshotter, key RoomKey, doc *Doc) error {
	if snap == nil {
		return nil
	}
	return snap.Snapshot(key, doc.Snapshot())
}
