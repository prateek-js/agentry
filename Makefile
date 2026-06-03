# agentry — deploy targets.
#
# Every target does a CLEAN build from source. No commit overlays, no
# file-copy-into-running-container, no patches. If you find yourself
# reaching for a shortcut, extend the Makefile instead — the next
# operator will thank you.

SSH_KEY     ?= ~/.ssh/acceldata_hz
BRIDGE_HOST ?= root@49.12.104.190
PROV_HOST   ?= root@46.224.166.75
LANDING_HOST ?= root@188.34.177.4

RUNTIME_IMG     := ghcr.io/agentry-ai/runtime:latest
PROVISIONER_IMG := ghcr.io/agentry-ai/sandbox-provisioner:latest

GO ?= go

# CLI release: bump VERSION when shipping a new build to agentry.run.
VERSION ?= v0.2.0
RELEASE_ARCHES := darwin-arm64 darwin-amd64 linux-arm64 linux-amd64

# Multi-arch builder for GHCR images. Created once with
# `docker buildx create --name agentry-multi --driver docker-container`.
BUILDX_BUILDER ?= agentry-multi
BUILDX_PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: help
help:
	@echo "agentry deploy targets:"
	@echo "  make cli                    build + install local CLI (/opt/homebrew/bin + ~/.local/bin)"
	@echo "  make bridge-deploy          build linux binary, ship to bridge box, restart"
	@echo "  make provisioner-deploy     rsync source, docker build provisioner, push, restart"
	@echo "  make runtime-deploy         rsync source, docker build runtime, verify entrypoint, push"
	@echo "  make smoke                  drive MCP smoke test (sandbox create→list→delete) end-to-end"
	@echo "  make smoke-login            drive agentry login + cluster-ls tenancy check end-to-end"
	@echo ""
	@echo "Each target verifies the result before claiming success — no shortcuts."

.PHONY: cli
cli:
	@echo "→ building agentry CLI"
	$(GO) build -trimpath -o /opt/homebrew/bin/agentry ./cmd/cli
	@cp /opt/homebrew/bin/agentry $$HOME/.local/bin/agentry
	@echo "✓ installed: /opt/homebrew/bin/agentry + ~/.local/bin/agentry"

.PHONY: bridge-deploy
bridge-deploy:
	@echo "→ building agentry-bridge for linux/amd64"
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o /tmp/agentry-bridge ./cmd/bridge
	@echo "→ shipping to $(BRIDGE_HOST)"
	scp -i $(SSH_KEY) /tmp/agentry-bridge $(BRIDGE_HOST):/tmp/agentry-bridge.new
	ssh -i $(SSH_KEY) $(BRIDGE_HOST) 'mv /usr/local/bin/agentry-bridge /usr/local/bin/agentry-bridge.bak.$$(date +%s) && mv /tmp/agentry-bridge.new /usr/local/bin/agentry-bridge && chmod +x /usr/local/bin/agentry-bridge && systemctl restart agentry-bridge && sleep 2 && systemctl is-active agentry-bridge'
	@echo "✓ bridge restarted"

.PHONY: sync-src
sync-src:
	@echo "→ rsync source to $(PROV_HOST):/root/agentry-src/"
	rsync -az --delete --exclude='.git' --exclude='node_modules' --exclude='bin/' --exclude='dist/' --exclude='*.bak.*' -e "ssh -i $(SSH_KEY)" ./ $(PROV_HOST):/root/agentry-src/

.PHONY: provisioner-deploy
provisioner-deploy: sync-src
	@echo "→ building provisioner image on $(PROV_HOST)"
	ssh -i $(SSH_KEY) $(PROV_HOST) 'cd /root/agentry-src && docker build -f docker/Dockerfile.provisioner -t $(PROVISIONER_IMG) .'
	ssh -i $(SSH_KEY) $(PROV_HOST) 'docker push $(PROVISIONER_IMG)'
	@# docker restart REUSES the existing container's image digest —
	@# so it'd silently keep running the OLD binary. We have to stop,
	@# remove, then `docker run` from the freshly-tagged image. Mounts
	@# (docker.sock + agentry-data volume) and env are preserved by
	@# reading them from the live container before teardown.
	@echo "→ recreating container with fresh image"
	@# Preserve the running container's env AND network mode. The
	@# default bridge network breaks the provisioner: when it dials
	@# the sandbox runtime at NodeHost:port (NodeHost=localhost,
	@# port=docker-published 3xxxx), it needs to be on the host
	@# network to reach those ports — sandboxes are siblings on the
	@# Docker daemon, not children. Without this the runtime proxy
	@# fails connection-refused on every request, the sandbox detail
	@# page goes dark, and the bindings list comes back empty.
	ssh -i $(SSH_KEY) $(PROV_HOST) 'set -e; \
		ENV_ARGS=$$(docker inspect agentry-provisioner --format "{{range .Config.Env}}-e {{.}} {{end}}"); \
		NET_MODE=$$(docker inspect agentry-provisioner --format "{{.HostConfig.NetworkMode}}"); \
		docker stop agentry-provisioner >/dev/null; \
		docker rm agentry-provisioner >/dev/null; \
		docker run -d --name agentry-provisioner --restart=unless-stopped \
			--network "$$NET_MODE" \
			-v /var/run/docker.sock:/var/run/docker.sock \
			-v agentry-data:/var/lib/agentry-provisioner \
			$$ENV_ARGS \
			$(PROVISIONER_IMG); \
		sleep 2; \
		docker ps --filter name=agentry-provisioner --format "{{.Status}} {{.Image}} network={{.Networks}}"'
	@echo "✓ provisioner image pushed + container recreated"

