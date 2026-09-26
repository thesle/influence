package ws

import (
	"math/rand"
	"testing"

	"pgregory.net/rapid"
)

// buildOrigin applies a list of ops to a fresh replica identified by origin and
// returns the materialized document. The replica origin does not affect how
// remote ops are applied — ids are already baked into the ops — so it is only a
// label here.
func applyAll(origin string, ops []Op) string {
	d := NewDoc(origin)
	for _, op := range ops {
		d.Apply(op)
	}
	return d.String()
}

// TestSequentialInsertBuildsExpectedString confirms a straightforward left-to-
// right authoring session materializes to the typed text (Req 25.2).
func TestSequentialInsertBuildsExpectedString(t *testing.T) {
	d := NewDoc("A")

	// Type "hello" one character at a time, each after the previous element.
	after := RootID
	for _, ch := range "hello" {
		op := d.NewInsert(after, string(ch))
		d.Apply(op)
		after = op.ID
	}

	if got := d.String(); got != "hello" {
		t.Fatalf("document = %q, want %q", got, "hello")
	}
}

// TestInsertAtStartOrders confirms inserts anchored at the document start are
// ordered deterministically among themselves so the text is well defined even
// when several elements share the RootID anchor.
func TestInsertAtStartOrders(t *testing.T) {
	d := NewDoc("A")
	// Insert "b", then "a" both at the start. The materialized order is fixed
	// by OpID.Less (higher Seq sorts earlier), so the later insert "a" lands
	// before "b".
	first := d.NewInsert(RootID, "b")
	d.Apply(first)
	second := d.NewInsert(RootID, "a")
	d.Apply(second)

	if got := d.String(); got != "ab" {
		t.Fatalf("document = %q, want %q", got, "ab")
	}
}

// TestDeleteRemovesElement confirms a delete tombstones its element and the
// text disappears from the materialized document, while the rest is preserved.
func TestDeleteRemovesElement(t *testing.T) {
	d := NewDoc("A")
	after := RootID
	var ids []OpID
	for _, ch := range "abc" {
		op := d.NewInsert(after, string(ch))
		d.Apply(op)
		after = op.ID
		ids = append(ids, op.ID)
	}
	// Delete the middle element ("b").
	d.Apply(d.NewDelete(ids[1]))

	if got := d.String(); got != "ac" {
		t.Fatalf("after delete document = %q, want %q", got, "ac")
	}
}

// TestDeleteIsIdempotent confirms replaying a delete (as a duplicated broadcast
// might) leaves the document unchanged.
func TestDeleteIsIdempotent(t *testing.T) {
	d := NewDoc("A")
	op := d.NewInsert(RootID, "x")
	d.Apply(op)
	del := d.NewDelete(op.ID)
	d.Apply(del)
	d.Apply(del) // replay

	if got := d.String(); got != "" {
		t.Fatalf("document = %q, want empty", got)
	}
}

// TestInsertIsIdempotent confirms replaying an insert does not duplicate the
// element, so a re-delivered broadcast is safe.
func TestInsertIsIdempotent(t *testing.T) {
	d := NewDoc("A")
	op := d.NewInsert(RootID, "x")
	d.Apply(op)
	d.Apply(op) // replay

	if got := d.String(); got != "x" {
		t.Fatalf("document = %q, want %q", got, "x")
	}
}

// TestDeleteBeforeInsertConverges confirms a tombstone arriving before its
// insert still takes effect once the insert lands — delete and insert commute
// regardless of arrival order.
func TestDeleteBeforeInsertConverges(t *testing.T) {
	// Mint the ops on one replica so ids line up.
	author := NewDoc("A")
	ins := author.NewInsert(RootID, "z")
	del := author.NewDelete(ins.ID)

	// Apply delete FIRST, then insert, on a fresh replica.
	d := NewDoc("B")
	d.Apply(del)
	d.Apply(ins)

	if got := d.String(); got != "" {
		t.Fatalf("document = %q, want empty (born-deleted)", got)
	}
}

