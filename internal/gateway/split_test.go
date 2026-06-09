package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prathyushnallamothu/potluck/internal/registry"
)

// fakeRunner pretends to be the agent's pipeline machinery, backed by a real
// HTTP test server standing in for llama-server.
type fakeRunner struct {
	port          int
	ensuredModel  string
	ensureWorkers []string
}

func (f *fakeRunner) EnsureLocal(_ context.Context, model string, workers []string) (int, error) {
	f.ensuredModel = model
	f.ensureWorkers = workers
	return f.port, nil
}
func (f *fakeRunner) PipelinePort(model string) (int, bool) { return f.port, true }
func (f *fakeRunner) TouchPipeline(string)                  {}

func TestOversizedModelRoutesThroughSplitPipeline(t *testing.T) {
	// Stand-in llama-server.
	var gotPath string
	llama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer llama.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(llama.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	const gib = uint64(1) << 30
	self := registry.NodeState{
		ID: "self", Name: "driver", Sharing: true, OllamaOK: true,
		UpdatedAt: time.Now(),
		Resources: registry.Resources{MemTotal: 16 * gib, MemAvailable: 8 * gib},
		Models:    []registry.Model{{Name: "huge:latest", Size: 40 * gib}},
	}
	self.Split.Driver = true
	worker := registry.NodeState{
		ID: "w1", Name: "worker", Addr: "10.0.0.2:11444", Sharing: true, OllamaOK: true,
		UpdatedAt: time.Now(),
		Resources: registry.Resources{MemTotal: 128 * gib, MemAvailable: 96 * gib},
	}
	worker.Split.Worker = true

	pool := &fakePool{self: "self", nodes: []registry.NodeState{self, worker}}
	g, err := New(pool, "http://127.0.0.1:1", "", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{port: port}
	g.SetSplitRunner(runner)
	mux := http.NewServeMux()
	g.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"huge","messages":[]}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if runner.ensuredModel != "huge" {
		t.Errorf("pipeline ensured for %q, want huge", runner.ensuredModel)
	}
	if len(runner.ensureWorkers) != 1 || runner.ensureWorkers[0] != "10.0.0.2:11444" {
		t.Errorf("workers = %v, want the worker's agent addr", runner.ensureWorkers)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("llama-server saw %q", gotPath)
	}
	if h := g.History(); len(h) != 1 || !strings.Contains(h[0].Node, "driver+[worker]") {
		t.Errorf("history = %+v, want split display name", h)
	}
}

func TestOllamaDialectSkipsSplitAndUsesPick(t *testing.T) {
	// Same oversized setup, but /api/chat (Ollama dialect): split is
	// OpenAI-only, so the request should fall through to single-node
	// routing — which still works because the holder has the model.
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"done":true}`))
	}))
	defer ollama.Close()

	const gib = uint64(1) << 30
	self := registry.NodeState{
		ID: "self", Name: "driver", Sharing: true, OllamaOK: true,
		UpdatedAt: time.Now(),
		Resources: registry.Resources{MemTotal: 16 * gib, MemAvailable: 8 * gib},
		Models:    []registry.Model{{Name: "huge:latest", Size: 40 * gib}},
	}
	self.Split.Driver = true

	pool := &fakePool{self: "self", nodes: []registry.NodeState{self}}
	g, err := New(pool, ollama.URL, "", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	g.SetSplitRunner(&fakeRunner{port: 1})
	mux := http.NewServeMux()
	g.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"huge"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLlamaProxyRequiresReadyPipeline(t *testing.T) {
	pool := &fakePool{self: "self"}
	g, err := New(pool, "http://127.0.0.1:1", "", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	// No runner attached: proxy must refuse cleanly.
	mux := http.NewServeMux()
	g.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/proxy/llama/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set(ModelHeader, "huge")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501 with no runner", rec.Code)
	}
}
