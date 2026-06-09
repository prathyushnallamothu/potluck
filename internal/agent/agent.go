// Package agent wires everything together: it keeps the local node state
// fresh, advertises on mDNS, polls discovered peers, and serves the HTTP
// surface (dashboard, pool API, and the inference gateway).
//
// Every node runs the same code — there is no central server. Point any
// OpenAI or Ollama client at any node and the pool answers.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prathyushnallamothu/potluck/internal/dashboard"
	"github.com/prathyushnallamothu/potluck/internal/discovery"
	"github.com/prathyushnallamothu/potluck/internal/gateway"
	"github.com/prathyushnallamothu/potluck/internal/registry"
	"github.com/prathyushnallamothu/potluck/internal/resources"
	"github.com/prathyushnallamothu/potluck/internal/scheduler"
)

// Config controls one agent process.
type Config struct {
	Name      string // human-readable node name (default: hostname)
	Port      int    // agent HTTP port
	OllamaURL string // local Ollama base URL
	Pool      string // pool name; nodes only join pools with the same name
	Token     string // optional shared secret for peer-to-peer execution
	Share     bool   // contribute this node's compute to the pool
	Version   string
}

type Agent struct {
	cfg Config
	id  string
	log *slog.Logger

	reg     *registry.Registry
	gw      *gateway.Gateway
	hints   chan discovery.PeerHint
	addrsMu sync.Mutex
	addrs   map[string]string // peer ID -> reachable host:port, from mDNS
}

func New(cfg Config, log *slog.Logger) (*Agent, error) {
	if cfg.Name == "" {
		if h, err := os.Hostname(); err == nil {
			cfg.Name = strings.Split(h, ".")[0]
		} else {
			cfg.Name = "node"
		}
	}
	a := &Agent{
		cfg:   cfg,
		log:   log,
		reg:   registry.New(),
		hints: make(chan discovery.PeerHint, 32),
		addrs: map[string]string{},
	}

	id, err := loadOrCreateID()
	if err != nil {
		return nil, err
	}
	a.id = id

	gw, err := gateway.New(a, cfg.OllamaURL, cfg.Token, log)
	if err != nil {
		return nil, err
	}
	a.gw = gw
	return a, nil
}

// Nodes implements gateway.Pool.
func (a *Agent) Nodes() []registry.NodeState { return a.reg.Snapshot() }

// SelfID implements gateway.Pool.
func (a *Agent) SelfID() string { return a.id }

// loadOrCreateID returns a stable node identity, persisted across restarts in
// ~/.potluck/id.
func loadOrCreateID() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".potluck")
	path := filepath.Join(dir, "id")
	if b, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(b))
		if len(id) >= 8 {
			return id, nil
		}
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o644); err != nil {
		return "", err
	}
	return id, nil
}

