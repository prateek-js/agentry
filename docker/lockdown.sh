#!/bin/bash
# Security lockdown for ad-sandbox containers.
#
# Two layers of defense:
# 1. Filesystem permissions — restrict access to sensitive paths
# 2. Restricted shell — block dangerous commands, restrict cd
#
# Run during container build: RUN bash /opt/sandbox/lockdown.sh

set -e

echo "[lockdown] Applying filesystem restrictions..."

# Create sandbox user if needed.
if ! id -u sandbox &>/dev/null; then
    useradd -m -s /bin/bash -d /workspace sandbox 2>/dev/null || true
fi

# Restrict sensitive paths.
chmod 700 /root 2>/dev/null || true
chmod 640 /etc/shadow 2>/dev/null || true

# Ensure workspace is writable.
chown -R sandbox:sandbox /workspace /outputs /tmp/sandbox 2>/dev/null || true
chmod 777 /workspace /outputs /tmp/sandbox 2>/dev/null || true

# ── Restricted shell ──────────────────────────────────────────────────────
echo "[lockdown] Installing restricted shell..."

cat > /usr/local/bin/sandbox-shell << 'SHELL_EOF'
#!/bin/bash
# Restricted shell for ad-sandbox.
# Blocks dangerous commands and restricts filesystem navigation.

ALLOWED_DIRS=("/workspace" "/tmp" "/outputs" "/usr" "/bin" "/opt")

# Block dangerous commands.
BLOCKED_CMDS="chroot nsenter unshare mount umount"

_is_allowed_dir() {
    local target="$1"
    # Resolve to absolute path.
    local resolved
    resolved=$(cd "$target" 2>/dev/null && pwd) || return 1
    for dir in "${ALLOWED_DIRS[@]}"; do
        if [[ "$resolved" == "$dir"* ]]; then
            return 0
        fi
    done
    return 1
}

# Override cd to restrict navigation.
cd() {
    if [ -z "$1" ] || [ "$1" = "~" ]; then
        builtin cd /workspace
        return
    fi
    if _is_allowed_dir "$1"; then
        builtin cd "$1"
    else
        echo "sandbox: cd: restricted: $1" >&2
        return 1
    fi
}

# Block dangerous commands by overriding them.
for cmd in $BLOCKED_CMDS; do
    eval "$cmd() { echo \"sandbox: $cmd: blocked for security\" >&2; return 1; }"
done

export HOME=/workspace
export PS1='sandbox:\w\$ '

# If invoked as login shell or with -c, execute directly.
if [ "$1" = "-c" ]; then
    shift
    eval "$@"
else
    exec /bin/bash --norc --noprofile "$@"
fi
SHELL_EOF

chmod +x /usr/local/bin/sandbox-shell

echo "[lockdown] Complete."