.PHONY: runtime-deploy
runtime-deploy: sync-src
	@echo "→ building runtime image on $(PROV_HOST)"
	ssh -i $(SSH_KEY) $(PROV_HOST) 'cd /root/agentry-src && docker build -f docker/Dockerfile.runtime -t $(RUNTIME_IMG) .'
	@echo "→ verifying entrypoint"
	ssh -i $(SSH_KEY) $(PROV_HOST) 'docker inspect $(RUNTIME_IMG) --format "Entrypoint: {{json .Config.Entrypoint}}  Cmd: {{json .Config.Cmd}}"'
	@echo "→ pushing to GHCR"
	ssh -i $(SSH_KEY) $(PROV_HOST) 'docker push $(RUNTIME_IMG)'
	@echo "✓ runtime image pushed — new sandboxes pick it up"

.PHONY: smoke
smoke:
	python3 hack/mcp_smoke.py

.PHONY: smoke-login
smoke-login:
	bash hack/login_smoke.sh

# ── Public-facing release artifacts ────────────────────────────────
#
#   make release VERSION=v0.x.y
#     Cross-compile the agentry CLI for the four supported platforms,
#     scp into /var/www/agentry/install/VERSION/, then flip latest.txt
#     so `curl agentry.run/install.sh | sh` picks it up.
#
#   make images
#     buildx-build BOTH ghcr.io images as multi-arch (amd64 + arm64).
#     Mac arm users pulling :latest now get arm64 layers transparently;
#     Hetzner amd64 still works. amd64 builds run under emulation on
#     Mac (slow once for the runtime image, acceptable for releases).
#
# Both targets do their own integrity check (curl HEAD; manifest
# inspect) at the end so a partial upload doesn't ship silently.

.PHONY: release
release:
	@echo "→ building CLI for $(RELEASE_ARCHES)"
	@mkdir -p /tmp/agentry-release/$(VERSION)
	@for combo in $(RELEASE_ARCHES); do \
		os=$${combo%-*}; arch=$${combo#*-}; \
		echo "  - $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" \
			-o /tmp/agentry-release/$(VERSION)/agentry-$$os-$$arch ./cmd/cli; \
	done
	@cd /tmp/agentry-release/$(VERSION) && shasum -a 256 agentry-* > SHA256SUMS
	@echo "→ uploading to $(LANDING_HOST):/var/www/agentry/install/$(VERSION)/"
	scp -i $(SSH_KEY) -q -r /tmp/agentry-release/$(VERSION) $(LANDING_HOST):/var/www/agentry/install/$(VERSION)
	@echo "→ flipping latest.txt"
	ssh -i $(SSH_KEY) $(LANDING_HOST) 'echo $(VERSION) > /var/www/agentry/install/latest.txt && chmod -R a+r /var/www/agentry/install/$(VERSION)'
	@echo "→ verifying public download"
	@curl -sfL -o /tmp/agentry-release-check "https://agentry.run/install/$(VERSION)/agentry-darwin-arm64" && \
		file /tmp/agentry-release-check | grep -q "Mach-O 64-bit executable arm64" && \
		rm /tmp/agentry-release-check && echo "✓ release $(VERSION) live at https://agentry.run/install.sh"

.PHONY: images
images:
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		docker buildx create --name $(BUILDX_BUILDER) --driver docker-container --bootstrap
	@echo "→ building provisioner ($(BUILDX_PLATFORMS))"
	docker buildx build --builder $(BUILDX_BUILDER) --platform $(BUILDX_PLATFORMS) \
		-f docker/Dockerfile.provisioner -t $(PROVISIONER_IMG) --push .
	@echo "→ building runtime ($(BUILDX_PLATFORMS); amd64 under emulation, slow but acceptable for releases)"
	docker buildx build --builder $(BUILDX_BUILDER) --platform $(BUILDX_PLATFORMS) \
		-f docker/Dockerfile.runtime -t $(RUNTIME_IMG) --push .
	@echo "→ verifying both manifests have amd64 + arm64"
	@docker buildx imagetools inspect $(PROVISIONER_IMG) | grep -E "linux/(amd64|arm64)\b" | sort -u
	@docker buildx imagetools inspect $(RUNTIME_IMG)     | grep -E "linux/(amd64|arm64)\b" | sort -u
	@echo "✓ both images live on GHCR as multi-arch"
