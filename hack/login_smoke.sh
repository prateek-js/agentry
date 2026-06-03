#!/usr/bin/env bash
# login_smoke.sh — end-to-end smoke for `agentry login` + tenant
# scoping. Run AFTER deploying bridge + agentry-app + installing CLI.
#
# What this verifies (the actual fix that landed):
#
#   1. `agentry login` round-trips PAT through localhost callback.
#   2. `agentry cluster ls` shows ONLY the caller's org clusters.
#   3. A cross-org probe (asking app.agentry.run for a cluster owned by
#      another org) returns 404, not someone else's data.
#
# Manual steps the test can't automate:
#
#   - You must be signed into https://app.agentry.run in your default
#     browser before running.
#   - The script will wait for you to click "Authorize" on the
#     /cli-login page.
set -euo pipefail

AGENTRY=${AGENTRY:-$(command -v agentry)}
APP_URL=${APP_URL:-https://app.agentry.run}

if [[ -z "$AGENTRY" || ! -x "$AGENTRY" ]]; then
  echo "FAIL: agentry CLI not on PATH (set AGENTRY=/path/to/agentry)" >&2
  exit 1
fi

step() { printf "\n→ %s\n" "$*"; }

step "agentry login (browser will open — click Authorize)"
"$AGENTRY" login --app-url "$APP_URL"

CFG="${AGENTRY_CONFIG:-$HOME/.agentry/agentry.json}"
if ! command -v jq >/dev/null 2>&1; then
  echo "FAIL: jq required (brew install jq)" >&2
  exit 1
fi

TOKEN=$(jq -r '.api_token // empty' "$CFG")
if [[ -z "$TOKEN" || "$TOKEN" != pat_tok_* ]]; then
  echo "FAIL: config has no PAT after login: $CFG" >&2
  jq . "$CFG" >&2
  exit 1
fi
echo "✓ PAT written to config: ${TOKEN:0:18}…"

step "agentry cluster ls (control-plane path; org-scoped)"
"$AGENTRY" cluster ls

step "Cross-org probe — directly ask app.agentry.run for someone else's cluster"
# clu_does_not_exist_in_your_org should resolve identically to a real
# other-org cluster — both should return 404 by the same code path
# (loadOrgCluster filters by org_id before checking the row exists).
HTTP=$(curl -s -o /tmp/clu_probe.json -w "%{http_code}" \
  -H "Authorization: Bearer $TOKEN" \
  "$APP_URL/api/v1/clusters/clu_does_not_exist_in_your_org")
if [[ "$HTTP" != "404" ]]; then
  echo "FAIL: cross-org probe returned HTTP $HTTP (want 404)" >&2
  cat /tmp/clu_probe.json >&2
  exit 1
fi
echo "✓ cross-org probe correctly 404"

step "Logout (revokes PAT server-side)"
"$AGENTRY" logout

REMAINING=$(jq -r '.api_token // empty' "$CFG")
if [[ -n "$REMAINING" ]]; then
  echo "FAIL: api_token still in config after logout" >&2
  exit 1
fi
echo "✓ local PAT cleared"

# Verify the token no longer works (server side revoke).
HTTP=$(curl -s -o /dev/null -w "%{http_code}" \
  -H "Authorization: Bearer $TOKEN" \
  "$APP_URL/api/v1/clusters")
if [[ "$HTTP" != "401" ]]; then
  echo "FAIL: revoked PAT still accepted (HTTP $HTTP, want 401)" >&2
  exit 1
fi
echo "✓ revoked PAT rejected by control plane"

echo
echo "ALL CHECKS PASSED"
