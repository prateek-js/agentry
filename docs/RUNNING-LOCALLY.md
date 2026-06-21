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

The `agentry mcp` command is the MCP server an AI client (Claude Code,
Cursor, Roo, …) spawns over stdio. It does **not** talk to the provisioner
directly — it routes every tool call through the **bridge** tunnel. So a
local MCP setup is three processes: a dev bridge, the provisioner dialing
it, and `agentry mcp` pointed at the bridge. No control plane, no
certificates.

> The hosted onboarding (`agentry init` / `agentry login`) talks to
> `app.agentry.run` and writes this config for you. Locally you skip it and
> write a small config by hand, as below.

**1. Start a dev bridge** (plain HTTP, no mTLS — local single-user only):

```sh
DEV_MODE=1 go run ./cmd/bridge -listen :8090
```

**2. Start the provisioner, dialing the bridge** as a cluster named `local`:

```sh
BACKEND=docker SANDBOX_IMAGE=agentry/runtime:latest \
  AGENTRY_BRIDGE_URL=http://localhost:8090 AGENTRY_CLUSTER_NAME=local \
  go run ./cmd/provisioner
```

You should see `broker tunnel established (cluster=local …)` in the
provisioner log and `cluster local connected` in the bridge log.

**3. Write a local CLI config.** `agentry mcp` reads `~/.agentry/agentry.json`
(override the path with `AGENTRY_CONFIG`). Create one pointing at the dev
bridge — no certs, no token:

```json
{
  "broker_url": "http://localhost:8090",
  "device_id": "local-dev",
  "cluster": "local",
  "app_url": "http://localhost:9999"
}
```

(`app_url` is unused locally — any placeholder is fine.)

**4. Point your AI client at it.** In the client's MCP config:

```json
{
  "mcpServers": {
    "agentry": {
      "command": "agentry",
      "args": ["mcp"],
      "env": { "AGENTRY_CONFIG": "/absolute/path/to/agentry-local.json" }
    }
  }
}
```

Drop the `env` block if you wrote the config to the default
`~/.agentry/agentry.json`. Make sure `agentry` is on `PATH` (`make cli`
installs it, or use an absolute path to `./bin/agentry`).

Ask the client to "list the agentry tools" — you should get ~30
(`sandbox_create`, `command_run`, `file_write`, `code_exec`, …), and they
execute on the local provisioner over the dev bridge.

### Quick check without an AI client

You can drive the MCP server over stdio directly:

```sh
{ printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}'
  printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  sleep 1
  printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
  sleep 2
} | AGENTRY_CONFIG=/absolute/path/to/agentry-local.json agentry mcp
```

A JSON response listing the tools means the whole path
(client → `agentry mcp` → bridge → provisioner) is wired.

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
