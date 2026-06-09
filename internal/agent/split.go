package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/prathyushnallamothu/potluck/internal/gateway"
	"github.com/prathyushnallamothu/potluck/internal/ollamafs"
)

// EnsureLocal implements gateway.SplitRunner: this node acts as the driver
// for a distributed pipeline. It recruits each worker agent's rpc-server,
// then launches llama-server against them, reusing the GGUF blob from the
// local Ollama store.
func (a *Agent) EnsureLocal(ctx context.Context, model string, workerAgentAddrs []string) (int, error) {
	if a.pipelines == nil || !a.pipelines.Available() {
		return 0, fmt.Errorf("llama-server not available on this node")
	}
	gguf, err := ollamafs.ModelBlobPath(a.cfg.ModelsDir, model)
	if err != nil {
		return 0, err
	}

	var rpcAddrs []string
	for _, addr := range workerAgentAddrs {
		rpcAddr, err := a.recruitWorker(ctx, addr)
		if err != nil {
			// A worker failing shrinks capacity; llama-server will fail
			// loudly if what's left is not enough. Keep going.
			a.log.Warn("worker recruitment failed", "worker", addr, "err", err)
			continue
		}
		rpcAddrs = append(rpcAddrs, rpcAddr)
	}
	if len(workerAgentAddrs) > 0 && len(rpcAddrs) == 0 {
		return 0, fmt.Errorf("no workers could be recruited for %q", model)
	}
	return a.pipelines.Ensure(ctx, model, gguf, rpcAddrs, a.cfg.CtxSize)
}

// PipelinePort implements gateway.SplitRunner.
func (a *Agent) PipelinePort(model string) (int, bool) {
	if a.pipelines == nil {
		return 0, false
	}
	return a.pipelines.Port(model)
}

// TouchPipeline implements gateway.SplitRunner.
func (a *Agent) TouchPipeline(model string) {
	if a.pipelines != nil {
		a.pipelines.Touch(model)
	}
}

// recruitWorker asks a peer agent to start its rpc-server and returns the
// worker's RPC endpoint (peer IP + reported RPC port).
func (a *Agent) recruitWorker(ctx context.Context, agentAddr string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+agentAddr+"/api/rpc/start", bytes.NewReader(nil))
	if err != nil {
		return "", err
	}
	if a.cfg.Token != "" {
		req.Header.Set(gateway.TokenHeader, a.cfg.Token)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	host, _, err := net.SplitHostPort(agentAddr)
	if err != nil {
		return "", fmt.Errorf("bad agent addr %q: %w", agentAddr, err)
	}
	return net.JoinHostPort(host, fmt.Sprint(out.Port)), nil
}

// handleRPCStart starts this node's rpc-server so a driver can use its
// memory. Token-gated like all peer execution paths.
func (a *Agent) handleRPCStart(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Token != "" && r.Header.Get(gateway.TokenHeader) != a.cfg.Token {
		http.Error(w, "missing or invalid pool token", http.StatusUnauthorized)
		return
	}
	if a.worker == nil || !a.worker.Available() {
		http.Error(w, "rpc-server not installed on this node", http.StatusNotImplemented)
		return
	}
	if !a.cfg.Share {
		http.Error(w, "this node is not sharing", http.StatusForbidden)
		return
	}
	if err := a.worker.Ensure(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"port": a.worker.Port()})
}

// handleRPCStop tears down this node's rpc-server.
func (a *Agent) handleRPCStop(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Token != "" && r.Header.Get(gateway.TokenHeader) != a.cfg.Token {
		http.Error(w, "missing or invalid pool token", http.StatusUnauthorized)
		return
	}
	if a.worker != nil {
		a.worker.Stop()
	}
	w.WriteHeader(http.StatusOK)
}

// handlePipelineEnsure lets a peer ask this node to drive a pipeline.
func (a *Agent) handlePipelineEnsure(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Token != "" && r.Header.Get(gateway.TokenHeader) != a.cfg.Token {
		http.Error(w, "missing or invalid pool token", http.StatusUnauthorized)
		return
	}
	var req gateway.EnsureRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Model == "" {
		http.Error(w, "body must be JSON with a model field", http.StatusBadRequest)
		return
	}
	port, err := a.EnsureLocal(r.Context(), req.Model, req.Workers)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(gateway.EnsureResponse{Port: port})
}
