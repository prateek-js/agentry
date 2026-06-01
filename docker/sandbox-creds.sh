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
#     <service>.json          ← caller convention; apps read directly
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

# Service bindings + user-staged secrets are managed exclusively by
# the provisioner (via `agentry service bind` and `agentry env set`) and
# land under /var/run/agentry/. This directory is NEVER bind-mounted from
# the host — the provisioner writes here via the runtime file_write
# API, and only those values are sourced as env vars.
#
# Layout (one subdir per bound service plus a `secrets` subdir for
# user-staged values):
#
#   /var/run/agentry/
#     postgres/
#       DATABASE_URL       ← single-line file, value verbatim
#     redis/
#       REDIS_URL
#     secrets/
#       OPENAI_API_KEY     ← user-staged via `agentry env set`
#
# Conventional env var names per service are documented in the
# catalog (GET /api/catalog) so the LLM reads the canonical names.
_agentry_dir=/var/run/agentry
if [ -d "$_agentry_dir" ]; then
    for _agentry_sub in "$_agentry_dir"/*/; do
        [ -d "$_agentry_sub" ] || continue
        for _agentry_file in "$_agentry_sub"*; do
            [ -r "$_agentry_file" ] || continue
            _agentry_name=$(basename "$_agentry_file")
            _agentry_value=$(cat "$_agentry_file")
            export "$_agentry_name=$_agentry_value"
        done
    done
    unset _agentry_sub _agentry_file _agentry_name _agentry_value
fi
unset _agentry_dir
