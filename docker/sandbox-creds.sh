# /etc/profile.d/sandbox-creds.sh — loaded for every login shell.
#
# Surfaces the credential mount at /etc/sandbox/creds/ as ambient
# environment, so apps don't need to know the path. If the directory
# is empty (no creds mounted), this is a silent no-op.
#
# Layout the host is expected to mount:
#
#   /etc/sandbox/creds/
#     env                     ← key=value pairs sourced as-is
#     aws/credentials         ← standard AWS creds file
#     aws/config              ← standard AWS config file
#     trino.json              ← caller convention; apps read directly
#     xdp.json                ← caller convention; apps read directly
#
# Apps that need creds read the JSON files explicitly — the only thing
# we auto-wire is the env file and the two AWS_*_FILE variables, both
# of which are standard cli/sdk conventions.

_creds_dir=/etc/sandbox/creds

if [ -r "$_creds_dir/env" ]; then
    set -a
    # shellcheck disable=SC1090
    . "$_creds_dir/env"
    set +a
fi

if [ -r "$_creds_dir/aws/credentials" ]; then
    export AWS_SHARED_CREDENTIALS_FILE="$_creds_dir/aws/credentials"
fi
if [ -r "$_creds_dir/aws/config" ]; then
    export AWS_CONFIG_FILE="$_creds_dir/aws/config"
fi

unset _creds_dir

# XDP service bindings + user-staged secrets are managed exclusively
# by the provisioner (via `xdp service bind` and `xdp env set`) and
# land under /var/run/xdp/. This directory is NEVER bind-mounted from
# the host — the provisioner writes here via the runtime file_write
# API, and only those values are sourced as env vars.
#
# The layout mirrors what XDP injects on a deployed pod, so dev and
# prod see identical paths and identical env var names:
#
#   /var/run/xdp/
#     trino/
#       TRINO_URL          ← single-line file, value verbatim
#       TRINO_USER
#       TRINO_PASSWORD
#     spark/
#       SPARK_MASTER
#     secrets/
#       OPENAI_API_KEY     ← user-staged via `xdp env set`
#
# Conventional env var names per service are documented in the
# catalog (GET /api/catalog) so the LLM reads the canonical names.
_xdp_dir=/var/run/xdp
if [ -d "$_xdp_dir" ]; then
    for _xdp_sub in "$_xdp_dir"/*/; do
        [ -d "$_xdp_sub" ] || continue
        for _xdp_file in "$_xdp_sub"*; do
            [ -r "$_xdp_file" ] || continue
            _xdp_name=$(basename "$_xdp_file")
            _xdp_value=$(cat "$_xdp_file")
            export "$_xdp_name=$_xdp_value"
        done
    done
    unset _xdp_sub _xdp_file _xdp_name _xdp_value
fi
unset _xdp_dir
