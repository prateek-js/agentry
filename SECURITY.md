# Security Policy

## Reporting a vulnerability

Please report security issues **privately** — do not open a public GitHub
issue.

Email **security@agentry.run** with:

- a description of the issue and its impact,
- steps to reproduce (a proof of concept if you have one),
- affected component(s) and version/commit.

We'll acknowledge your report, work with you on a fix, and credit you in the
release notes unless you prefer otherwise.

## Scope

This repo is the open-source engine (CLI, runtime, provisioner, bridge,
authproxy). The hosted control plane (`app.agentry.run`) is a separate,
closed-source service; issues there are also welcome at the same address.

## Security model (summary)

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#security-model) for detail.
Key properties:

- The provisioner's control API binds **loopback** by default.
- CLI ↔ bridge ↔ provisioner use **mTLS** with client-cert identity in
  hosted mode (`DEV_MODE` disables this for local single-user use only).
- The runtime is gated by a provisioner-managed **API key** so a co-located
  process can't drive a sandbox.
- Sandbox isolation comes from the backend (Docker / Kubernetes, optionally
  gVisor / Kata / Firecracker via `runtime_class`).

When running locally with `DEV_MODE` / no certificates, you are explicitly
turning these protections off — only do that on a trusted single-user
machine, never on a shared or internet-exposed host.
