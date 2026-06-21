# Changelog

All notable changes to this project are documented here. The format is based
on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Open-source documentation set: architecture, running-locally, development,
  self-hosting, contributing, and security guides.
- `make` developer targets (`build`, `cli`, `runtime-image`, `dev`, `smoke`,
  `test`, `vet`, `fmt`) and a zero-dependency local smoke test.
- CI (build, vet, race tests, golangci-lint), issue/PR templates,
  `CODE_OF_CONDUCT`, `CODEOWNERS`, Dependabot.

### Changed
- Provisioner defaults to the Docker backend. The Kubernetes backend (and the
  Kata/gVisor isolation runtimes that ride on it) are stubbed as "coming
  soon"; the work-in-progress is reachable via `BACKEND=k8s-experimental`.

### Fixed
- IPv6-safe address formatting in the runtime CONNECT proxy.
