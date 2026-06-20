# Architecture

agentry gives an AI agent a real Linux sandbox on hardware you control. This
doc explains the pieces, how a request flows end to end, the ports involved,
and the security model.

## The pieces

```
 ┌───────────────────────────┐         ┌──────────────────────────────────┐
 │  Your laptop              │         │  Your machine (VPS / box / laptop) │
 │                           │         │                                    │
 │  AI client (Claude Code,  │         │   provisioner  ──►  Docker / K8s    │
 │  Cursor, Roo, …)          │         │   :8002              │             │
 │      │ MCP/stdio          │         │      │ reverse-proxy │             │
 │      ▼                    │  mTLS   │      ▼               ▼             │
 │  agentry CLI ─────────────┼──tunnel─┼──► bridge ──► runtime (in sandbox) │
 │                           │         │              :8080 HTTP API        │
 └───────────────────────────┘         └──────────────────────────────────┘
```

### runtime (`cmd/runtime`)
The daemon that runs **inside every sandbox container**. It exposes the
sandbox's capabilities as an HTTP API on `:8080` (override with `-addr` or
`SANDBOX_PORT`):

- **shell** — run commands, background processes, stream logs, an
  interactive PTY over WebSocket (`/v1/shell/*`).
- **files** — read/write/list/grep/edit, HTTP range + multipart
  (`/v1/file/*`).
- **ports** — discover what's listening, wait for a port (`/v1/ports`).
- **projects** — scaffold/start/stop framework projects (Next, FastAPI,
  Streamlit, static, automation) (`/v1/project/*`).
- **code interpreter** — a persistent Jupyter kernel (`/v1/code`).
- **editor** — lazy-starts code-server (in-browser VS Code) on loopback,
  served back through the app-proxy (`/v1/ide/*`).

The runtime trusts its caller: it's bound inside the container and only the
provisioner reaches it. When the provisioner sets a runtime API key, the
runtime rejects any request without it.

### provisioner (`cmd/provisioner`)
The daemon that runs **on your machine** and owns sandbox lifecycle. It
listens on `127.0.0.1:8002` (loopback by default; override with
`PROVISIONER_ADDR`) and:

