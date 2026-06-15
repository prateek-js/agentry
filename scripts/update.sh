#!/bin/sh
# agentry server updater.
#
#   curl -fsSL https://agentry.run/update.sh | sh
#
# Pulls the latest agentry runtime + provisioner images and recreates the
# provisioner container on the new image, PRESERVING its environment (the
# enrollment token) and network mode. Your sandboxes and deployed apps
# keep running; the server reconnects to the dashboard within seconds.
#
# Idempotent: safe to re-run. This is the bootstrap path for servers on a
# build from before one-click updates existed — afterwards you can update
# from the dashboard's "Server software" panel.
set -eu

NAME="${AGENTRY_PROVISIONER_CONTAINER:-agentry-provisioner}"
PROV_IMG="ghcr.io/agentry-ai/sandbox-provisioner:latest"
RT_IMG="ghcr.io/agentry-ai/runtime:latest"

if ! command -v docker >/dev/null 2>&1; then
  echo "✗ docker not found on this host." >&2
  exit 1
fi

if ! docker inspect "$NAME" >/dev/null 2>&1; then
  echo "✗ no container named '$NAME' on this host." >&2
  echo "  update.sh upgrades an EXISTING agentry server. To add a new one," >&2
  echo "  use the 'Add this machine' command from the dashboard." >&2
  echo "  (If your provisioner has a different name, set AGENTRY_PROVISIONER_CONTAINER.)" >&2
  exit 1
fi

echo "→ pulling latest images"
docker pull "$PROV_IMG"
docker pull "$RT_IMG" || echo "  (runtime image pull failed — non-fatal; it's only for new sandboxes)"

echo "→ recreating '$NAME' on the new image (preserving env + network)"
# Capture the live container's env + network BEFORE teardown — this is
# what keeps the enrollment token intact. docker restart would silently
# keep the OLD image, so we must stop → rm → run.
ENV_ARGS="$(docker inspect "$NAME" --format '{{range .Config.Env}}-e {{.}} {{end}}')"
NET_MODE="$(docker inspect "$NAME" --format '{{.HostConfig.NetworkMode}}')"
[ -n "$NET_MODE" ] || NET_MODE=host

docker stop "$NAME" >/dev/null
docker rm "$NAME" >/dev/null

# shellcheck disable=SC2086
docker run -d --name "$NAME" --restart=unless-stopped \
  --network "$NET_MODE" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v agentry-data:/var/lib/agentry-provisioner \
  $ENV_ARGS \
  "$PROV_IMG" >/dev/null

sleep 2
echo "→ status:"
docker ps --filter "name=$NAME" --format '   {{.Status}}  {{.Image}}'
echo "✓ updated — the server will reconnect to the dashboard shortly."
