# agentry — build & develop.
#
# This Makefile holds the open-source developer targets: build the
# binaries, build the sandbox runtime image, run the engine locally, test.
#
# Maintainer-only publish/deploy targets (release signing, multi-arch
# image push, host deploys to specific infrastructure) live in an
# OPTIONAL `release.mk` that is gitignored and NOT part of this repo —
# see release.mk.example for its shape. The line below includes it when
# present and is silently ignored otherwise.
-include release.mk

# Keep `make` (no args) on help even though release.mk is included first.
.DEFAULT_GOAL := help

GO  ?= go
BIN ?= bin

# The sandbox image the provisioner runs. Built + tagged locally by
# `make runtime-image`; in production the provisioner pulls it from a
# registry. Override with SANDBOX_IMAGE when running the provisioner.
RUNTIME_IMAGE     ?= agentry/runtime:latest
PROVISIONER_IMAGE ?= agentry/provisioner:latest

.PHONY: help
help:
	@echo "agentry — build & develop:"
	@echo "  make build           build all binaries into ./$(BIN)"
	@echo "  make cli             build just the agentry CLI into ./$(BIN)/agentry"
	@echo "  make runtime-image   build the sandbox runtime image locally ($(RUNTIME_IMAGE))"
	@echo "  make dev             run the sandbox engine locally (provisioner, Docker backend)"
	@echo "  make smoke           end-to-end local check: create a sandbox, run a command, tear down"
	@echo "  make test            go test ./..."
	@echo "  make vet             go vet ./..."
	@echo "  make fmt             gofmt -w (tracked .go files)"
	@echo ""
	@echo "First run: make runtime-image && make smoke"

# ── Build ──────────────────────────────────────────────────────────

.PHONY: build
build:
	@mkdir -p $(BIN)
	@for c in cli bridge provisioner runtime authproxy; do \
		echo "→ building $$c"; \
		$(GO) build -trimpath -o $(BIN)/agentry-$$c ./cmd/$$c || exit 1; \
	done
	@cp $(BIN)/agentry-cli $(BIN)/agentry
	@echo "✓ binaries in ./$(BIN) (CLI also as ./$(BIN)/agentry)"

.PHONY: cli
cli:
	@mkdir -p $(BIN)
	$(GO) build -trimpath -o $(BIN)/agentry ./cmd/cli
	@echo "✓ ./$(BIN)/agentry"

.PHONY: runtime-image
runtime-image:
	@echo "→ building sandbox runtime image $(RUNTIME_IMAGE) (loaded into local Docker)"
	docker build -f docker/Dockerfile.runtime -t $(RUNTIME_IMAGE) .
	@echo "✓ $(RUNTIME_IMAGE)"

.PHONY: provisioner-image
provisioner-image:
	@echo "→ building provisioner image $(PROVISIONER_IMAGE)"
	docker build -f docker/Dockerfile.provisioner -t $(PROVISIONER_IMAGE) .
	@echo "✓ $(PROVISIONER_IMAGE)"

# ── Run locally ────────────────────────────────────────────────────

# Run the sandbox engine on your machine with no control plane, no
# bridge, and no certificates: the provisioner with the Docker backend.
# It listens on 127.0.0.1:8002 and creates each sandbox as a local Docker
# container from $(RUNTIME_IMAGE). Build the image first: `make runtime-image`.
# See docs/RUNNING-LOCALLY.md for how to drive it.
.PHONY: dev
dev:
	@docker image inspect $(RUNTIME_IMAGE) >/dev/null 2>&1 || \
		{ echo "✗ $(RUNTIME_IMAGE) not found — run 'make runtime-image' first"; exit 1; }
	@echo "→ provisioner on http://127.0.0.1:8002 (Docker backend, local-dev posture; Ctrl-C to stop)"
	BACKEND=docker SANDBOX_IMAGE=$(RUNTIME_IMAGE) PROVISIONER_ADDR=127.0.0.1:8002 $(GO) run ./cmd/provisioner

.PHONY: smoke
smoke:
	bash hack/local_smoke.sh

# ── Quality ────────────────────────────────────────────────────────

.PHONY: test
test:
	$(GO) test ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: fmt
fmt:
	@gofmt -w $$(git ls-files '*.go')
	@echo "✓ formatted"
