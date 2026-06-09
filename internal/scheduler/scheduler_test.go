package scheduler

import (
	"errors"
	"testing"
	"time"

	"github.com/prathyushnallamothu/potluck/internal/registry"
)

func node(name string, opts func(*registry.NodeState)) registry.NodeState {
	n := registry.NodeState{
		ID:        name,
		Name:      name,
		Sharing:   true,
		OllamaOK:  true,
		UpdatedAt: time.Now(),
		Resources: registry.Resources{MemTotal: 16 << 30, MemAvailable: 8 << 30},
	}
	if opts != nil {
		opts(&n)
	}
	return n
}

func withModel(names ...string) func(*registry.NodeState) {
	return func(n *registry.NodeState) {
		for _, name := range names {
			n.Models = append(n.Models, registry.Model{Name: name, Size: 4 << 30})
		}
	}
}

func TestPickPrefersLoadedModel(t *testing.T) {
	cold := node("cold", withModel("llama3.2:latest"))
	warm := node("warm", withModel("llama3.2:latest"))
	warm.Loaded = []string{"llama3.2:latest"}

	got, err := Pick([]registry.NodeState{cold, warm}, "llama3.2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "warm" {
		t.Errorf("picked %q, want warm node", got.Name)
	}
}

func TestPickPrefersLeastBusy(t *testing.T) {
	busy := node("busy", withModel("m"))
	busy.Active = 3
	idle := node("idle", withModel("m"))

	got, err := Pick([]registry.NodeState{busy, idle}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "idle" {
		t.Errorf("picked %q, want idle node", got.Name)
	}
}

func TestPickSkipsIneligible(t *testing.T) {
	notSharing := node("selfish", withModel("m"))
	notSharing.Sharing = false
	stale := node("gone", withModel("m"))
	stale.UpdatedAt = time.Now().Add(-time.Minute)
	noOllama := node("broken", withModel("m"))
	noOllama.OllamaOK = false
	ok := node("ok", withModel("m"))

	got, err := Pick([]registry.NodeState{notSharing, stale, noOllama, ok}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ok" {
		t.Errorf("picked %q, want the only eligible node", got.Name)
	}
}

func TestPickNoNodeError(t *testing.T) {
	n := node("a", withModel("mistral:latest"))
	_, err := Pick([]registry.NodeState{n}, "llama3.2")
	var e *ErrNoNode
	if !errors.As(err, &e) {
		t.Fatalf("want ErrNoNode, got %v", err)
	}
	if len(e.Available) != 1 || e.Available[0] != "mistral:latest" {
		t.Errorf("available = %v, want the pool's models", e.Available)
	}
}

func TestPickMatchesLatestTag(t *testing.T) {
	n := node("a", withModel("llama3.2:latest"))
	if _, err := Pick([]registry.NodeState{n}, "llama3.2"); err != nil {
		t.Errorf("bare name should match :latest tag, got %v", err)
	}
	n2 := node("b", withModel("llama3.2"))
	if _, err := Pick([]registry.NodeState{n2}, "llama3.2:latest"); err != nil {
		t.Errorf(":latest should match bare name, got %v", err)
	}
}

func TestPoolModelDetailsMergesNodes(t *testing.T) {
	a := node("a", withModel("m1", "m2"))
	b := node("b", withModel("m1"))
	details := PoolModelDetails([]registry.NodeState{a, b})
	if len(details) != 2 {
		t.Fatalf("got %d models, want 2", len(details))
	}
	if details[0].Name != "m1" || len(details[0].Nodes) != 2 {
		t.Errorf("m1 should be on 2 nodes, got %+v", details[0])
	}
}
