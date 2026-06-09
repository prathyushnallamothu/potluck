// Package resources probes the local machine (CPU, RAM, GPU) and the local
// Ollama daemon (models pulled, models loaded). Everything is best-effort:
// a probe failure degrades the node's advertised capabilities, never crashes.
package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"

	"github.com/prathyushnallamothu/potluck/internal/registry"
)

// Collect gathers hardware facts about this machine.
func Collect() registry.Resources {
	res := registry.Resources{
		OS:     runtime.GOOS,
		Arch:   runtime.GOARCH,
		NumCPU: runtime.NumCPU(),
	}
	if h, err := os.Hostname(); err == nil {
		res.Hostname = h
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		res.MemTotal = vm.Total
		res.MemAvailable = vm.Available
	}
	if infos, err := cpu.Info(); err == nil && len(infos) > 0 {
		res.CPUModel = strings.TrimSpace(infos[0].ModelName)
	}
	res.GPUs = detectGPUs(res)
	return res
}

// detectGPUs finds accelerators. NVIDIA via nvidia-smi; Apple Silicon is
// reported as one Metal device with unified memory (free ≈ available RAM).
func detectGPUs(res registry.Resources) []registry.GPU {
	var gpus []registry.GPU
	if g, ok := nvidiaGPUs(); ok {
		gpus = append(gpus, g...)
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		name := "Apple Silicon"
		if res.CPUModel != "" {
			name = res.CPUModel
		}
		gpus = append(gpus, registry.GPU{
			Name:     name + " (unified memory)",
			Kind:     "metal",
			MemTotal: res.MemTotal,
			MemFree:  res.MemAvailable,
		})
	}
	return gpus
}

func nvidiaGPUs() ([]registry.GPU, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.total,memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, false
	}
	var gpus []registry.GPU
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) != 3 {
			continue
		}
		totalMB, _ := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		freeMB, _ := strconv.ParseUint(strings.TrimSpace(parts[2]), 10, 64)
		gpus = append(gpus, registry.GPU{
			Name:     strings.TrimSpace(parts[0]),
			Kind:     "cuda",
			MemTotal: totalMB * 1024 * 1024,
			MemFree:  freeMB * 1024 * 1024,
		})
	}
	return gpus, len(gpus) > 0
}

// OllamaState is the result of probing the local Ollama daemon.
type OllamaState struct {
	OK      bool
	Version string
	Models  []registry.Model
	Loaded  []string
}

type ollamaTagsResponse struct {
	Models []struct {
		Name    string `json:"name"`
		Size    uint64 `json:"size"`
		Details struct {
			Family        string `json:"family"`
			ParameterSize string `json:"parameter_size"`
			Quantization  string `json:"quantization_level"`
		} `json:"details"`
	} `json:"models"`
}

type ollamaPSResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// ProbeOllama queries the local Ollama HTTP API for its version, pulled
// models, and currently loaded models.
func ProbeOllama(ctx context.Context, baseURL string) OllamaState {
	client := &http.Client{Timeout: 4 * time.Second}
	st := OllamaState{}

	var ver struct {
		Version string `json:"version"`
	}
	if err := getJSON(ctx, client, baseURL+"/api/version", &ver); err != nil {
		return st
	}
	st.OK = true
	st.Version = ver.Version

	var tags ollamaTagsResponse
	if err := getJSON(ctx, client, baseURL+"/api/tags", &tags); err == nil {
		for _, m := range tags.Models {
			st.Models = append(st.Models, registry.Model{
				Name:       m.Name,
				Size:       m.Size,
				Family:     m.Details.Family,
				Parameters: m.Details.ParameterSize,
				Quant:      m.Details.Quantization,
			})
		}
	}

	var ps ollamaPSResponse
	if err := getJSON(ctx, client, baseURL+"/api/ps", &ps); err == nil {
		for _, m := range ps.Models {
			st.Loaded = append(st.Loaded, m.Name)
		}
	}
	return st
}

func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
