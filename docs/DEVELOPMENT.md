# Development

How to build agentry from source and work on it.

## Prerequisites

- **Go 1.26+** (see `go.mod`)
- **Docker** with buildx — to build the sandbox runtime/provisioner images
- Standard Unix tooling (`make`, `bash`, `curl`)

## Make targets

```sh
make build           # build all binaries into ./bin
make cli             # just the agentry CLI → ./bin/agentry
make runtime-image   # build the sandbox runtime image (agentry/runtime:latest)
make dev             # run the provisioner locally (Docker backend)
make smoke           # end-to-end local check (create sandbox, exec, delete)
make test            # go test ./...
make vet             # go vet ./...
make fmt             # gofmt tracked .go files
make help            # list targets
```

`make build` produces `agentry-cli`, `agentry-bridge`, `agentry-provisioner`,
`agentry-runtime`, `agentry-authproxy` (and a copy of the CLI as
`./bin/agentry`).

> **Maintainer publish/deploy** targets (signed CLI releases, multi-arch
> image push, host deploys) are not in this repo's `Makefile` — they live in
> an optional, gitignored `release.mk` that references private
> infrastructure. See `release.mk.example` for its shape.

## Repo layout

```
cmd/
  cli/          # the `agentry` CLI (MCP over stdio + sh/logs/vsc)
  runtime/      # in-sandbox daemon (HTTP API)
  provisioner/  # sandbox lifecycle on your machine (Docker/K8s)
  bridge/       # stateless mTLS routing pivot
  authproxy/    # optional auth sidecar for deployed apps
pkg/            # shared libraries (tunnel, mcp, broker, telemetry, …)
docker/         # Dockerfile.runtime, Dockerfile.provisioner + baked docs/skills
docs/           # architecture + local-dev + contributor docs
hack/           # smoke scripts
scripts/        # install.sh / update.sh
services/       # service binding templates
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for what each component does.

## The runtime image

`docker/Dockerfile.runtime` builds the sandbox userland. It's a `python`
base plus a baked toolchain: Node.js, a build toolchain (buildah/crun/
fuse-overlayfs, qemu for cross-arch), AWS CLI, GitHub CLI, code-server (the
in-browser editor), and Railpack (used by the deploy path). It also bakes
the operator docs/skills under `/etc/sandbox` and the `@agentry/automation`
template.

Build it locally with `make runtime-image`. The provisioner runs each
sandbox from this image (`SANDBOX_IMAGE`, default `agentry/runtime:latest`).

## Running a single component

```sh
# provisioner (Docker backend, loopback API) — see docs/RUNNING-LOCALLY.md
BACKEND=docker go run ./cmd/provisioner

# bridge in dev mode (plain HTTP, no mTLS)
DEV_MODE=1 go run ./cmd/bridge -listen :8090

# runtime (normally runs inside a sandbox container, not on the host)
go run ./cmd/runtime -addr :8080
```

## Tests

```sh
make test            # whole tree
go test ./pkg/...    # a subtree
go test -race ./...  # with the race detector
```

`make smoke` is the end-to-end check; it requires Docker and the runtime
image (`make runtime-image`).

## Conventions

- Standard Go style; run `make fmt` and `make vet` before sending a PR.
- Keep changes focused; match the surrounding code's comment density and
  idiom.
- New handlers/endpoints get table-driven tests covering the happy path and
  the failure modes (bad input, not found, upstream error).
