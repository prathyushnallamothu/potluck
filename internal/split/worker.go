package split

import (
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Worker manages this node's rpc-server process, started on demand when a
// driver recruits the node into a pipeline and stopped explicitly or on
// agent shutdown. One rpc-server serves all pipelines; its memory is freed
// when the driver's llama-server disconnects.
type Worker struct {
	bin  string
	port int
	log  *slog.Logger

	mu  sync.Mutex
	cmd *exec.Cmd
}

func NewWorker(bin string, port int, log *slog.Logger) *Worker {
	return &Worker{bin: bin, port: port, log: log}
}

// Available reports whether this node can act as a pipeline worker.
func (w *Worker) Available() bool { return w.bin != "" }

// Running reports whether the rpc-server process is alive.
func (w *Worker) Running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cmd != nil
}

// Port returns the rpc-server listen port.
func (w *Worker) Port() int { return w.port }

// Ensure starts rpc-server if it is not already running and waits until the
// port accepts connections.
func (w *Worker) Ensure() error {
	if w.bin == "" {
		return fmt.Errorf("rpc-server not installed on this node")
	}
	w.mu.Lock()
	if w.cmd != nil {
		w.mu.Unlock()
		return nil
	}
	args := []string{"--host", "0.0.0.0", "--port", fmt.Sprint(w.port)}
	if devices := w.pickDevices(); devices != "" {
		args = append(args, "--device", devices)
	}
	cmd := exec.Command(w.bin, args...)
	if err := startLogged(cmd, "rpc-server"); err != nil {
		w.mu.Unlock()
		return fmt.Errorf("starting rpc-server: %w", err)
	}
	w.cmd = cmd
	w.mu.Unlock()
	w.log.Info("rpc-server started", "port", w.port, "pid", cmd.Process.Pid)

	// Reap the process and clear state whenever it exits, however it exits.
	go func() {
		cmd.Wait()
		w.mu.Lock()
		if w.cmd == cmd {
			w.cmd = nil
		}
		w.mu.Unlock()
		w.log.Info("rpc-server exited", "port", w.port)
	}()

	return waitForPort(fmt.Sprintf("127.0.0.1:%d", w.port), 10*time.Second)
}

// Stop terminates the rpc-server if running.
func (w *Worker) Stop() {
	w.mu.Lock()
	cmd := w.cmd
	w.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}

// deviceLine matches rpc-server's device listing, e.g.
// "  MTL0: Apple M4 (12124 MiB, 12123 MiB free)".
var deviceLine = regexp.MustCompile(`^\s{2}(\S+): .*\((\d+) MiB, \d+ MiB free\)`)

// pickDevices chooses which backend devices the rpc-server should expose.
// Zero-memory devices (e.g. BLAS on macOS) must be excluded — exposing them
// makes drivers schedule tensors onto a device with no memory, which crashes
// the worker mid-load. Prefer real accelerators; fall back to CPU.
func (w *Worker) pickDevices() string {
	out, err := exec.Command(w.bin, "--device", "list").CombinedOutput()
	if err != nil && len(out) == 0 {
		return "" // old rpc-server without --device; expose defaults
	}
	var gpus, cpus []string
	for _, line := range strings.Split(string(out), "\n") {
		m := deviceLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		mem, _ := strconv.Atoi(m[2])
		if mem <= 0 {
			continue
		}
		if strings.HasPrefix(name, "CPU") {
			cpus = append(cpus, name)
		} else {
			gpus = append(gpus, name)
		}
	}
	if len(gpus) > 0 {
		return strings.Join(gpus, ",")
	}
	return strings.Join(cpus, ",")
}

func waitForPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("rpc-server did not start listening on %s within %s", addr, timeout)
}
