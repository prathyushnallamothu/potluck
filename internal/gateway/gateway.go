// Package gateway exposes the pool as a single LLM server. Any node answers
// on both API styles:
//
//	OpenAI-compatible:  /v1/chat/completions, /v1/completions, /v1/embeddings, /v1/models
//	Ollama-compatible:  /api/chat, /api/generate, /api/embeddings, /api/embed, /api/tags
//
// The receiving node parses the "model" field, asks the scheduler for the
// best node, and streams the request through. Remote execution goes via the
// target agent's /proxy/ollama/* endpoint (each node's Ollama stays bound to
// localhost; only agents talk across the network).
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prathyushnallamothu/potluck/internal/registry"
	"github.com/prathyushnallamothu/potluck/internal/scheduler"
)

// TokenHeader carries the shared pool token between agents.
const TokenHeader = "X-Potluck-Token"

// maxBodyBytes caps inference request bodies (prompts + images).
const maxBodyBytes = 64 << 20 // 64 MiB

// RequestRecord is one routed inference request, kept for the dashboard.
type RequestRecord struct {
	ID       int64     `json:"id"`
	Time     time.Time `json:"time"`
	Path     string    `json:"path"`
	Model    string    `json:"model"`
	Node     string    `json:"node"`
	Status   int       `json:"status"`
	Duration float64   `json:"duration_ms"`
}

// Pool is the view of cluster state the gateway needs from the agent.
type Pool interface {
	Nodes() []registry.NodeState
	SelfID() string
}

// SplitRunner is the agent's Phase 2 machinery, used when this node drives a
// distributed pipeline. Nil means this node can't drive splits.
type SplitRunner interface {
	// EnsureLocal starts (or reuses) a pipeline for model on this node,
	// recruiting the given peer agents as RPC workers, and returns the
	// local llama-server port. Blocks through model load.
	EnsureLocal(ctx context.Context, model string, workerAgentAddrs []string) (int, error)
	// PipelinePort returns the port of a ready local pipeline, if any.
	PipelinePort(model string) (int, bool)
	// TouchPipeline marks a pipeline as recently used.
	TouchPipeline(model string)
}

// Gateway routes inference traffic for one agent process.
type Gateway struct {
	pool      Pool
	split     SplitRunner // nil if this node can't drive split pipelines
	ollamaURL *url.URL    // local Ollama base
	token     string
	log       *slog.Logger
	client    *http.Client

	active      atomic.Int64
	totalServed atomic.Uint64

	reqID   atomic.Int64
	histMu  sync.Mutex
	history []RequestRecord
}

func New(pool Pool, ollamaBase, token string, log *slog.Logger) (*Gateway, error) {
	u, err := url.Parse(ollamaBase)
	if err != nil {
		return nil, fmt.Errorf("invalid ollama url: %w", err)
	}
	return &Gateway{
		pool:      pool,
		ollamaURL: u,
		token:     token,
		log:       log,
		// No global timeout: generations stream for minutes. Dial/TLS
		// timeouts come from http.DefaultTransport.
		client: &http.Client{},
	}, nil
}

// SetSplitRunner attaches the agent's Phase 2 machinery (done after
// construction because the agent needs the gateway's counters first).
func (g *Gateway) SetSplitRunner(r SplitRunner) { g.split = r }

// Active returns the number of in-flight inference requests executing on
// this node's Ollama.
func (g *Gateway) Active() int { return int(g.active.Load()) }

// TotalServed returns how many inference requests this node has executed.
func (g *Gateway) TotalServed() uint64 { return g.totalServed.Load() }

// History returns recent requests routed by this node, newest first.
func (g *Gateway) History() []RequestRecord {
	g.histMu.Lock()
	defer g.histMu.Unlock()
	out := make([]RequestRecord, len(g.history))
	copy(out, g.history)
	return out
}

func (g *Gateway) record(r RequestRecord) {
	const keep = 100
	g.histMu.Lock()
	defer g.histMu.Unlock()
	g.history = append([]RequestRecord{r}, g.history...)
	if len(g.history) > keep {
		g.history = g.history[:keep]
	}
}

// Register installs all gateway routes on mux.
func (g *Gateway) Register(mux *http.ServeMux) {
	// Inference: parse model, schedule, forward.
	for _, p := range []string{
		"/v1/chat/completions", "/v1/completions", "/v1/embeddings",
		"/api/chat", "/api/generate", "/api/embeddings", "/api/embed",
	} {
		mux.HandleFunc("POST "+p, g.handleInference)
	}
	// Aggregated model listings.
	mux.HandleFunc("GET /v1/models", g.handleOpenAIModels)
	mux.HandleFunc("GET /api/tags", g.handleTags)
	// Peer-to-peer execution paths. Methods are explicit so the patterns
	// don't conflict with "GET /" (dashboard) under Go 1.22 mux rules.
	mux.HandleFunc("POST /proxy/ollama/", g.handleProxy)
	mux.HandleFunc("GET /proxy/ollama/", g.handleProxy)
	mux.HandleFunc("POST /proxy/llama/", g.handleLlamaProxy)
	mux.HandleFunc("GET /proxy/llama/", g.handleLlamaProxy)
}

