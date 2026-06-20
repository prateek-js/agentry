#!/usr/bin/env bash
# local_smoke.sh — prove the sandbox engine works end-to-end with NO
# control plane, NO bridge, and NO third-party services.
#
# Starts the provisioner (Docker backend), creates a sandbox, runs a
# command inside it, then tears everything down. This is the open-source
# "does it work on my machine" check.
#
# Prereqs: Go, Docker, and the runtime image built locally:
#     make runtime-image
set -euo pipefail

IMAGE="${SANDBOX_IMAGE:-agentry/runtime:latest}"
ADDR="${PROVISIONER_ADDR:-127.0.0.1:8002}"
SID="smoke-$$"

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "✗ runtime image '$IMAGE' not found — build it first: make runtime-image" >&2
  exit 1
fi

echo "→ starting provisioner (Docker backend) on http://$ADDR"
BACKEND=docker SANDBOX_IMAGE="$IMAGE" PROVISIONER_ADDR="$ADDR" \
  go run ./cmd/provisioner >/tmp/agentry-smoke-prov.log 2>&1 &
PROV_PID=$!
cleanup() {
  curl -fsS -X DELETE "http://$ADDR/api/sandboxes/$SID" >/dev/null 2>&1 || true
  kill "$PROV_PID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

for _ in $(seq 1 30); do
  curl -fsS "http://$ADDR/api/version" >/dev/null 2>&1 && break
  sleep 1
done

echo "→ creating sandbox '$SID'"
curl -fsS -X POST "http://$ADDR/api/sandboxes" \
  -H 'content-type: application/json' -d "{\"sandbox_id\":\"$SID\"}" >/dev/null
sleep 2

echo "→ running a command inside the sandbox"
OUT=$(curl -fsS -X POST "http://$ADDR/api/sandboxes/$SID/runtime/v1/shell/exec" \
  -H 'content-type: application/json' \
  -d '{"command":"echo OK_$(uname -m); node --version; python3 --version","timeout":30}')
echo "$OUT"

echo "$OUT" | grep -q '"exit_code":0' \
  && echo "✓ local smoke passed" \
  || { echo "✗ command did not exit 0"; exit 1; }
