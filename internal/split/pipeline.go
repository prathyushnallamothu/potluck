package split

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// IdleTimeout is how long an unused pipeline stays warm before teardown.
// Loading a large model over WiFi is expensive, so this errs long.
const IdleTimeout = 10 * time.Minute

// pipeline is one running llama-server serving one model across RPC workers.
type pipeline struct {
	info PipelineInfo
	port int
	cmd  *exec.Cmd
}

// Manager runs distributed pipelines on the driver node.
type Manager struct {
	bin      string // llama-server path
	basePort int
	log      *slog.Logger

	mu        sync.Mutex
	pipelines map[string]*pipeline // keyed by model name
}

func NewManager(bin string, basePort int, log *slog.Logger) *Manager {
	m := &Manager{bin: bin, basePort: basePort, log: log, pipelines: map[string]*pipeline{}}
	go m.gcLoop()
	return m
}

// Available reports whether this node can act as a pipeline driver.
func (m *Manager) Available() bool { return m.bin != "" }

// Infos lists running pipelines for the node's advertised state.
func (m *Manager) Infos() []PipelineInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PipelineInfo, 0, len(m.pipelines))
	for _, p := range m.pipelines {
		out = append(out, p.info)
	}
	return out
}

// Ensure returns the local port of a ready pipeline for model, starting one
// (and blocking through model load) if needed. workers is the list of
// rpc host:port endpoints to distribute across; an empty list runs
// driver-local only, which is still useful when only the driver has weights
// but a peer asked for the model.
func (m *Manager) Ensure(ctx context.Context, model, ggufPath string, workers []string, ctxSize int) (int, error) {
	if m.bin == "" {
		return 0, fmt.Errorf("llama-server not installed on this node")
	}

	m.mu.Lock()
	if p, ok := m.pipelines[model]; ok {
		state := p.info.State
		port := p.port
		p.info.LastUsed = time.Now()
		m.mu.Unlock()
		switch state {
		case StateReady:
			return port, nil
		case StateLoading:
			return port, m.waitReady(ctx, model, port)
		default:
			// Previous attempt failed; fall through to relaunch.
			m.stop(model)
			m.mu.Lock()
		}
	}

	port, err := freePort(m.basePort)
	if err != nil {
		m.mu.Unlock()
		return 0, err
	}

	args := []string{
		"-m", ggufPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprint(port),
		"--alias", model,
		"-ngl", "999", // offload everything; backends share by free memory
		"-c", fmt.Sprint(ctxSize),
		"--no-webui",
	}
	if len(workers) > 0 {
		args = append(args, "--rpc", strings.Join(workers, ","))
	}
	cmd := exec.Command(m.bin, args...)
	if err := startLogged(cmd, "pipeline-"+model); err != nil {
		m.mu.Unlock()
		return 0, fmt.Errorf("starting llama-server: %w", err)
	}

	p := &pipeline{
		info: PipelineInfo{
			Model: model, State: StateLoading, Workers: workers,
			StartedAt: time.Now(), LastUsed: time.Now(),
		},
		port: port,
		cmd:  cmd,
	}
	m.pipelines[model] = p
	m.mu.Unlock()
	m.log.Info("pipeline starting", "model", model, "port", port, "workers", workers)

	go func() {
		cmd.Wait()
		m.mu.Lock()
		if cur, ok := m.pipelines[model]; ok && cur == p {
			if cur.info.State != StateError {
				delete(m.pipelines, model)
			}
		}
		m.mu.Unlock()
		m.log.Info("pipeline process exited", "model", model)
	}()

	if err := m.waitReady(ctx, model, port); err != nil {
		m.setError(model, err)
		m.stop(model)
		return 0, err
	}
	m.setState(model, StateReady)
	m.log.Info("pipeline ready", "model", model, "port", port)
	return port, nil
}

// waitReady polls llama-server's /health until it reports ready. Large
// models stream weights to workers during this window, so the deadline is
// generous and the caller's ctx can cancel earlier.
func (m *Manager) waitReady(ctx context.Context, model string, port int) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Minute)
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !m.alive(model) {
			return fmt.Errorf("pipeline for %q exited during load (likely out of memory or worker unreachable)", model)
		}
		resp, err := client.Get(url)
		if err == nil {
			ok := resp.StatusCode == http.StatusOK
			resp.Body.Close()
			if ok {
				return nil
			}
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("pipeline for %q did not become ready", model)
}

// Touch marks a pipeline as recently used.
func (m *Manager) Touch(model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pipelines[model]; ok {
		p.info.LastUsed = time.Now()
	}
}

// Port returns the local port of a ready pipeline, if one exists.
func (m *Manager) Port(model string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pipelines[model]
	if !ok || p.info.State != StateReady {
		return 0, false
	}
	return p.port, true
}

func (m *Manager) alive(model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.pipelines[model]
	return ok
}

func (m *Manager) setState(model string, s PipelineState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pipelines[model]; ok {
		p.info.State = s
	}
}

func (m *Manager) setError(model string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pipelines[model]; ok {
		p.info.State = StateError
		p.info.Error = err.Error()
	}
}

// stop kills one pipeline's process and forgets it.
func (m *Manager) stop(model string) {
	m.mu.Lock()
	p, ok := m.pipelines[model]
	if ok {
		delete(m.pipelines, model)
	}
	m.mu.Unlock()
	if ok && p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
}

// StopAll tears down every pipeline (agent shutdown).
func (m *Manager) StopAll() {
	m.mu.Lock()
	models := make([]string, 0, len(m.pipelines))
	for model := range m.pipelines {
		models = append(models, model)
	}
	m.mu.Unlock()
	for _, model := range models {
		m.stop(model)
	}
}

func (m *Manager) gcLoop() {
	for range time.Tick(30 * time.Second) {
		m.mu.Lock()
		var idle []string
		for model, p := range m.pipelines {
			if p.info.State == StateReady && time.Since(p.info.LastUsed) > IdleTimeout {
				idle = append(idle, model)
			}
		}
		m.mu.Unlock()
		for _, model := range idle {
			m.log.Info("pipeline idle, tearing down", "model", model)
			m.stop(model)
		}
	}
}

// freePort finds an unused TCP port at or above base, so multiple pipelines
// (or restarts) don't collide.
func freePort(base int) (int, error) {
	for p := base; p < base+100; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			l.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in range %d-%d", base, base+100)
}
