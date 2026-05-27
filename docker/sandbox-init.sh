#!/bin/bash
# sandbox-init — runs at container start, before sandbox-runtime.
#
# Registers qemu binfmt_misc interpreters so cross-architecture buildah
# builds (e.g. `build-image --platform linux/amd64` on an arm64 host)
# can exec foreign-arch ELFs. Without this, a Dockerfile `RUN` step
# inside a cross-platform build fails with `exec format error`.
#
# Safe to no-op when:
#   - binfmt_misc isn't mounted (older kernels / restricted hosts)
#   - the interpreter is already registered (rebuild / restart)
#   - we lack permission (running unprivileged for some reason)
#
# Then exec the sandbox runtime.

set -e

BINFMT=/proc/sys/fs/binfmt_misc

# Docker doesn't mount binfmt_misc inside containers by default. Mount
# it if it isn't already. Needs CAP_SYS_ADMIN which the sandbox has.
if [ ! -e "$BINFMT/register" ]; then
    mount -t binfmt_misc binfmt_misc "$BINFMT" 2>/dev/null || true
fi

# Register qemu interpreters via update-binfmts (idempotent — re-running
# after a restart silently skips already-registered entries). Failures
# are non-fatal so the sandbox boots even on hosts where binfmt is
# locked down.
if [ -w "$BINFMT/register" ]; then
    if command -v update-binfmts >/dev/null 2>&1; then
        update-binfmts --enable 2>/dev/null || true
    fi
fi

exec /usr/local/bin/sandbox-runtime "$@"