// Run starts all loops and the HTTP server, blocking until ctx is cancelled
// or the server fails.
func (a *Agent) Run(ctx context.Context) error {
	a.refreshSelf(ctx) // populate before anything reads it

	if err := discovery.Advertise(ctx, a.id, a.cfg.Name, a.cfg.Pool, a.cfg.Port, a.cfg.Version); err != nil {
		return err
	}
	go discovery.Browse(ctx, a.id, a.cfg.Pool, a.hints, a.log)
	go a.collectHints(ctx)
	go a.loop(ctx, 3*time.Second, a.refreshSelf)
	go a.loop(ctx, 3*time.Second, a.pollPeers)
	go a.loop(ctx, 5*time.Second, func(context.Context) {
		for _, id := range a.reg.Prune(scheduler.StaleAfter * 2) {
			a.log.Info("peer left the pool", "id", id)
		}
	})

	mux := http.NewServeMux()
	a.gw.Register(mux)
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("GET /api/pool", a.handlePool)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /", dashboard.Handler())

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", a.cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	a.log.Info("potluck agent up",
		"name", a.cfg.Name, "id", a.id, "pool", a.cfg.Pool,
		"dashboard", fmt.Sprintf("http://localhost:%d", a.cfg.Port),
		"openai_api", fmt.Sprintf("http://localhost:%d/v1", a.cfg.Port),
		"sharing", a.cfg.Share)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *Agent) loop(ctx context.Context, every time.Duration, fn func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			fn(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// refreshSelf probes local hardware and Ollama and updates the registry.
func (a *Agent) refreshSelf(ctx context.Context) {
	res := resources.Collect()
	oll := resources.ProbeOllama(ctx, a.cfg.OllamaURL)
	a.reg.Upsert(registry.NodeState{
		ID:            a.id,
		Name:          a.cfg.Name,
		Version:       a.cfg.Version,
		Pool:          a.cfg.Pool,
		Addr:          fmt.Sprintf("%s:%d", res.Hostname, a.cfg.Port),
		Sharing:       a.cfg.Share,
		Resources:     res,
		OllamaOK:      oll.OK,
		OllamaVersion: oll.Version,
		Models:        oll.Models,
		Loaded:        oll.Loaded,
		Active:        a.gw.Active(),
		TotalServed:   a.gw.TotalServed(),
		Self:          true,
	})
}

// collectHints records peer addresses found by mDNS so pollPeers can reach
// them before their first successful state fetch.
func (a *Agent) collectHints(ctx context.Context) {
	for {
		select {
		case h := <-a.hints:
			a.addrsMu.Lock()
			known := a.addrs[h.ID] == h.Addr
			a.addrs[h.ID] = h.Addr
			a.addrsMu.Unlock()
			if !known {
				a.log.Info("discovered peer", "name", h.Name, "addr", h.Addr)
			}
		case <-ctx.Done():
			return
		}
	}
}

// pollPeers fetches /api/state from every known peer and updates the registry.
func (a *Agent) pollPeers(ctx context.Context) {
	a.addrsMu.Lock()
	targets := make(map[string]string, len(a.addrs))
	for id, addr := range a.addrs {
		targets[id] = addr
	}
	a.addrsMu.Unlock()

	client := &http.Client{Timeout: 3 * time.Second}
	for id, addr := range targets {
		if id == a.id {
			continue
		}
		st, err := fetchState(ctx, client, addr)
		if err != nil {
			continue // prune loop handles persistent failures
		}
		st.Self = false
		st.Addr = addr // trust the address we can actually reach
		a.reg.Upsert(st)
	}
}

func fetchState(ctx context.Context, client *http.Client, addr string) (registry.NodeState, error) {
	var st registry.NodeState
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/state", nil)
	if err != nil {
		return st, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("status %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&st)
	return st, err
}

func (a *Agent) handleState(w http.ResponseWriter, _ *http.Request) {
	st, ok := a.reg.Get(a.id)
	if !ok {
		http.Error(w, "state not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

// PoolView is the aggregate the dashboard renders.
type PoolView struct {
	Pool         string                   `json:"pool"`
	Self         string                   `json:"self"`
	Nodes        []registry.NodeState     `json:"nodes"`
	Models       []scheduler.ModelDetail  `json:"models"`
	Requests     []gateway.RequestRecord  `json:"requests"`
	TotalMem     uint64                   `json:"total_mem"`
	AvailableMem uint64                   `json:"available_mem"`
	OnlineNodes  int                      `json:"online_nodes"`
}

func (a *Agent) handlePool(w http.ResponseWriter, _ *http.Request) {
	nodes := a.reg.Snapshot()
	view := PoolView{
		Pool:     a.cfg.Pool,
		Self:     a.id,
		Nodes:    nodes,
		Models:   scheduler.PoolModelDetails(nodes),
		Requests: a.gw.History(),
	}
	for _, n := range nodes {
		if time.Since(n.UpdatedAt) < scheduler.StaleAfter {
			view.OnlineNodes++
			view.TotalMem += n.Resources.MemTotal
			view.AvailableMem += n.Resources.MemAvailable
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(view)
}
