# Potluck architecture

## Design principles

1. **No central server.** Every device runs the identical agent. Any node's endpoint serves the whole pool; any node can leave without taking the pool down.
2. **Ollama stays local.** Each device's Ollama binds to localhost as usual. Only Potluck agents talk across the network, through an authenticated proxy. Users don't reconfigure anything they already run.
3. **Consent is first-class.** Sharing is a per-device decision (`--share`), visible to everyone on the dashboard.
4. **Best-effort everywhere.** A failed GPU probe, an unreachable peer, or a dead Ollama degrades that node's advertised capability — it never crashes an agent or the pool.

## Components

```
┌─────────────────────────── one agent process per device ───────────────────────────┐
│                                                                                     │
│  discovery        registry          scheduler         gateway          dashboard    │
│  mDNS adv/browse  node states       picks best node   /v1/*, /api/*    embedded UI  │
│       │               ▲                  ▲             /proxy/ollama/*       │       │
│       └── hints ──────┤                  │                  │                │       │
│                       │                  └──── Pick() ──────┤                │       │
│  resources ── self state every 3s ──▶ registry ◀── peer poll every 3s        │       │
│                                                                                     │
└──────────────────────────────────────────┬──────────────────────────────────────────┘
                                           │ localhost only
                                       Ollama daemon
```

### Discovery (`internal/discovery`)
Agents register `_potluck._tcp` in mDNS with TXT records (`id`, `name`, `pool`, `version`) and continuously browse in 10-second windows. Discovery yields only *hints* (ID + address); authoritative state always comes from polling the peer.

### State exchange (`internal/agent`, `internal/registry`)
Every agent:
- refreshes its own state every 3 s (RAM via gopsutil, GPUs via `nvidia-smi` / Apple Silicon detection, models via Ollama `/api/tags`, loaded models via `/api/ps`)
- polls every known peer's `GET /api/state` every 3 s
- prunes peers not heard from in 40 s; the scheduler already ignores anything staler than 20 s

This is O(n²) polling, which is fine for the target size (a WiFi network, ≤ ~30 devices). Gossip-style fan-out is a later optimization if anyone runs pools bigger than that.

### Scheduling (`internal/scheduler`)
For a request naming model M, eligible nodes are: sharing, Ollama up, state fresh. Ranking:
1. M already loaded in memory (avoids a multi-GB model swap)
2. fewest in-flight requests
3. most available memory

If no node has M, the client gets a 404 listing what the pool *can* serve — errors should teach.

### Gateway (`internal/gateway`)
Both API dialects on every node:
- OpenAI: `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`
- Ollama: `/api/chat`, `/api/generate`, `/api/embeddings`, `/api/embed`, `/api/tags`

The receiving node reads the body (capped at 64 MiB), extracts `model`, schedules, and forwards: to its own Ollama directly, or to the chosen peer's `/proxy/ollama/*` with the pool token header. Responses stream with per-chunk flushing, so token streaming works end-to-end. Model listings are pool-wide merges.

### Trust boundary
`/proxy/ollama/*` is the only way peers execute on your device, and it checks `X-Potluck-Token` when a token is configured. The serving device necessarily sees plaintext prompts — inference happens there. The roadmap adds mTLS for transport; prompt privacy from the *serving device* is impossible without confidential computing, and the docs say so honestly.

## Phase 2: model splitting (implemented in v0.2)

Serves models that fit no single device by running them across devices on
llama.cpp's RPC backend (only ~kB of activations cross the network per token,
which WiFi handles; tensor-parallel sync traffic would not).

Flow, for a `/v1/*` request naming model M:

1. `scheduler.PlanSplit` checks every eligible holder of M against
   `Need(size) = size × 1.25` vs `Usable(free) = free × 0.8`. If someone fits
   M whole, Phase 1 routing applies. Otherwise it picks a **driver** (a holder
   of M with `llama-server` installed, most free memory first) and recruits
   **workers** (nodes with `rpc-server`, by free memory) until capacity covers
   the need.
2. The gateway asks the driver's agent (`POST /api/pipeline/ensure`) to bring
   the pipeline up. The driver recruits each worker (`POST /api/rpc/start`,
   token-gated), which starts `rpc-server` exposing only devices with real
   memory — zero-memory devices like BLAS must be excluded or drivers
   schedule tensors onto them and crash the worker.
3. The driver resolves M's GGUF blob from its local Ollama store (no second
   download) and launches `llama-server --rpc w1,w2,… -ngl 999`, which
   distributes layers across workers and its own backend by free memory.
   Readiness is polled via `/health`; big models stream weights to workers
   during this window.
4. The request proxies into the pipeline (`/proxy/llama/*` on the driver,
   model named in the `X-Potluck-Model` header). Pipelines stay warm and are
   torn down after 10 minutes idle; agent shutdown kills all child processes.
   Child process output is captured in `~/.potluck/logs/`.

Failure handling: a worker that can't be recruited is skipped (llama-server
fails loudly if the remainder doesn't fit); a pipeline that dies during load
reports the error and is relaunched on the next request; if splitting is
impossible (no binaries), the gateway falls back to Phase 1 single-node
routing and logs why. Split pipelines speak the OpenAI dialect only —
`/api/*` requests for oversized models fall back to Phase 1.

## Phase 3: the social layer

- Lender policies: share only on AC power / when idle / time windows
- Fairness: per-user accounting; lending earns priority
- mTLS between agents with a pool-join PIN
- Native packaging: menubar/tray app, Homebrew, Scoop
