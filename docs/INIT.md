# agentry — getting started

Three steps. Five minutes. Works on any Docker-capable machine — laptop,
VPS, bare metal, Mac or Linux, arm64 or amd64.

## 1. Install the CLI

```sh
curl -fsSL https://agentry.run/install.sh | sh
```

The installer auto-detects your OS and arch and drops the right binary
at `/usr/local/bin/agentry`. If `/usr/local/bin` isn't writable it
falls back to `sudo`; if you don't want sudo, pre-set the prefix:

```sh
AGENTRY_PREFIX=$HOME/.local/bin curl -fsSL https://agentry.run/install.sh | sh
```

Verify:

```sh
agentry status   # should print "config: ~/.agentry/agentry.json (not initialised)"
```

## 2. Sign in (`agentry login`)

```sh
agentry login
```

This opens your browser to <https://app.agentry.run/cli-login>. Sign in
with Clerk (Google / GitHub / email), click **Authorize**, and you're
done — the dashboard hands a personal access token back to the CLI
over loopback (`127.0.0.1`) and writes `~/.agentry/agentry.json`. The
token never travels over the network.

Tokens are listed under **Settings → CLI tokens** in the dashboard and
can be revoked with one click. `agentry logout` removes the local
token and revokes it server-side.

## 3. Add a server (the box that runs sandboxes)

A **server** is any machine with Docker that runs the agentry
provisioner. Sandboxes run there. Your laptop counts (Docker Desktop /
OrbStack / Colima are all fine), or use a VPS / bare metal.

1. In the dashboard, **Servers → + Add server**. Pick a name (e.g.
   "macbook" or "hetzner-prod").
2. The dashboard hands you a single `docker run …` line — paste it on
   the target machine.

The provisioner image is multi-arch (`linux/amd64` + `linux/arm64`),
so `docker pull` transparently grabs the right layers — Apple Silicon
included.

3. Within seconds the dashboard shows the server as **connected**.

## 4. Wire up your AI client

agentry exposes itself as an MCP server. Point your client at:

**Claude Desktop / Cursor / Roo Code** (`mcp.json` or equivalent):

```json
{
  "mcpServers": {
    "agentry": {
      "command": "agentry",
      "args": ["mcp"]
    }
  }
}
```

That's it. The next time the client starts, the LLM gets a `sandbox_*`
tool family. Tell it "build me a Next.js app that…" and it scaffolds,
binds services, runs `npm install`, and ships to a public URL — all on
the server you connected.

---

## Service bindings (optional, but you'll want this on day one)

```sh
agentry service ls                       # what the catalog supports
agentry service bind postgres            # interactive — paste your URL
agentry service bind --from-env stripe   # CI / scripted — reads STRIPE_*
```

Bindings are stored per-cluster on your laptop. Every sandbox the LLM
creates on that cluster gets the env vars stamped at boot. You can
override per-deploy from the dashboard's "New deployment" form.

## Switching between servers

```sh
agentry cluster              # interactive picker
agentry cluster use macbook  # explicit
agentry cluster current      # show what's active
```

Hot-reload is built in: the active `agentry mcp` process picks up
cluster switches within ~1 s, no restart needed. Roo / Cursor / Claude
Desktop don't have to be touched.

## Common operations

```sh
agentry sandbox ls             # what's running on the current cluster
agentry forward 3000           # tunnel localhost:3000 → sandbox's :3000
agentry forward <sid>:8000     # specific sandbox + port
agentry deploy                 # ship the current project (when on a sandbox host)
```

## Where things live

- `~/.agentry/agentry.json` — your config (control plane URL, PAT, current cluster)
- `~/.agentry/services/<cluster>/` — stored cluster-default bindings (one JSON per service)
- `https://agentry.run/install.sh` — installer for the CLI
- `ghcr.io/agentry-ai/sandbox-provisioner:latest` — multi-arch image for the provisioner
- `ghcr.io/agentry-ai/runtime:latest` — multi-arch image for sandbox containers
- `https://app.agentry.run` — dashboard
- `https://bridge.agentry.run` — broker / tunnel pivot (your laptop and your server both dial out to it; never the other way around)

## Troubleshooting

**"sandbox service seems to be having issues" in Roo / similar**
Usually the local `agentry mcp` child is stale (you upgraded the CLI
but the long-running child still runs the old binary). Kill it; the
client respawns it:

```sh
pkill -f "agentry mcp"
```

**Deploys fail mentioning the wrong project name**
Your provisioner image is older than the auto-resolve fix. Pull fresh:

```sh
docker pull ghcr.io/agentry-ai/sandbox-provisioner:latest
# then recreate the container — easiest via dashboard's "New token" button
```

**`agentry cluster` doesn't see a recently-created cluster**
Refresh:

```sh
agentry cluster ls --refresh   # pulls the latest from the control plane
```
