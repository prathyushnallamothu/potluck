// Package scheduler picks the best node in the pool to serve an inference
// request. Phase 1 strategy: route whole requests to a node that has the
// model. Ranking, best first:
//
//  1. model already loaded in memory (no cold-start swap)
//  2. fewest in-flight requests
//  3. most available memory headroom
//
// Nodes that are offline-stale, not sharing, or missing the model are
// excluded.
package scheduler

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prathyushnallamothu/potluck/internal/registry"
)

// StaleAfter is how old a node's state may be before it is skipped.
const StaleAfter = 20 * time.Second

// ErrNoNode explains why no node could serve the request and lists what the
// pool can serve instead, so clients get an actionable message.
type ErrNoNode struct {
	Model     string
	Available []string
}

func (e *ErrNoNode) Error() string {
	if len(e.Available) == 0 {
		return fmt.Sprintf("no node in the pool has model %q and no models are available; pull one with `ollama pull` on any node", e.Model)
	}
	return fmt.Sprintf("no node in the pool has model %q; available models: %s", e.Model, strings.Join(e.Available, ", "))
}

// Pick selects the best node to serve the named model.
func Pick(nodes []registry.NodeState, model string) (registry.NodeState, error) {
	candidates := nodes[:0:0]
	for _, n := range nodes {
		if !eligible(n) {
			continue
		}
		if n.HasModel(model) {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 0 {
		return registry.NodeState{}, &ErrNoNode{Model: model, Available: PoolModels(nodes)}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		al, bl := a.HasLoaded(model), b.HasLoaded(model)
		if al != bl {
			return al
		}
		if a.Active != b.Active {
			return a.Active < b.Active
		}
		return a.Resources.MemAvailable > b.Resources.MemAvailable
	})
	return candidates[0], nil
}

func eligible(n registry.NodeState) bool {
	return n.Sharing && n.OllamaOK && time.Since(n.UpdatedAt) < StaleAfter
}

// PoolModels returns the deduplicated, sorted model names served by any
// eligible node in the pool.
func PoolModels(nodes []registry.NodeState) []string {
	seen := map[string]bool{}
	for _, n := range nodes {
		if !eligible(n) {
			continue
		}
		for _, m := range n.Models {
			seen[m.Name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// PoolModelDetails returns one entry per distinct model with its size and the
// names of nodes serving it.
type ModelDetail struct {
	registry.Model
	Nodes []string `json:"nodes"`
}

func PoolModelDetails(nodes []registry.NodeState) []ModelDetail {
	byName := map[string]*ModelDetail{}
	for _, n := range nodes {
		if !eligible(n) {
			continue
		}
		for _, m := range n.Models {
			d, ok := byName[m.Name]
			if !ok {
				d = &ModelDetail{Model: m}
				byName[m.Name] = d
			}
			d.Nodes = append(d.Nodes, n.Name)
		}
	}
	out := make([]ModelDetail, 0, len(byName))
	for _, d := range byName {
		sort.Strings(d.Nodes)
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