- creates/destroys sandboxes via a **backend**. `BACKEND=docker` (the local
  Docker daemon) is the supported backend today; each sandbox runs the
  runtime image. A Kubernetes backend — and the stronger-isolation runtimes
  that ride on it, Kata and gVisor — are on the roadmap (`BACKEND=k8s`
  currently returns "coming soon"; see [Backends](#backends)).
- reverse-proxies `/api/sandboxes/{id}/runtime/*` to that sandbox's runtime,
  so the runtime port never has to be exposed.
- manages bindings, build/deploy, TTL reaping, and (in hosted mode) an
  outbound mTLS tunnel to the bridge.

The control API (create/delete/proxy) is loopback-only on purpose: in
production it's reached over the outbound tunnel, never by dialing the port.

### bridge (`cmd/bridge`)
A **stateless mTLS/yamux routing pivot**. It lets a CLI on your laptop reach
a provisioner that's behind NAT: the provisioner dials *out* to the bridge
and holds the tunnel open; the CLI's requests are routed down it by an
`X-Cluster` header. The bridge has no database and issues no certificates —
it validates client certs against a CA it loads at startup
(`CA_CERT_URL`/`CA_CERT_PATH`) and routes. `DEV_MODE=1` runs it as plain
HTTP with all mTLS/tenancy checks off, for local use.

> For a single machine you usually don't need the bridge at all — the CLI
> (or `curl`) can talk to the provisioner directly. The bridge matters when
> the agent and the compute are on different networks.

### CLI (`cmd/cli`, the `agentry` binary)
What you run. It speaks **MCP over stdio** so AI clients can call sandbox
tools, and adds human commands: `agentry sh` (shell into a sandbox),
`agentry logs -f`, `agentry vsc` (open the in-browser editor). Config lives
at `~/.agentry/agentry.json` (`AGENTRY_CONFIG` overrides).

### authproxy (`cmd/authproxy`)
An optional auth sidecar that can be baked in front of a **deployed** app to
add email/password + OAuth login. Not part of the sandbox path; only
relevant to the deploy feature.

## Data plane: how a tool call flows

Hosted (through the bridge):

```
AI client → agentry CLI → bridge (mTLS tunnel) → provisioner
          → /api/sandboxes/{id}/runtime/v1/... → runtime (in the sandbox)
```

Local (direct, no bridge):

```
curl / CLI → provisioner :8002 → /api/sandboxes/{id}/runtime/v1/... → runtime
```

The runtime-proxy path is the key seam: everything the agent does inside a
sandbox is an HTTP call to `…/runtime/…`, which the provisioner forwards to
the right container. WebSockets (PTY, dev-server preview) and SSE (log
follow) pass through the same path.

## Ports

| Component | Default | Notes |
|---|---|---|
| runtime | `:8080` | inside the sandbox container; not exposed to the host network |
| provisioner | `127.0.0.1:8002` | loopback; `PROVISIONER_ADDR` to change |
| bridge | `:8090` (DEV_MODE) / `:443`+`:80` (prod) | `-listen`, `HTTPS_LISTEN`, `HTTP_LISTEN` |
| sandbox app ports | ephemeral | mapped by the backend; surfaced via the runtime ports API |

## Security model

- **Loopback by default.** The provisioner's control API binds `127.0.0.1`,
  so the create/delete/proxy surface isn't exposed to the LAN.
- **mTLS between CLI ↔ bridge ↔ provisioner** in hosted mode. The bridge
  enforces client-cert identity (role + cluster), so one tenant can't route
  to another. `DEV_MODE` disables this for local single-user use.
- **Runtime API key.** The provisioner auto-generates a key (persisted under
  its cert dir), injects it into each sandbox, and stamps it on every call —
  so a co-located process or SSRF that reaches the runtime's loopback port
  can't drive it. With no cert dir (pure local dev) the key is empty and the
  runtime accepts unauthenticated calls.
- **Sandbox isolation** comes from the backend. Today that's Docker, with a
  hardened security posture (cap-drop, no-new-privileges, seccomp) plus an
  optional egress policy and shm sizing. Stronger-isolation runtimes
  (gVisor, Kata) will arrive with the Kubernetes backend — see
  [Backends](#backends).
- **Cert issuance is not in this repo.** Enrollment + CA signing live in the
  closed control plane; the OSS bridge only *consumes* a CA cert. For a
  self-hosted mTLS setup you supply your own CA (see RUNNING-LOCALLY.md).

## Backends

The provisioner is backend-agnostic behind a small `Backend` interface
(create/delete pod + service, exec, list, annotations). Today there is one
production backend:

| Backend | Status | Notes |
|---|---|---|
| **Docker** (`BACKEND=docker`) | **Supported** | Single host. Hardened security posture, egress policy, shm sizing, in-sandbox image builds, and deployments all work. This is what the hosted product runs. |
| **Kubernetes** (`BACKEND=k8s`) | **Coming soon** | A pod/service implementation exists but is incomplete (no security-context hardening, egress, shm sizing, or deploy path) and untested — it's gated off and returns "coming soon." A contributor opt-in (`BACKEND=k8s-experimental`) exposes the WIP. |
| **Kata / gVisor** | **Coming soon** | Stronger sandbox isolation, selected per-pod via `runtime_class` once the Kubernetes backend lands. |

If you want to help land the Kubernetes/Kata/gVisor backends, the interface
to implement is `Backend` in `pkg/provisioner/provisioner.go`; the Docker
backend (`docker_client.go`) is the reference for the full feature set
(security posture, egress, shm, builder mode).

## What's open vs. hosted (recap)

Everything above ships in this repo and runs standalone. The hosted control
plane adds accounts, the web dashboard, device enrollment + certificate
issuance, and a managed bridge — convenience and multi-tenancy on top of the
same engine, not a dependency of it.