// TestConcurrentInsertsConvergeInAnyOrder is the core CRDT property expressed as
// an explicit example: two editors concurrently insert at the same anchor, and
// the two replicas apply the resulting op set in different orders. They must
// converge to the same document (Req 25.2).
func TestConcurrentInsertsConvergeInAnyOrder(t *testing.T) {
	// Shared prefix element "x" both editors see before diverging.
	base := NewDoc("seed")
	x := base.NewInsert(RootID, "x")

	// Editor A inserts "a" after x; editor B concurrently inserts "b" after x.
	a := Op{Kind: OpInsert, ID: OpID{Origin: "A", Seq: 1}, After: x.ID, Value: "a"}
	b := Op{Kind: OpInsert, ID: OpID{Origin: "B", Seq: 1}, After: x.ID, Value: "b"}

	// Replica 1 applies x, a, b; replica 2 applies x, b, a.
	doc1 := applyAll("r1", []Op{x, a, b})
	doc2 := applyAll("r2", []Op{x, b, a})

	if doc1 != doc2 {
		t.Fatalf("replicas diverged: %q vs %q", doc1, doc2)
	}
	// And the tie-break is deterministic: same-Seq inserts order by Origin.
	if doc1 != "xab" {
		t.Fatalf("converged document = %q, want %q", doc1, "xab")
	}
}

// TestConvergenceUnderAllPermutations builds a fixed op set (interleaved
// inserts and deletes across two origins) and confirms every one of its
// permutations that respects insert-before-nothing (ops are self-contained, so
// all orders are legal) converges to the identical document.
func TestConvergenceUnderAllPermutations(t *testing.T) {
	// Author a set of ops on a seed replica so ids are fixed.
	seed := NewDoc("seed")
	h := seed.NewInsert(RootID, "h")
	i := seed.NewInsert(h.ID, "i")
	bang := seed.NewInsert(i.ID, "!")
	delI := seed.NewDelete(i.ID)
	// A concurrent insert from another origin at the same anchor as i.
	y := Op{Kind: OpInsert, ID: OpID{Origin: "other", Seq: 1}, After: h.ID, Value: "Y"}

	ops := []Op{h, i, bang, delI, y}
	want := applyAll("ref", ops)

	// Try many random permutations; all must match the reference document.
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 200; trial++ {
		perm := make([]Op, len(ops))
		copy(perm, ops)
		rng.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		if got := applyAll("perm", perm); got != want {
			t.Fatalf("permutation %v diverged: got %q want %q", perm, got, want)
		}
	}
}

// TestSnapshotMaterializesConvergedDoc confirms Snapshot returns the same text
// as String and that SnapshotDoc hands that text to a Snapshotter for
// persistence to POLYGON.content — the periodic-snapshot hook (Req 25.2).
func TestSnapshotMaterializesConvergedDoc(t *testing.T) {
	d := NewDoc("A")
	after := RootID
	for _, ch := range "doc" {
		op := d.NewInsert(after, string(ch))
		d.Apply(op)
		after = op.ID
	}

	if d.Snapshot() != d.String() || d.Snapshot() != "doc" {
		t.Fatalf("snapshot = %q, want %q", d.Snapshot(), "doc")
	}

	var gotKey RoomKey
	var gotContent string
	snap := SnapshotFunc(func(key RoomKey, content string) error {
		gotKey, gotContent = key, content
		return nil
	})
	key := testKey("poly-1")
	if err := SnapshotDoc(snap, key, d); err != nil {
		t.Fatalf("SnapshotDoc: %v", err)
	}
	if gotKey != key || gotContent != "doc" {
		t.Fatalf("snapshotter got (%v, %q), want (%v, %q)", gotKey, gotContent, key, "doc")
	}

	// A nil Snapshotter is a no-op (persistence not configured).
	if err := SnapshotDoc(nil, key, d); err != nil {
		t.Fatalf("nil SnapshotDoc: %v", err)
	}
}

