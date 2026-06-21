# Running agentry locally

You can run the entire sandbox engine on one machine with **no control
plane, no bridge, no certificates, and no third-party services** — just Go
and Docker. This is the path for trying agentry, developing on it, or
self-hosting a single-machine setup.

## Prerequisites

- **Go 1.26+**
- **Docker** (any recent Docker Engine / Docker Desktop / OrbStack)

## 1. Build the sandbox runtime image

Every sandbox is a container started from the runtime image. Build it once:

```sh
make runtime-image      # tags agentry/runtime:latest
```

This bakes the sandbox userland — Node, Python, a build toolchain, the
in-browser editor, etc. The first build takes a few minutes.

## 2. Smoke test (one command)

```sh
make smoke
```

This starts the provisioner, creates a sandbox, runs a command inside it,
and tears everything down. Success looks like:

```
→ running a command inside the sandbox
{"success":true, ... "output":"OK_aarch64\r\nv22.12.0\r\nPython 3.13.14","exit_code":0}
✓ local smoke passed
```

If that passes, the engine works on your machine.

## 3. Run the engine and drive it

Start the provisioner (Docker backend, loopback API):

```sh
make dev
# → provisioner on http://127.0.0.1:8002
```

In another terminal, talk to it directly over HTTP. The provisioner exposes
sandbox lifecycle under `/api/sandboxes`, and reverse-proxies each sandbox's
runtime under `/api/sandboxes/{id}/runtime/...`.

```sh
# create a sandbox
curl -s localhost:8002/api/sandboxes -d '{"sandbox_id":"demo"}'

# run a command inside it (the runtime's shell API, via the proxy)
curl -s localhost:8002/api/sandboxes/demo/runtime/v1/shell/exec \
  -d '{"command":"echo hi && node --version","timeout":30}'

# write a file
curl -s localhost:8002/api/sandboxes/demo/runtime/v1/file/write \
  -d '{"path":"/workspace/hello.txt","content":"hello"}'

# list sandboxes, then delete
curl -s localhost:8002/api/sandboxes
curl -s -X DELETE localhost:8002/api/sandboxes/demo
```

That's the same path an AI client drives through MCP — just without the
tunnel in front of it.

## Wiring an AI client (MCP)

The `agentry mcp` command exposes the sandbox tools to any MCP client over
stdio. In its default form it connects to the hosted control plane (for
accounts + the managed tunnel). For a **local, no-control-plane** setup you
run the provisioner yourself (above) and point your tooling at
`127.0.0.1:8002`. The hosted onboarding (`agentry init` / `agentry login`)
talks to `app.agentry.run` and is **not** required for the local engine —
see [What needs the hosted control plane](#what-needs-the-hosted-control-plane).

## Optional: run the bridge locally

You only need the bridge if the agent and the compute are on different
networks. For local use it's unnecessary (talk to the provisioner directly).
To run it anyway in dev mode (plain HTTP, no mTLS):

```sh
DEV_MODE=1 go run ./cmd/bridge -listen :8090
```

`DEV_MODE` disables TLS, mTLS, and all tenancy checks, and refuses to start
if a production TLS/deploy domain is configured — it's strictly for local
single-user use.

## Environment variable reference

### provisioner (`cmd/provisioner`)

| Variable | Default | Purpose |
|---|---|---|
| `BACKEND` | `docker` | The supported backend. `k8s`/`kata`/`gvisor` return "coming soon". |
| `SANDBOX_IMAGE` | `agentry/runtime:latest` | Image each sandbox runs. |
| `PROVISIONER_ADDR` | `127.0.0.1:8002` | Listen address. |
| `NODE_HOST` | `localhost` (docker) | Host clients use to reach sandbox ports. |
| `SANDBOX_DEFAULT_SHM_SIZE` | `2Gi` | `/dev/shm` per sandbox (`0` = Docker default). |
| `SANDBOX_DEFAULT_CREDS_DIR` | — | Host dir bind-mounted read-only into every sandbox at `/etc/sandbox/creds`. |
| `REAPER_INTERVAL_SECONDS` | `60` | TTL reaper sweep interval (`0` disables). |
| `AGENTRY_SANDBOX_BUILDER_MODE` | off | Permissive sandbox posture so in-sandbox image builds work. |
| `AGENTRY_CERT_DIR` | — | Enables the mTLS/enroll flow + runtime API key. **Leave unset for local dev.** |
| `AGENTRY_BRIDGE_URL` | — | Dial out to a bridge. Leave unset for local. |
| `AGENTRY_RUNTIME_API_KEY` | — | Override the auto-managed runtime key. |

### runtime (`cmd/runtime`) — runs inside the sandbox

| Variable / flag | Default | Purpose |
|---|---|---|
| `-addr` / `SANDBOX_PORT` | `:8080` | Listen address. |

### bridge (`cmd/bridge`)

| Variable / flag | Default | Purpose |
|---|---|---|
| `DEV_MODE` | off | Plain HTTP, no mTLS/tenancy — local only. |
| `-listen` | `:8090` | Listen address (DEV_MODE). |
| `CA_CERT_URL` / `CA_CERT_PATH` | — | CA used to validate client certs (prod). |
| `TLS_DOMAIN` | — | ACME/Let's Encrypt domain (prod). |
| `HTTP_LISTEN` / `HTTPS_LISTEN` | `:80` / `:443` | Prod listeners. |

## What needs the hosted control plane

Everything above runs without it. The hosted control plane
(`app.agentry.run`, closed source) is only required for:

- **`agentry init` / `agentry login`** — device enrollment + certificate
  issuance and the account/cluster model. The local engine doesn't use them.
- **The managed multi-tenant bridge** at `bridge.agentry.run` and the web
  dashboard.
- **Deployments through the hosted ingress** (`*.agentry.live` URLs, custom
  domains). The build path runs locally; the managed public URL does not.

To get the hosted experience — remote access over a tunnel, public deploy
URLs — on your own hardware, see **[SELF-HOSTING.md](SELF-HOSTING.md)**,
which lists exactly which pieces to bring for each part.

## Troubleshooting

- **`runtime image not found`** — run `make runtime-image` first.
- **`pull access denied … using cached image`** in the provisioner log —
  harmless: it tried to refresh `agentry/runtime:latest` from a registry and
  fell back to your locally-built image. Expected when self-building.
- **Docker permission errors** — make sure your user can run `docker ps`.
