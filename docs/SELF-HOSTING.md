# Self-hosting

The hosted service at `app.agentry.run` is **convenience** — it runs the
multi-tenant control plane (accounts, dashboard, device enrollment + CA,
the managed tunnel, deploy ingress) so you don't have to. Everything that
actually does the work — creating sandboxes, the runtime API, building and
running apps — is in this repo and runs without it.

This guide lists, for each thing the hosted service provides, **what you
bring to get the same result yourself.**

## What the hosted service does — and your DIY equivalent

| Hosted service provides | Self-host: bring this instead |
|---|---|
| Web dashboard + accounts | Nothing — you're the single operator. Drive everything from the CLI / HTTP API. |
| One-paste onboarding (`agentry init` / `login`) | Run the provisioner yourself with env vars (below). |
| Device enrollment + **CA / certificate issuance** | Your own CA + client certs (only needed for the remote-tunnel setup). |
| **Managed multi-tenant bridge** (`bridge.agentry.run`) | Run `cmd/bridge` yourself **only if** compute is on a different network. |
| **Deploy ingress** (`*.agentry.live`, custom domains) | Your own domain + TLS + reverse proxy (only if you want public deploy URLs). |

Pick the tier that matches your setup. Most people want Tier 1.

---

## Tier 1 — single machine or same network (recommended)

If the AI client and the compute are on the same machine or LAN, you need
**no control plane, no bridge, and no certificates.** Run the provisioner
with the Docker backend and talk to it directly.

```sh
make runtime-image
make dev            # provisioner on http://127.0.0.1:8002 (Docker backend)
```

Drive it over the runtime API (see
[RUNNING-LOCALLY.md](RUNNING-LOCALLY.md) for the full walkthrough and the
env-var reference):

```sh
curl -s localhost:8002/api/sandboxes -d '{"sandbox_id":"demo"}'
curl -s localhost:8002/api/sandboxes/demo/runtime/v1/shell/exec \
  -d '{"command":"node --version","timeout":30}'
```

This is a fully supported, complete setup. The hosted service adds nothing
to the sandbox capabilities here — only the dashboard and remote access.

---

## Tier 2 — remote compute over a tunnel (what the managed bridge does)

If the compute lives somewhere the client can't reach directly (a box behind
NAT, a server in a different network), you replicate the managed bridge by
running `cmd/bridge` yourself. The provisioner dials **out** to it and holds
the tunnel open; the client's requests route down it. To stand this up you
bring:

1. **A public host for the bridge** — any small VM with a stable address.
2. **A domain + TLS** for that host. The bridge does ACME/Let's Encrypt
   itself: set `TLS_DOMAIN=bridge.example.com` (it serves `:80`/`:443`).
3. **A CA** that signs the client certificates. The bridge validates every
   client cert against it:
   ```sh
   openssl genrsa -out ca.key 4096
   openssl req -x509 -new -nodes -key ca.key -days 3650 -out ca.crt -subj "/CN=agentry-self-host-ca"
   ```
   Point the bridge at it: `CA_CERT_PATH=/path/to/ca.crt`.
4. **Client certs** for the provisioner (and for the CLI, if you route the
   CLI through the bridge), issued from that CA. The provisioner loads its
   bundle from `AGENTRY_CERT_DIR` and dials the bridge via
   `AGENTRY_BRIDGE_URL` under the identity `AGENTRY_CLUSTER_NAME`.

```sh
# bridge (on the public host)
TLS_DOMAIN=bridge.example.com CA_CERT_PATH=/etc/agentry/ca.crt ./agentry-bridge

# provisioner (on the compute host)
AGENTRY_BRIDGE_URL=https://bridge.example.com \
AGENTRY_CLUSTER_NAME=my-box \
AGENTRY_CERT_DIR=/etc/agentry/certs \
BACKEND=docker ./agentry-provisioner
```

> **Gap to know about.** The hosted service issues these certs automatically
> via its enrollment endpoint + KMS-backed CA. There is **no turnkey
> self-host enrollment yet** — you generate the CA and issue client certs by
> hand (or with your own script). `agentry init` / `agentry login` are
> hosted-only; for a self-hosted tunnel you wire the provisioner's cert
> bundle and bridge URL directly via the env vars above. A small OSS
> enrollment/CA helper is a natural contribution — see
> [CONTRIBUTING.md](../CONTRIBUTING.md).

For a quick local test of the tunnel path without any of this, run the
bridge in `DEV_MODE=1` (plain HTTP, no mTLS) — see RUNNING-LOCALLY.md.

---

## Deployments (optional)

The build step — turn a sandbox project into an OCI image — runs entirely on
your own machine (Docker backend, no external services). What the hosted
service adds is the **public URL**: it routes `*.agentry.live` (and custom
domains, via Cloudflare for SaaS) to the running container through the
managed bridge.

To serve deployed apps publicly yourself, bring:

- a **wildcard domain** (e.g. `*.apps.example.com`) and TLS for it, and
- a **reverse proxy / ingress** in front of the deploy containers (or route
  through your self-hosted bridge with a deploy domain configured).

If you don't need public URLs, you can skip this entirely — the build path
and running the container locally work on their own.

---

## Summary

- **Just want sandboxes on your own box?** Tier 1. Nothing to bring but Go +
  Docker.
- **Need to reach compute across a network?** Tier 2 — bring a public host,
  a domain + TLS, a CA, and client certs.
- **Want public deploy URLs?** Add a wildcard domain + TLS + ingress.
- **Want accounts, a dashboard, and automatic enrollment?** That's the
  hosted service — it exists so you don't have to run any of the above.
