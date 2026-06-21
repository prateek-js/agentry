# agentry

[![CI](https://github.com/agentry-ai/agentry/actions/workflows/ci.yml/badge.svg)](https://github.com/agentry-ai/agentry/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/agentry-ai/agentry)](https://goreportcard.com/report/github.com/agentry-ai/agentry)
[![Go Reference](https://pkg.go.dev/badge/github.com/agentry-ai/agentry.svg)](https://pkg.go.dev/github.com/agentry-ai/agentry)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**Self-hosted compute for AI agents.** Give an AI coding agent (Claude Code,
Cursor, Roo, or anything that speaks [MCP](https://modelcontextprotocol.io))
a real Linux sandbox on hardware *you* control — your laptop, a VPS, a
bare-metal box — to build, run, and ship real apps. Code and data stay on
your machine.

```
 AI client (Claude Code / Cursor / Roo …)
        │  MCP over stdio
        ▼
   agentry CLI ──mTLS──► bridge ──mTLS──► provisioner ──► Docker
   (your laptop)        (routing pivot)   (your machine)    runs the sandbox
                                                                  │
                                                            agentry runtime
                                                         (HTTP API in-container)
```

The sandbox is a full Linux box with Node, Python, a build toolchain, an
in-browser editor, and an HTTP API the agent drives to write files, run
commands, start dev servers, and deploy.

## Open source vs. hosted

**This repo is the whole engine.** Sandboxes, the runtime API, building and
running apps — all of it is here and runs standalone with nothing but Go and
Docker (see [Run it locally](#run-it-locally)).

The hosted service at `app.agentry.run` is **convenience, not capability.**
It's a closed-source control plane that runs the multi-tenant pieces for you
— a web dashboard + accounts, automatic device enrollment + certificate
issuance, the managed tunnel, and deploy ingress — so you don't have to
stand any of that up. It adds nothing to what a sandbox can *do*.

| This repo (`agentry`, OSS) — the engine | Hosted (`app.agentry.run`, closed) — convenience |
|---|---|
| CLI, runtime, provisioner, bridge, authproxy | Web dashboard, accounts, billing |
| Full sandbox lifecycle + runtime API | One-paste onboarding |
| Build + run apps | Automatic enrollment + CA / cert issuance |
| Run the bridge yourself | Managed multi-tenant tunnel + deploy ingress |

Want the hosted experience on your own hardware? **[docs/SELF-HOSTING.md](docs/SELF-HOSTING.md)**
lists exactly which pieces to bring to replicate each part.

## Components

| Path | Component | What it does |
|---|---|---|
| `cmd/runtime` | **runtime** | Runs inside every sandbox container. HTTP API for shell, files, ports, a code interpreter, and the in-browser editor. |
| `cmd/provisioner` | **provisioner** | Runs on your machine. Creates/destroys sandboxes (Docker backend today; Kubernetes/Kata/gVisor coming soon) and reverse-proxies to each sandbox's runtime. |
| `cmd/bridge` | **bridge** | Stateless mTLS/yamux routing pivot that lets a remote CLI reach a provisioner behind NAT. Optional for local use. |
| `cmd/cli` | **agentry** CLI | What you run. Speaks MCP over stdio to AI clients; `sh`/`logs`/`vsc` to look inside a sandbox. |
| `cmd/authproxy` | **authproxy** | Optional auth sidecar baked into deployed apps (email/password + OAuth). |
| `pkg/` | — | Shared libraries (tunnel, mcp, broker, telemetry, …). |
| `docker/` | — | Dockerfiles for the runtime + provisioner images. |
| `docs/` | — | Architecture, local-dev, and contributor docs. |

## Run it locally

No accounts, no cloud, no certificates. You need **Go 1.26+** and **Docker**.

```sh
# 1. Build the sandbox runtime image (bakes Node, Python, build tools, editor)
make runtime-image

# 2. Prove it works end-to-end: starts the provisioner, creates a sandbox,
#    runs a command inside it, tears down.
make smoke
```

Expected tail:

```
→ running a command inside the sandbox
{"success":true, ... "output":"OK_aarch64\r\nv22.12.0\r\nPython 3.13.14","exit_code":0}
✓ local smoke passed
```

To keep the engine running and drive it yourself:

```sh
make dev      # provisioner on http://127.0.0.1:8002 (Docker backend)
```

Then create a sandbox and run a command in it:

```sh
curl -s localhost:8002/api/sandboxes -d '{"sandbox_id":"demo"}'
curl -s localhost:8002/api/sandboxes/demo/runtime/v1/shell/exec \
  -d '{"command":"echo hello && python3 -c \"print(2+2)\"","timeout":30}'
curl -s -X DELETE localhost:8002/api/sandboxes/demo
```

Full walkthrough, env-var reference, and how to wire an AI client →
**[docs/RUNNING-LOCALLY.md](docs/RUNNING-LOCALLY.md)**.

## Documentation

- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — how the components fit, the data plane, ports, and the security model.
- **[docs/SECURITY-MODEL.md](docs/SECURITY-MODEL.md)** — the zero-trust posture: what stays on your hardware and what's authenticated.
- **[docs/SERVICE-AND-ENV-MODEL.md](docs/SERVICE-AND-ENV-MODEL.md)** — how bindings + env vars are wired straight into the runtime (the platform never sees them).
- **[docs/RUNNING-LOCALLY.md](docs/RUNNING-LOCALLY.md)** — run the engine on your machine, end to end, with no third-party services.
- **[docs/SELF-HOSTING.md](docs/SELF-HOSTING.md)** — what to bring to get the hosted experience (remote access, deploy URLs) on your own hardware.
- **[docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)** — build from source, the `make` targets, and the repo layout.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** · **[SECURITY.md](SECURITY.md)**

## Status

Pre-alpha. The architecture is stable; APIs may still move.

## License

[Apache License 2.0](LICENSE).
