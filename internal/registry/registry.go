// Package registry holds the live view of every node in the pool.
// Each agent maintains its own registry, built from mDNS discovery plus
// periodic state polls of peers — there is no central server.
package registry

import (
	"sort"
	"sync"
	"time"
)

// GPU describes one accelerator on a node.
type GPU struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // "cuda", "metal", "cpu"
	MemTotal uint64 `json:"mem_total"`
	MemFree  uint64 `json:"mem_free"`
}

// Resources is a point-in-time snapshot of a node's hardware.
type Resources struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	CPUModel     string `json:"cpu_model"`
	NumCPU       int    `json:"num_cpu"`
	MemTotal     uint64 `json:"mem_total"`
	MemAvailable uint64 `json:"mem_available"`
	GPUs         []GPU  `json:"gpus,omitempty"`
}

// Model is an LLM available on a node (pulled into its local Ollama).
type Model struct {
	Name       string `json:"name"`
	Size       uint64 `json:"size"`
	Family     string `json:"family,omitempty"`
	Parameters string `json:"parameters,omitempty"`
	Quant      string `json:"quant,omitempty"`
}

// NodeState is everything the pool knows about one node.
type NodeState struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Version       string    `json:"version"`
	Pool          string    `json:"pool"`
	Addr          string    `json:"addr"` // host:port of the agent's HTTP server
	Sharing       bool      `json:"sharing"`
	Resources     Resources `json:"resources"`
	OllamaOK      bool      `json:"ollama_ok"`
	OllamaVersion string    `json:"ollama_version,omitempty"`
	Models        []Model   `json:"models"`
	Loaded        []string  `json:"loaded"` // model names currently in memory
	Active        int       `json:"active"` // in-flight inference requests
	TotalServed   uint64    `json:"total_served"`
	UpdatedAt     time.Time `json:"updated_at"`
	Self          bool      `json:"self"`
}

// HasModel reports whether the node has the named model pulled.
// Ollama treats "llama3.2" and "llama3.2:latest" as the same model.
func (n *NodeState) HasModel(name string) bool {
	for _, m := range n.Models {
		if m.Name == name || m.Name == name+":latest" || m.Name+":latest" == name {
			return true
		}
	}
	return false
}

// HasLoaded reports whether the named model is resident in memory on the node.
func (n *NodeState) HasLoaded(name string) bool {
	for _, l := range n.Loaded {
		if l == name || l == name+":latest" || l+":latest" == name {
			return true
		}
	}
	return false
}

// Registry is a concurrency-safe map of node ID to last-known state.
type Registry struct {
	mu    sync.RWMutex
	nodes map[string]*NodeState
}

func New() *Registry {
	return &Registry{nodes: make(map[string]*NodeState)}
}

// Upsert stores the latest state for a node.
func (r *Registry) Upsert(s NodeState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.UpdatedAt = time.Now()
	r.nodes[s.ID] = &s
}

// Touch refreshes the timestamp of a node if it exists.
func (r *Registry) Touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[id]; ok {
		n.UpdatedAt = time.Now()
	}
}

// Remove drops a node from the registry.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.nodes, id)
}

// Prune removes nodes (except self) not updated within ttl and returns
// the IDs removed.
func (r *Registry) Prune(ttl time.Duration) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed []string
	cutoff := time.Now().Add(-ttl)
	for id, n := range r.nodes {
		if !n.Self && n.UpdatedAt.Before(cutoff) {
			delete(r.nodes, id)
			removed = append(removed, id)
		}
	}
	return removed
}

// Snapshot returns a copy of all node states, sorted self-first then by name.
func (r *Registry) Snapshot() []NodeState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]NodeState, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Self != out[j].Self {
			return out[i].Self
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Get returns the state for one node, if known.
func (r *Registry) Get(id string) (NodeState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[id]
	if !ok {
		return NodeState{}, false
	}
	return *n, true
}
