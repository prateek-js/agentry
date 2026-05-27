# agentry

Self-hosted compute for AI agents, with a managed control plane.

```
laptop (Claude / Roo / Cursor)
     │  MCP over stdio
     ▼
agentry CLI ───mTLS─── bridge.agentry.run ───mTLS─── provisioner
                       (routing pivot)               (your machine)
                                                            │
                                                            └── Docker / K8s / Kata
                                                                runs the sandbox
```

You bring the compute (laptop, VPS, K8s cluster). agentry runs the
control plane that wires it up. Bring-your-own-infra without the
ops tax.

## Repo layout

This is the open-source monorepo. The closed-source control plane
(Go API + React frontend backing **app.agentry.run**) lives in a
separate repo.

| Path | What |
|---|---|
| `cmd/bridge` | Stateless mTLS/yamux routing pivot. Deployed at **bridge.agentry.run**. |
| `cmd/provisioner` | Cluster-side daemon. One `docker run` to enroll and start. Runs on user's infra. |
| `cmd/runtime` | In-container daemon every sandbox runs. HTTP API for the provisioner. |
| `cmd/cli` | User-side CLI (formerly `xdp`). Talks MCP over stdio for AI clients. |
| `pkg/` | Shared internal libraries. |
| `docker/` | Dockerfiles for the runtime, provisioner, and bridge images. |
| `docs/` | Architecture + deployment docs. |

## Status

Pre-alpha. Architecture solidified, control plane v1 in active build.

## License

Apache License 2.0.
