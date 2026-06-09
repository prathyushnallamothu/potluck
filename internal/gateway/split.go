package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/prathyushnallamothu/potluck/internal/scheduler"
)

// ModelHeader carries the model name on /proxy/llama requests, since model
// names ("user/model:tag") don't embed cleanly in URL paths.
const ModelHeader = "X-Potluck-Model"

// EnsureRequest is the body of POST /api/pipeline/ensure, sent to a driver
// node to bring up (or reuse) a pipeline.
type EnsureRequest struct {
	Model   string   `json:"model"`
	Workers []string `json:"workers"` // agent host:port of recruited workers
}

// EnsureResponse reports the driver-local llama-server port once ready.
type EnsureResponse struct {
	Port int `json:"port"`
}

// forwardSplit serves a request via a distributed pipeline: ensure the
// pipeline exists on the plan's driver, then stream the request through it.
// Returns the serving node's display name and the response status.
func (g *Gateway) forwardSplit(w http.ResponseWriter, r *http.Request, plan *scheduler.Plan, model string, body []byte) (string, int) {
	workerAddrs := make([]string, 0, len(plan.Workers))
	workerNames := make([]string, 0, len(plan.Workers))
	for _, n := range plan.Workers {
		workerAddrs = append(workerAddrs, n.Addr)
		workerNames = append(workerNames, n.Name)
	}
	display := plan.Driver.Name + "+[" + strings.Join(workerNames, ",") + "]"
	g.log.Info("split pipeline requested", "model", model,
		"driver", plan.Driver.Name, "workers", workerNames,
		"need_gib", plan.Need>>30)

	var target string
	if plan.Driver.ID == g.pool.SelfID() {
		if g.split == nil {
			httpError(w, http.StatusNotImplemented, "this node cannot drive split pipelines (llama-server missing)")
			return display, http.StatusNotImplemented
		}
		port, err := g.split.EnsureLocal(r.Context(), model, workerAddrs)
		if err != nil {
			httpError(w, http.StatusBadGateway, fmt.Sprintf("failed to start pipeline: %v", err))
			return display, http.StatusBadGateway
		}
		target = fmt.Sprintf("http://127.0.0.1:%d%s", port, r.URL.Path)
	} else {
		if err := g.ensureRemote(r, plan.Driver.Addr, model, workerAddrs); err != nil {
			httpError(w, http.StatusBadGateway, fmt.Sprintf("driver %q failed to start pipeline: %v", plan.Driver.Name, err))
			return display, http.StatusBadGateway
		}
		target = "http://" + plan.Driver.Addr + "/proxy/llama" + r.URL.Path
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to build upstream request")
		return display, http.StatusInternalServerError
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ModelHeader, model)
	if g.token != "" {
		req.Header.Set(TokenHeader, g.token)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		httpError(w, http.StatusBadGateway, fmt.Sprintf("pipeline unreachable: %v", err))
		return display, http.StatusBadGateway
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
	return display, resp.StatusCode
}

// ensureRemote asks a driver node's agent to bring up a pipeline. Model load
// can take minutes, so this inherits the client's context rather than a
// fixed timeout.
func (g *Gateway) ensureRemote(r *http.Request, driverAddr, model string, workers []string) error {
	body, _ := json.Marshal(EnsureRequest{Model: model, Workers: workers})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		"http://"+driverAddr+"/api/pipeline/ensure", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.token != "" {
		req.Header.Set(TokenHeader, g.token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// handleLlamaProxy forwards a peer's request into a ready local pipeline.
func (g *Gateway) handleLlamaProxy(w http.ResponseWriter, r *http.Request) {
	if g.token != "" && r.Header.Get(TokenHeader) != g.token {
		httpError(w, http.StatusUnauthorized, "missing or invalid pool token")
		return
	}
	if g.split == nil {
		httpError(w, http.StatusNotImplemented, "this node does not run pipelines")
		return
	}
	model := r.Header.Get(ModelHeader)
	port, ok := g.split.PipelinePort(model)
	if !ok {
		httpError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("no ready pipeline for %q on this node; ensure it first", model))
		return
	}
	g.split.TouchPipeline(model)
	g.active.Add(1)
	defer func() {
		g.active.Add(-1)
		g.totalServed.Add(1)
	}()

	upstream, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.FlushInterval = -1
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/proxy/llama")
	r.Host = upstream.Host
	proxy.ServeHTTP(w, r)
}
