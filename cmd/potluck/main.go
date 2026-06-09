// Command potluck runs one node of a Potluck pool: it shares this machine's
// Ollama with the local network and exposes the whole pool as a single
// OpenAI/Ollama-compatible server with a live dashboard.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prathyushnallamothu/potluck/internal/agent"
)

// version is overridden at release time via -ldflags "-X main.version=v0.2.0".
var version = "dev"

func main() {
	fs := flag.NewFlagSet("potluck", flag.ExitOnError)
	var cfg agent.Config
	fs.StringVar(&cfg.Name, "name", "", "node name shown in the pool (default: hostname)")
	fs.IntVar(&cfg.Port, "port", 11444, "agent HTTP port (dashboard + API)")
	fs.StringVar(&cfg.OllamaURL, "ollama", "http://127.0.0.1:11434", "local Ollama base URL")
	fs.StringVar(&cfg.Pool, "pool", "default", "pool name; only nodes with the same pool join together")
	fs.StringVar(&cfg.Token, "token", "", "shared secret required for peers to run jobs on this node")
	fs.BoolVar(&cfg.Share, "share", true, "contribute this machine's compute to the pool (false = consume only)")
	showVersion := fs.Bool("version", false, "print version and exit")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `potluck — pool the devices on your WiFi into one LLM server

Usage:
  potluck [flags]          start the agent (dashboard at http://localhost:11444)
  potluck status [flags]   print the pool as seen by the local agent

Flags:
`)
		fs.PrintDefaults()
	}

	args := os.Args[1:]
	if len(args) > 0 && args[0] == "status" {
		fs.Parse(args[1:])
		os.Exit(status(cfg.Port))
	}
	fs.Parse(args)

	if *showVersion {
		fmt.Println("potluck", version)
		return
	}

	cfg.Version = version
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	a, err := agent.New(cfg, log)
	if err != nil {
		log.Error("failed to start", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil {
		log.Error("agent exited", "err", err)
		os.Exit(1)
	}
}

// status prints a compact table of the pool by querying the local agent.
func status(port int) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/pool", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "no agent running on port %d — start one with `potluck`\n", port)
		return 1
	}
	defer resp.Body.Close()

	var view agent.PoolView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		fmt.Fprintln(os.Stderr, "bad response from agent:", err)
		return 1
	}

	fmt.Printf("pool %q — %d device(s) online, %s pooled memory (%s available)\n\n",
		view.Pool, view.OnlineNodes, fmtBytes(view.TotalMem), fmtBytes(view.AvailableMem))
	fmt.Printf("%-18s %-10s %-12s %-8s %s\n", "DEVICE", "STATUS", "FREE MEM", "ACTIVE", "MODELS")
	for _, n := range view.Nodes {
		st := "online"
		if !n.OllamaOK {
			st = "no-ollama"
		}
		if !n.Sharing {
			st = "consume"
		}
		models := ""
		for i, m := range n.Models {
			if i > 0 {
				models += ", "
			}
			models += m.Name
		}
		name := n.Name
		if n.Self {
			name += "*"
		}
		fmt.Printf("%-18s %-10s %-12s %-8d %s\n",
			name, st, fmtBytes(n.Resources.MemAvailable), n.Active, models)
	}
	if len(view.Models) > 0 {
		fmt.Printf("\nservable models: %d — try: curl localhost:%d/v1/models\n", len(view.Models), port)
	}
	return 0
}

func fmtBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
