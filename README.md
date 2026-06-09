# 🍲 Potluck

**Pool the devices on your WiFi into one LLM server.**

Everyone on the network runs one binary. Potluck discovers every device automatically, shows a live dashboard of who's sharing what (GPU, RAM, models), and exposes the whole pool as a single OpenAI-compatible and Ollama-compatible API. Point your IDE, chat client, or script at any node — the request runs on the best device that can serve it.

```
your laptop ──┐
roommate's PC ├──> one endpoint: http://localhost:11444/v1  ──> the pool answers
old Mac mini ─┘
```

> Status: **alpha**, v0.2. Phase 1 (pooling) and Phase 2 (splitting one model across devices that can't fit it alone) both work today.

## Why

Local models are great until the model you want doesn't fit your machine — while the gaming PC across the room sits idle. Existing distributed tools (exo, llama.cpp RPC) assume one person owns every device and require manual setup. Potluck is built for *groups*: zero-config discovery, per-device consent (`--share=false` to consume without contributing), and a dashboard everyone can see.

## Quickstart

Requirements: [Ollama](https://ollama.com) installed and running on each device that shares compute. Go 1.24+ to build from source (binary releases coming).

```sh
# on every device on the WiFi
go install github.com/prathyushnallamothu/potluck/cmd/potluck@latest
potluck
```

That's it. Each agent finds the others via mDNS. Then:

- **Dashboard**: open http://localhost:11444 — live view of every device, its memory, GPUs, and models
- **Pool status in the terminal**: `potluck status`
- **Use it** from any OpenAI client:

```sh
curl http://localhost:11444/v1/chat/completions \
  -d '{"model": "llama3.2", "messages": [{"role": "user", "content": "hi"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:11444/v1", api_key="potluck")
resp = client.chat.completions.create(model="llama3.2",
    messages=[{"role": "user", "content": "hi"}])
```

Ollama-style endpoints (`/api/chat`, `/api/generate`, `/api/tags`, …) work too, so existing Ollama tooling can point at the pool.

## Running models too big for any one device

If a requested model fits no single device, Potluck runs it as a **pipeline across devices** (llama.cpp RPC under the hood): the node holding the weights becomes the driver, other nodes lend their GPU/RAM as workers, and layers are distributed by free memory. This happens automatically on `/v1/*` requests — no extra commands.

Requirements per participating device: llama.cpp built with RPC support
(`cmake -B build -DGGML_RPC=ON && cmake --build build --target rpc-server llama-server`),
with `rpc-server` (worker capability) and `llama-server` (driver capability) on `$PATH` or pointed at via `--rpc-server-bin` / `--llama-server-bin`. Devices without these binaries still participate in normal pooling.

Notes:
- The driver reuses the GGUF weights already in its Ollama store — no second download.
- First load streams weights to workers over WiFi and can take minutes for big models; pipelines then stay warm for 10 minutes of idle before teardown.
- Split models are served on the OpenAI endpoints (`/v1/*`) only.
- Worker/driver process logs land in `~/.potluck/logs/` for debugging.

## How it works

- Every device runs the same agent — **no central server**. Any node's endpoint serves the whole pool.
- Agents advertise and discover each other with **mDNS** (`_potluck._tcp`), then exchange state (free RAM, GPUs, pulled models, loaded models, load) over HTTP every few seconds.
- When a request arrives, the **scheduler** picks the best node that has the model: model already loaded in memory beats cold start, then least-busy, then most free memory.
- Each device's Ollama stays bound to localhost. Cross-device execution goes through the target agent's authenticated proxy, so only Potluck agents talk across the network.
- A device that closes its lid simply ages out of the registry within ~20 s and stops receiving requests.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--name` | hostname | Device name shown in the pool |
| `--port` | `11444` | Agent port (dashboard + API) |
| `--ollama` | `http://127.0.0.1:11434` | Local Ollama URL |
| `--pool` | `default` | Pool name — devices only join matching pools |
| `--token` | _(none)_ | Shared secret peers must present to run jobs on this device |
| `--share` | `true` | Set `false` to use the pool without contributing |
| `--rpc-server-bin` | find on PATH | llama.cpp `rpc-server` (lend memory to split pipelines) |
| `--llama-server-bin` | find on PATH | llama.cpp `llama-server` (drive split pipelines) |
| `--rpc-port` | `50752` | Port for `rpc-server` when recruited into a pipeline |
| `--ctx` | `4096` | Context size for split pipelines |
| `--models-dir` | `~/.ollama/models` | Ollama store to reuse GGUF blobs from |

## Security model (read this)

Potluck is designed for **trusted local networks** (your home, your team's office). Today:

- `--token` gates remote execution on your device — set the same token on every node of a private pool.
- `rpc-server` (split pipelines) is llama.cpp's protocol and is unauthenticated by design; Potluck only starts it when a token-bearing peer recruits the node, but on a hostile LAN anyone could connect to it while it runs. Don't enable split on networks you don't trust.
- Prompts and responses are visible to the device that serves them (that's physics, not a bug — the model runs there). Don't send secrets through devices you don't trust.
- Traffic between agents is plain HTTP on your LAN; mTLS between agents is on the roadmap.

## Roadmap

- **Phase 1 — pooling (shipped, v0.1)**: discovery, dashboard, scheduling, OpenAI/Ollama gateway
- **Phase 2 — model splitting (shipped, v0.2)**: models too big for any single device run as a llama.cpp RPC pipeline across devices
- **Phase 3 — the social layer (next)**: lender policies (only share on AC power / when idle), fairness quotas and credits, mTLS, native installers and menubar app, Ollama-dialect support for split models

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for design details and [CONTRIBUTING.md](CONTRIBUTING.md) to help build it.

## Comparison

| | Potluck | exo | llama.cpp RPC | Ollama |
|---|---|---|---|---|
| Zero-config LAN discovery | ✅ | ✅ | ❌ manual IPs | — |
| Built for multi-user sharing (consent, dashboard) | ✅ | ❌ single owner | ❌ | — |
| Works with existing Ollama installs | ✅ | ❌ | ❌ | ✅ |
| Split one model across devices | ✅ | ✅ | ✅ | ❌ |

## License

[MIT](LICENSE)
