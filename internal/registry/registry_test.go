package registry

import (
	"testing"
	"time"
)

func TestPruneKeepsSelfAndFresh(t *testing.T) {
	r := New()
	r.Upsert(NodeState{ID: "self", Self: true})
	r.Upsert(NodeState{ID: "fresh"})
	r.Upsert(NodeState{ID: "stale"})

	// Backdate the stale node directly through the public API surface:
	// re-upsert then manipulate via Prune with a future-looking ttl.
	r.mu.Lock()
	r.nodes["stale"].UpdatedAt = time.Now().Add(-time.Hour)
	r.mu.Unlock()

	removed := r.Prune(time.Minute)
	if len(removed) != 1 || removed[0] != "stale" {
		t.Fatalf("removed = %v, want [stale]", removed)
	}
	if _, ok := r.Get("self"); !ok {
		t.Error("self should never be pruned")
	}
	if _, ok := r.Get("fresh"); !ok {
		t.Error("fresh node should not be pruned")
	}
}

func TestSnapshotSelfFirst(t *testing.T) {
	r := New()
	r.Upsert(NodeState{ID: "b", Name: "bbb"})
	r.Upsert(NodeState{ID: "a", Name: "aaa", Self: true})
	snap := r.Snapshot()
	if len(snap) != 2 || !snap[0].Self {
		t.Fatalf("snapshot should list self first, got %+v", snap)
	}
}

func TestHasModelTagNormalization(t *testing.T) {
	n := NodeState{Models: []Model{{Name: "llama3.2:latest"}}}
	if !n.HasModel("llama3.2") || !n.HasModel("llama3.2:latest") {
		t.Error("bare and tagged names should both match")
	}
	if n.HasModel("llama3.1") {
		t.Error("different model should not match")
	}
}
