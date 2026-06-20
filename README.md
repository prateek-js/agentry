# agentry

**Self-hosted compute for AI agents.** Give an AI coding agent (Claude Code,
Cursor, Roo, or anything that speaks [MCP](https://modelcontextprotocol.io))
a real Linux sandbox on hardware *you* control — your laptop, a VPS, a
bare-metal box — to build, run, and ship real apps. Code and data stay on
your machine.

```
 AI client (Claude Code / Cursor / Roo …)
        │  MCP over stdio
        ▼
   agentry CLI ──mTLS──► bridge ──mTLS──► provisioner ──► Docker / K8s
   (your laptop)        (routing pivot)   (your machine)    runs the sandbox
                                                                  │
                                                            agentry runtime
                                                         (HTTP API in-container)
```

The sandbox is a full Linux box with Node, Python, a build toolchain, an
in-browser editor, and an HTTP API the agent drives to write files, run
commands, start dev servers, and deploy.

## Open source vs. hosted

This repo is the **engine**, and it runs standalone — you can build and run
the whole thing locally with nothing but Go and Docker (see
[Run it locally](#run-it-locally)).

A separate, **closed-source control plane** (the hosted service at
`app.agentry.run`) adds the multi-tenant web dashboard, accounts, device
enrollment + certificate issuance, and the managed `bridge` — i.e. the
turnkey "paste one command on any machine and drive it from the web" SaaS.
The OSS engine does not require it.

| This repo (`agentry`, OSS) | Hosted (`app.agentry.run`, closed) |
|---|---|
| CLI, runtime, provisioner, bridge, authproxy | Web dashboard, accounts, billing |
| Sandbox lifecycle + the full runtime API | Device enrollment + CA / cert issuance |
| Self-host locally with Go + Docker | Managed multi-tenant tunnel + routing |

## Components

| Path | Component | What it does |
|---|---|---|
| `cmd/runtime` | **runtime** | Runs inside every sandbox container. HTTP API for shell, files, ports, a code interpreter, and the in-browser editor. |
| `cmd/provisioner` | **provisioner** | Runs on your machine. Creates/destroys sandboxes (Docker or Kubernetes backend) and reverse-proxies to each sandbox's runtime. |
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
- **[docs/RUNNING-LOCALLY.md](docs/RUNNING-LOCALLY.md)** — run the engine on your machine, end to end, with no third-party services.
- **[docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)** — build from source, the `make` targets, and the repo layout.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** · **[SECURITY.md](SECURITY.md)**

## Status

Pre-alpha. The architecture is stable; APIs may still move.

## License

[Apache License 2.0](LICENSE).
