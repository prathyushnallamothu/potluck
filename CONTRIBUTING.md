# Contributing to Potluck

Thanks for helping build shared local AI! All contributions are welcome — code, docs, bug reports, and real-world testing on weird network setups are all equally valuable.

## Getting started

```sh
git clone https://github.com/prathyushnallamothu/potluck
cd potluck
make test          # vet + unit tests
make run           # build and start an agent locally
```

You need Go 1.24+ and (for live testing) [Ollama](https://ollama.com) running locally. Two terminals on one machine can't form a pool (one mDNS instance per host) — test multi-node behavior with a second device or a VM.

## Project layout

```
cmd/potluck/          CLI entrypoint (agent + status subcommand)
internal/agent/       wiring: loops, HTTP server, pool view
internal/discovery/   mDNS advertise + browse
internal/registry/    node state, the pool's source of truth
internal/resources/   hardware + Ollama probing
internal/scheduler/   request placement policy
internal/gateway/     OpenAI/Ollama API surface + peer proxy
internal/dashboard/   embedded web UI (single HTML file, no build step)
docs/                 architecture and design notes
```

## Guidelines

- Keep the agent a **single static binary** — no runtime dependencies beyond Ollama. Think hard before adding a Go dependency.
- The dashboard stays a **single embedded HTML file** with no build step. If it ever genuinely needs a framework, that's a discussion issue first.
- Scheduling changes need a unit test in `internal/scheduler` demonstrating the behavior.
- Run `make test` before sending a PR; CI runs the same.
- One change per PR, with a description of *why*.

## Good first issues

- Battery/AC-power detection feeding a "pause sharing on battery" policy
- `potluck pull <model>` that targets the pool node with the most free disk/memory
- Prometheus `/metrics` endpoint
- Windows testing and packaging (Homebrew/Scoop)

## Roadmap work (bigger bites)

- **Phase 2 — model splitting**: orchestrating `llama.cpp` `rpc-server` workers for models that fit no single device. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#phase-2-model-splitting).
- **Phase 3 — fairness + security**: mTLS between agents, per-user quotas, lending credits.

Open an issue to discuss before starting roadmap-sized work.

## Code of conduct

Be kind. The project is named after a meal everyone shares — act accordingly.