// TestBroadcastPropagatesCRDTOp confirms a locally-created CRDT op fans out to
// the other editors of a Polygon via the hub, and that applying the propagated
// op on the receiver's replica reproduces the sender's document — the transport
// half of "propagate within 5 seconds" (Req 25.2). It reuses the hub's
// fakeClient pattern; a real transport would encode Op to bytes, but the wire
// encoding is orthogonal to convergence, so this exercises the hub fan-out
// directly.
func TestBroadcastPropagatesCRDTOp(t *testing.T) {
	h := NewHub()
	key := testKey("collab-poly")

	sender := newFakeClient("sender")
	receiver := newFakeClient("receiver")
	h.Join(key, sender)
	h.Join(key, receiver)

	// Sender authors an insert and broadcasts its (here, raw text) payload.
	senderDoc := NewDoc(sender.ID())
	op := senderDoc.NewInsert(RootID, "hi")
	senderDoc.Apply(op)

	// The hub fans the op out to the other editor, excluding the sender.
	payload := []byte(op.Value) // stand-in wire payload; see doc comment.
	if delivered := h.Broadcast(key, sender.ID(), payload); delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}
	if len(sender.messages()) != 0 {
		t.Errorf("sender received its own op; want none")
	}
	msgs := receiver.messages()
	if len(msgs) != 1 {
		t.Fatalf("receiver got %d messages, want 1", len(msgs))
	}

	// Applying the propagated op on the receiver's replica converges it to the
	// sender's document.
	receiverDoc := NewDoc(receiver.ID())
	receiverDoc.Apply(op)
	if receiverDoc.String() != senderDoc.String() {
		t.Fatalf("receiver %q != sender %q after propagation",
			receiverDoc.String(), senderDoc.String())
	}
}

// TestCRDTConvergenceProperty is the property-based statement of the CRDT
// guarantee: for an arbitrary set of self-consistent operations, applying them
// in two independently random orders on two replicas always yields the same
// document.
//
// Validates: Requirements 25.2
func TestCRDTConvergenceProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate a set of ops with valid anchors. We build them incrementally
		// so every insert's After references either RootID or an already-minted
		// element id, and every delete targets an already-minted id. This keeps
		// the op set self-consistent while still spanning multiple origins and
		// interleaved inserts/deletes.
		n := rapid.IntRange(0, 30).Draw(rt, "numOps")

		origins := []string{"A", "B", "C"}
		seqByOrigin := map[string]uint64{}
		var minted []OpID // ids available as anchors / delete targets
		var ops []Op

		charGen := rapid.SampledFrom([]rune("xyz01 "))

		for k := 0; k < n; k++ {
			origin := rapid.SampledFrom(origins).Draw(rt, "origin")
			isDelete := len(minted) > 0 && rapid.Bool().Draw(rt, "isDelete")

			if isDelete {
				target := minted[rapid.IntRange(0, len(minted)-1).Draw(rt, "delTarget")]
				ops = append(ops, Op{Kind: OpDelete, ID: target})
				continue
			}

			// Insert: anchor is RootID or a previously minted id.
			after := RootID
			if len(minted) > 0 && rapid.Bool().Draw(rt, "anchored") {
				after = minted[rapid.IntRange(0, len(minted)-1).Draw(rt, "anchor")]
			}
			seqByOrigin[origin]++
			id := OpID{Origin: origin, Seq: seqByOrigin[origin]}
			val := string(charGen.Draw(rt, "char"))
			ops = append(ops, Op{Kind: OpInsert, ID: id, After: after, Value: val})
			minted = append(minted, id)
		}

		// Two independent random permutations of the SAME op set.
		order1 := rapid.Permutation(ops).Draw(rt, "order1")
		order2 := rapid.Permutation(ops).Draw(rt, "order2")

		doc1 := applyAll("r1", order1)
		doc2 := applyAll("r2", order2)

		if doc1 != doc2 {
			rt.Fatalf("replicas diverged under different orders:\n  %q\n  %q\nops=%v",
				doc1, doc2, ops)
		}
	})
}
