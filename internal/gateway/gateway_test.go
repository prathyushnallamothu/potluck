package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prathyushnallamothu/potluck/internal/registry"
)

type fakePool struct {
	nodes []registry.NodeState
	self  string
}

func (f *fakePool) Nodes() []registry.NodeState { return f.nodes }
func (f *fakePool) SelfID() string              { return f.self }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestExtractModel(t *testing.T) {
	if got := extractModel([]byte(`{"model":"llama3.2","messages":[]}`)); got != "llama3.2" {
		t.Errorf("got %q", got)
	}
	if got := extractModel([]byte(`not json`)); got != "" {
		t.Errorf("got %q for invalid json", got)
	}
}

func TestInferenceRoutesToLocalOllama(t *testing.T) {
	// Fake local Ollama that records what it receives.
	var gotPath, gotModel string
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var m struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &m)
		gotModel = m.Model
		w.Write([]byte(`{"done":true}`))
	}))
	defer ollama.Close()

	pool := &fakePool{self: "self"}
	pool.nodes = []registry.NodeState{{
		ID: "self", Name: "self", Sharing: true, OllamaOK: true,
		UpdatedAt: time.Now(),
		Models:    []registry.Model{{Name: "llama3.2:latest"}},
	}}

	g, err := New(pool, ollama.URL, "", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	g.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama3.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("ollama saw path %q", gotPath)
	}
	if gotModel != "llama3.2" {
		t.Errorf("ollama saw model %q", gotModel)
	}
	if len(g.History()) != 1 {
		t.Errorf("history length = %d, want 1", len(g.History()))
	}
}

func TestInferenceUnknownModelReturns404(t *testing.T) {
	pool := &fakePool{self: "self"}
	g, err := New(pool, "http://127.0.0.1:1", "", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	g.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"nope"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestProxyRequiresToken(t *testing.T) {
	pool := &fakePool{self: "self"}
	g, err := New(pool, "http://127.0.0.1:1", "secret", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	g.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/proxy/ollama/api/chat", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 without token", rec.Code)
	}
}

func TestOpenAIModelsAggregates(t *testing.T) {
	pool := &fakePool{self: "self", nodes: []registry.NodeState{
		{ID: "a", Sharing: true, OllamaOK: true, UpdatedAt: time.Now(),
			Models: []registry.Model{{Name: "m1"}}},
		{ID: "b", Sharing: true, OllamaOK: true, UpdatedAt: time.Now(),
			Models: []registry.Model{{Name: "m2"}}},
	}}
	g, err := New(pool, "http://127.0.0.1:1", "", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	g.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 2 {
		t.Fatalf("got %d models, want 2", len(out.Data))
	}
}
