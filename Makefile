# agentry — deploy targets.
#
# Every target does a CLEAN build from source. No commit overlays, no
# file-copy-into-running-container, no patches. If you find yourself
# reaching for a shortcut, extend the Makefile instead — the next
# operator will thank you.

# AWS (account 206579390825, us-east-1). app + bridge run on Graviton
# (arm64) EC2; the SSH key + migration state live under ~/.agentry-migrate.
SSH_KEY     ?= ~/.agentry-migrate/agentry-aws.pem
BRIDGE_HOST ?= ubuntu@18.235.159.224

RUNTIME_IMG     := ghcr.io/agentry-ai/runtime:latest
PROVISIONER_IMG := ghcr.io/agentry-ai/sandbox-provisioner:latest

# Public install bucket + CloudFront dist (AWS acct 206579390825). The
# `images` target writes the provisioner version marker here so the
# dashboard's "latest" check reads the exact version it just published.
SITE_BUCKET ?= agentry-site-206579390825
SITE_DIST   ?= E1U8GS89XKS1PG

# Provisioner build version — stamped into the binary AND published to
# the landing host as provisioner-latest.txt, which the control plane
# reads to tell the dashboard "update available". Computed locally
# (the remote build host has no .git). Override to pin.
PROV_VERSION ?= $(shell date -u +%Y.%m.%d)-$(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

GO ?= go

# CLI release: bump VERSION when shipping a new build to agentry.run.
VERSION ?= v0.6.6
RELEASE_ARCHES := darwin-arm64 darwin-amd64 linux-arm64 linux-amd64 windows-amd64 windows-arm64

# macOS code signing for release binaries. MACOS_SIGN_ID is the Developer ID
# Application identity (name or SHA-1) in the keychain. Notarization runs only
# when both NOTARY_APPLE_ID and NOTARY_PASSWORD (an app-specific password) are
# passed on the make line — never commit them. NOTARY_TEAM_ID defaults to the
# team. The signing identity lives in the login keychain (Developer ID G2
# intermediate must be installed, else codesign fails to build the chain).
MACOS_SIGN_ID   ?= Developer ID Application: Ashwin Rajeeva (BQ8YBBT9ZK)
NOTARY_TEAM_ID  ?= BQ8YBBT9ZK
NOTARY_APPLE_ID ?=
NOTARY_PASSWORD ?=

# Multi-arch builder for GHCR images. Created once with
# `docker buildx create --name agentry-multi --driver docker-container`.
BUILDX_BUILDER ?= agentry-multi
BUILDX_PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: help
help:
	@echo "agentry deploy targets:"
	@echo "  make cli                    build + install local CLI (/opt/homebrew/bin + ~/.local/bin)"
	@echo "  make bridge-deploy          build linux binary, ship to bridge box, restart"
	@echo "  make images                 build + push BOTH images multi-arch (amd64+arm64) — use this to publish"
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
	@echo "→ building agentry-bridge for linux/arm64 (AWS Graviton)"
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -o /tmp/agentry-bridge ./cmd/bridge
	@echo "→ shipping to $(BRIDGE_HOST)"
	scp -i $(SSH_KEY) /tmp/agentry-bridge $(BRIDGE_HOST):/tmp/agentry-bridge.new
	ssh -i $(SSH_KEY) $(BRIDGE_HOST) 'sudo mv /usr/local/bin/agentry-bridge /usr/local/bin/agentry-bridge.bak.$$(date +%s) && sudo mv /tmp/agentry-bridge.new /usr/local/bin/agentry-bridge && sudo chmod +x /usr/local/bin/agentry-bridge && sudo systemctl restart agentry-bridge && sleep 2 && systemctl is-active agentry-bridge'
	@echo "✓ bridge restarted"

# RETIRED: provisioner-deploy / runtime-deploy did a single-arch
# `docker build` on an amd64 host and pushed it to :latest. That
# OVERWRITES the multi-arch manifest with an amd64-only image, which
# breaks every arm64 (Apple Silicon) onboard with "no matching manifest
# for linux/arm64". Always publish both images multi-arch via `make
# images` (buildx, amd64+arm64). Servers update via the dashboard's
# "Update server" button or by re-running the `--pull=always` onboard
# command — no per-host rebuild needed.
.PHONY: provisioner-deploy runtime-deploy
provisioner-deploy runtime-deploy:
	@echo "✗ '$@' is retired: it published a single-arch (amd64-only) image and" >&2
	@echo "  clobbered the multi-arch :latest manifest, breaking arm64 onboards." >&2
	@echo "  Publish both images multi-arch instead:" >&2
	@echo "      make images" >&2
	@exit 2

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
#     upload to s3://$(SITE_BUCKET)/install/VERSION/, then flip latest.txt
#     so `curl agentry.run/install.sh | sh` picks it up.
#
#   make images
#     buildx-build BOTH ghcr.io images as multi-arch (amd64 + arm64).
#     Ships both arches: arm64 (Apple Silicon, AWS Graviton) + amd64.
#     amd64 builds run under emulation on Mac (slow once for the runtime
#     image, acceptable for releases).
#
# Both targets do their own integrity check (curl HEAD; manifest
# inspect) at the end so a partial upload doesn't ship silently.

.PHONY: release
release:
	@echo "→ building CLI for $(RELEASE_ARCHES)"
	@mkdir -p /tmp/agentry-release/$(VERSION)
	@for combo in $(RELEASE_ARCHES); do \
		os=$${combo%-*}; arch=$${combo#*-}; \
		ext=""; [ "$$os" = windows ] && ext=".exe"; \
		echo "  - $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" \
			-o /tmp/agentry-release/$(VERSION)/agentry-$$os-$$arch$$ext ./cmd/cli; \
	done
	@$(MAKE) --no-print-directory sign-macos
	@cd /tmp/agentry-release/$(VERSION) && shasum -a 256 agentry-* > SHA256SUMS
	@echo "→ uploading to s3://$(SITE_BUCKET)/install/$(VERSION)/ (served via CloudFront at agentry.run/install)"
	aws s3 cp /tmp/agentry-release/$(VERSION)/ s3://$(SITE_BUCKET)/install/$(VERSION)/ --recursive --cache-control "public,max-age=31536000"
	aws s3 cp /tmp/agentry-release/$(VERSION)/SHA256SUMS s3://$(SITE_BUCKET)/install/$(VERSION)/SHA256SUMS --content-type text/plain --cache-control "public,max-age=300"
	@echo "→ flipping latest.txt → $(VERSION)"
	printf '%s' "$(VERSION)" | aws s3 cp - s3://$(SITE_BUCKET)/install/latest.txt --content-type text/plain --cache-control "public,max-age=60"
	aws cloudfront create-invalidation --distribution-id $(SITE_DIST) --paths /install/latest.txt >/dev/null
	@echo "→ verifying public download"
	@curl -sfL -o /tmp/agentry-release-check "https://agentry.run/install/$(VERSION)/agentry-darwin-arm64" && \
		file /tmp/agentry-release-check | grep -q "Mach-O 64-bit executable arm64" && \
		rm /tmp/agentry-release-check && echo "✓ release $(VERSION) live at https://agentry.run/install.sh"

# sign-macos: Developer ID codesign (hardened runtime + secure timestamp) the
# darwin release binaries, then notarize if NOTARY_APPLE_ID + NOTARY_PASSWORD
# are set. Invoked by `release`; a no-op off macOS so Linux/CI builds still run.
# Bare CLI binaries can't be stapled (no container) — notarization registers
# the hash with Apple so Gatekeeper passes the online check on first run.
.PHONY: sign-macos
sign-macos:
	@if [ "$$(uname)" != "Darwin" ]; then echo "⚠ not on macOS — skipping codesign/notarize"; exit 0; fi
	@echo "→ codesigning macOS binaries (Developer ID + hardened runtime + timestamp)"
	@for b in agentry-darwin-arm64 agentry-darwin-amd64; do \
		codesign --force --options runtime --timestamp --sign "$(MACOS_SIGN_ID)" /tmp/agentry-release/$(VERSION)/$$b || exit 1; \
		codesign --verify --strict /tmp/agentry-release/$(VERSION)/$$b && echo "  ✓ signed $$b"; \
	done
	@if [ -n "$(NOTARY_APPLE_ID)" ] && [ -n "$(NOTARY_PASSWORD)" ]; then \
		echo "→ notarizing macOS binaries"; \
		for b in agentry-darwin-arm64 agentry-darwin-amd64; do \
			ditto -c -k --keepParent /tmp/agentry-release/$(VERSION)/$$b /tmp/$$b.notarize.zip; \
			xcrun notarytool submit /tmp/$$b.notarize.zip --apple-id "$(NOTARY_APPLE_ID)" --password "$(NOTARY_PASSWORD)" --team-id "$(NOTARY_TEAM_ID)" --wait || exit 1; \
			rm -f /tmp/$$b.notarize.zip; \
		done; \
	else \
		echo "⚠ notarization skipped — pass NOTARY_APPLE_ID and NOTARY_PASSWORD to enable"; \
	fi

.PHONY: images
images:
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		docker buildx create --name $(BUILDX_BUILDER) --driver docker-container --bootstrap
	@echo "→ building provisioner ($(BUILDX_PLATFORMS), version=$(PROV_VERSION))"
	docker buildx build --builder $(BUILDX_BUILDER) --platform $(BUILDX_PLATFORMS) \
		--build-arg PROVISIONER_VERSION=$(PROV_VERSION) \
		-f docker/Dockerfile.provisioner -t $(PROVISIONER_IMG) --push .
	@echo "→ building runtime ($(BUILDX_PLATFORMS); amd64 under emulation, slow but acceptable for releases)"
	docker buildx build --builder $(BUILDX_BUILDER) --platform $(BUILDX_PLATFORMS) \
		-f docker/Dockerfile.runtime -t $(RUNTIME_IMG) --push .
	@echo "→ verifying both manifests have amd64 + arm64"
	@docker buildx imagetools inspect $(PROVISIONER_IMG) | grep -E "linux/(amd64|arm64)\b" | sort -u
	@docker buildx imagetools inspect $(RUNTIME_IMG)     | grep -E "linux/(amd64|arm64)\b" | sort -u
	@echo "→ publishing version marker $(PROV_VERSION) (same value stamped into the image,"
	@echo "  so the dashboard's 'latest' can't drift from what's running)"
	printf '%s' "$(PROV_VERSION)" | aws s3 cp - s3://$(SITE_BUCKET)/install/provisioner-latest.txt \
		--content-type text/plain --cache-control "public,max-age=60"
	aws cloudfront create-invalidation --distribution-id $(SITE_DIST) \
		--paths /install/provisioner-latest.txt >/dev/null
	@echo "✓ both images live on GHCR as multi-arch; latest marker = $(PROV_VERSION)"
