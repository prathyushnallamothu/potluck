// Package split implements Phase 2: running one model as a pipeline across
// several devices when it fits on none of them alone.
//
// Mechanics (built on llama.cpp's RPC backend):
//
//   - Worker nodes run `rpc-server`, exposing their GPU/RAM over the LAN.
//   - One driver node — a node that already has the model in its Ollama
//     store — runs `llama-server --rpc worker1,worker2,...`, which loads the
//     GGUF and distributes layers across the workers and its own backend
//     proportionally to their free memory.
//   - The driver's agent proxies OpenAI-style requests into that llama-server.
//
// Only the driver needs the weights on disk; llama.cpp streams tensors to
// workers at load time. First load of a big model over WiFi takes minutes —
// pipelines are kept warm and torn down after an idle timeout.
package split

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Binaries are the llama.cpp executables Phase 2 needs. Empty paths mean the
// capability is unavailable on this node.
type Binaries struct {
	RPCServer   string `json:"rpc_server,omitempty"`   // worker capability
	LlamaServer string `json:"llama_server,omitempty"` // driver capability
}

// Detect finds llama.cpp binaries, preferring explicit paths over $PATH.
func Detect(rpcServerPath, llamaServerPath string) Binaries {
	return Binaries{
		RPCServer:   resolve(rpcServerPath, "rpc-server"),
		LlamaServer: resolve(llamaServerPath, "llama-server"),
	}
}

func resolve(explicit, name string) string {
	if explicit != "" {
		if p, err := exec.LookPath(explicit); err == nil {
			return p
		}
		return ""
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// PipelineState is the lifecycle of one distributed model instance.
type PipelineState string

const (
	StateLoading PipelineState = "loading"
	StateReady   PipelineState = "ready"
	StateError   PipelineState = "error"
)

// PipelineInfo is what a driver advertises about a running pipeline.
type PipelineInfo struct {
	Model     string        `json:"model"`
	State     PipelineState `json:"state"`
	Workers   []string      `json:"workers"` // rpc host:port list (empty entry = driver-local only)
	StartedAt time.Time     `json:"started_at"`
	LastUsed  time.Time     `json:"last_used"`
	Error     string        `json:"error,omitempty"`
}

// Overhead factors for memory planning. A loaded model needs its weights
// plus KV cache and compute buffers; workers shouldn't be filled to the brim.
const (
	FitFactor        = 1.25 // model size -> memory needed to run it
	WorkerUsableFrac = 0.80 // fraction of a node's free memory a pipeline may claim
)

// Need returns the estimated memory required to run a model of the given
// file size.
func Need(modelSize uint64) uint64 {
	return uint64(float64(modelSize) * FitFactor)
}

// Usable returns how much of a node's available memory a pipeline may use.
func Usable(memAvailable uint64) uint64 {
	return uint64(float64(memAvailable) * WorkerUsableFrac)
}

// LogDir returns (creating if needed) the directory where rpc-server and
// llama-server output is captured, so failures are debuggable.
func LogDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	dir := filepath.Join(home, ".potluck", "logs")
	os.MkdirAll(dir, 0o755)
	return dir
}

// openLog opens an append-mode log file for a child process, falling back to
// nil (output discarded) on error.
func openLog(name string) *os.File {
	safe := strings.NewReplacer("/", "_", ":", "_", " ", "_").Replace(name)
	f, err := os.OpenFile(filepath.Join(LogDir(), safe+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// startLogged launches cmd with stdout/stderr captured to a named log file.
func startLogged(cmd *exec.Cmd, logName string) error {
	if f := openLog(logName); f != nil {
		cmd.Stdout = f
		cmd.Stderr = f
		err := cmd.Start()
		f.Close() // child holds its own descriptor
		return err
	}
	return cmd.Start()
}
