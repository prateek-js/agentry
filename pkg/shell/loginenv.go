package shell

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
)

// LoginShellEnv returns the env you'd see inside a fresh login bash
// shell — i.e. with /etc/profile + /etc/profile.d/*.sh sourced. The
// returned slice is a drop-in for exec.Cmd.Env.
//
// Why this exists: things we spawn (jupyter kernels, project starts)
// inherit the runtime daemon's env. The runtime started before any
// bindings happened, so its env doesn't contain TRINO_URL etc. even
// when /var/run/xdp/trino/TRINO_URL is on disk. Running bash -lc env
// at spawn time re-reads the profile scripts so children see the
// freshly-bound vars.
//
// Falls back to os.Environ() on any error — better to launch with the
// runtime's env than to fail the spawn entirely.
func LoginShellEnv(ctx context.Context) []string {
	shellPath := "/bin/bash"
	if _, err := os.Stat(shellPath); err != nil {
		return os.Environ()
	}
	// env -0 prints NUL-separated KEY=VALUE so values containing
	// newlines round-trip cleanly.
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, shellPath, "-lc", "env -0")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return os.Environ()
	}
	raw := out.String()
	parts := strings.Split(raw, "\x00")
	env := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || !strings.Contains(p, "=") {
			continue
		}
		env = append(env, p)
	}
	if len(env) == 0 {
		return os.Environ()
	}
	return env
}