// extractModel pulls the "model" field out of a JSON request body.
func extractModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

func (g *Gateway) handleInference(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		httpError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	model := extractModel(body)
	if model == "" {
		httpError(w, http.StatusBadRequest, `request body must include a "model" field`)
		return
	}

	nodes := g.pool.Nodes()

	// Phase 2: if the model fits no single node, run it as a pipeline
	// across devices. Split pipelines speak the OpenAI dialect only.
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		plan, planErr := scheduler.PlanSplit(nodes, model)
		if planErr != nil {
			// Split needed but impossible — fall through to Phase 1
			// (Ollama may still manage via CPU offload), but say why.
			g.log.Warn("split not possible, trying single node", "model", model, "reason", planErr)
		}
		if plan != nil {
			nodeName, status := g.forwardSplit(w, r, plan, model, body)
			g.finish(start, r.URL.Path, model, nodeName, status)
			return
		}
	}

	node, err := scheduler.Pick(nodes, model)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	status := g.forward(w, r, node, body)
	g.finish(start, r.URL.Path, model, node.Name, status)
}

func (g *Gateway) finish(start time.Time, path, model, nodeName string, status int) {
	rec := RequestRecord{
		ID:       g.reqID.Add(1),
		Time:     start,
		Path:     path,
		Model:    model,
		Node:     nodeName,
		Status:   status,
		Duration: float64(time.Since(start).Microseconds()) / 1000.0,
	}
	g.record(rec)
	g.log.Info("routed", "model", model, "node", nodeName, "path", path,
		"status", status, "ms", int(rec.Duration))
}

// forward sends the request to the chosen node and streams the response back.
// Local node: straight to local Ollama. Remote node: via its agent proxy.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, node registry.NodeState, body []byte) int {
	var target string
	if node.ID == g.pool.SelfID() {
		target = g.ollamaURL.String() + r.URL.Path
	} else {
		target = "http://" + node.Addr + "/proxy/ollama" + r.URL.Path
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to build upstream request")
		return http.StatusInternalServerError
	}
	req.Header.Set("Content-Type", "application/json")
	if g.token != "" {
		req.Header.Set(TokenHeader, g.token)
	}

	if node.ID == g.pool.SelfID() {
		g.active.Add(1)
		defer func() {
			g.active.Add(-1)
			g.totalServed.Add(1)
		}()
	}

	resp, err := g.client.Do(req)
	if err != nil {
		httpError(w, http.StatusBadGateway, fmt.Sprintf("node %q unreachable: %v", node.Name, err))
		return http.StatusBadGateway
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
	return resp.StatusCode
}

// handleProxy executes a request on this node's local Ollama, on behalf of a
// peer agent. Path: /proxy/ollama/<ollama path>.
func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	if g.token != "" && r.Header.Get(TokenHeader) != g.token {
		httpError(w, http.StatusUnauthorized, "missing or invalid pool token")
		return
	}
	g.active.Add(1)
	defer func() {
		g.active.Add(-1)
		g.totalServed.Add(1)
	}()

	proxy := httputil.NewSingleHostReverseProxy(g.ollamaURL)
	proxy.FlushInterval = -1 // stream tokens as they arrive
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/proxy/ollama")
	r.Host = g.ollamaURL.Host
	proxy.ServeHTTP(w, r)
}

func (g *Gateway) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	type oaiModel struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	models := scheduler.PoolModels(g.pool.Nodes())
	out := struct {
		Object string     `json:"object"`
		Data   []oaiModel `json:"data"`
	}{Object: "list", Data: []oaiModel{}}
	for _, m := range models {
		out.Data = append(out.Data, oaiModel{ID: m, Object: "model", Created: time.Now().Unix(), OwnedBy: "potluck"})
	}
	writeJSON(w, out)
}

// handleTags merges every node's models into one Ollama-style tags response,
// so `ollama` CLI pointed at the pool sees everything.
func (g *Gateway) handleTags(w http.ResponseWriter, r *http.Request) {
	type tagModel struct {
		Name    string `json:"name"`
		Model   string `json:"model"`
		Size    uint64 `json:"size"`
		Details struct {
			Family        string `json:"family,omitempty"`
			ParameterSize string `json:"parameter_size,omitempty"`
			Quantization  string `json:"quantization_level,omitempty"`
		} `json:"details"`
	}
	details := scheduler.PoolModelDetails(g.pool.Nodes())
	out := struct {
		Models []tagModel `json:"models"`
	}{Models: []tagModel{}}
	for _, d := range details {
		m := tagModel{Name: d.Name, Model: d.Name, Size: d.Size}
		m.Details.Family = d.Family
		m.Details.ParameterSize = d.Parameters
		m.Details.Quantization = d.Quant
		out.Models = append(out.Models, m)
	}
	writeJSON(w, out)
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// flushCopy streams src to w, flushing after every chunk so token streams
// reach the client immediately.
func flushCopy(w http.ResponseWriter, src io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "potluck_routing_error"},
	})
}
