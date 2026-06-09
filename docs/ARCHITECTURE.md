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

## Phase 2: model splitting

Goal: serve models that fit no single device by running them as a layer pipeline across devices (pipeline parallelism — only ~kB of activations cross the network per token, which WiFi handles; tensor parallelism does not fit WiFi).

Plan:
- Ship a `llama.cpp` `rpc-server` worker alongside the agent (or orchestrate an existing install).
- The scheduler gains a second mode: when no node fits M whole, partition layers proportionally to each volunteer's free memory and launch a pipeline.
- Weight shards are cached on lenders after first use; first-run distribution is the cold-start cost.
- Churn handling: if a stage drops mid-generation, fail the request fast, re-partition without the lost node, and retry once.

## Phase 3: the social layer

- Lender policies: share only on AC power / when idle / time windows
- Fairness: per-user accounting; lending earns priority
- mTLS between agents with a pool-join PIN
- Native packaging: menubar/tray app, Homebrew, Scoop
