# Contributing to agentry

Thanks for your interest in agentry. This repo is the open-source engine —
CLI, runtime, provisioner, bridge, authproxy. (The hosted control plane is a
separate, closed-source service; see the README.)

## Getting set up

1. Install **Go 1.26+** and **Docker**.
2. Build and smoke-test locally:
   ```sh
   make runtime-image
   make smoke
   ```
   See [docs/RUNNING-LOCALLY.md](docs/RUNNING-LOCALLY.md) and
   [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).

## Making a change

1. Fork and branch from `main`.
2. Keep the change focused; match the surrounding code's style and comment
   density.
3. Add tests for new behavior — table-driven, covering the happy path and
   the failure modes.
4. Before opening a PR:
   ```sh
   make fmt
   make vet
   make test
   make smoke      # if you touched the sandbox/runtime/provisioner path
   ```
5. Open a PR against `main` with a clear description of *what* changed and
   *why*. Link any related issue.

## Reporting bugs / proposing features

Open a GitHub issue. For bugs, include: what you ran, what you expected,
what happened, and your OS + Docker version. For the local engine, the
provisioner log (`make dev` output) is usually the most useful artifact.

## Security

Please do **not** file security issues as public GitHub issues — see
[SECURITY.md](SECURITY.md) for private disclosure.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), the same license as the project.
