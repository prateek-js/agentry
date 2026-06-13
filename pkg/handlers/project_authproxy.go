package handlers

import (
	"os"
	"strings"
)

// project_authproxy.go — m2 authproxy wrap of the user's start_command.
//
// When the operator runs `agentry auth enable`, the CLI stamps
// AGENTRY_AUTH_ENABLED=true (plus AGENTRY_AUTH_DB / AGENTRY_AUTH_SECRET
// / provider env vars) into every sandbox env. project_start reads
// the resulting `env` slice and — if auth is enabled — wraps the
// start_command so the user's app sits behind the sidecar:
//
//   bridge --(:3000)--> authproxy --(127.0.0.1:3001)--> user app
//
// We DO NOT spawn authproxy alongside the user's command from this
// daemon. Instead, we set AGENTRY_AUTHPROXY_EXEC in env and swap the
// argv0 to `authproxy`. authproxy's exec mode (see
// cmd/authproxy/exec.go) takes care of starting the child and
// supervising it, so we keep a single PGID for the project and the
// existing port-discovery / log-capture / restart machinery keeps
// working unchanged.
//
// authproxy binary path: `/usr/local/bin/authproxy` — baked into the
// runtime image (docker/Dockerfile.runtime). Sandboxes that pre-date
// the bake will hit a "no such file" at project_start, which the
// existing error-surfacing already shows via the captured stderr.

const authproxyBinary = "/usr/local/bin/authproxy"

// authproxyDefaultPort + authproxyDefaultUpstreamPort mirror the
// constants in cmd/authproxy/exec.go. We hard-code them rather than
// reaching into cmd/authproxy because cmd/cli/... and cmd/authproxy
// are separate binaries; the wrap contract is "PORT=3000, upstream=
// PORT+1=3001". A future change that lets operators tune these via
// env would update both sides in lockstep.
const (
	authproxyDefaultPort         = 3000
	authproxyDefaultUpstreamPort = 3001
)

// maybeWrapAuthSidecar returns (cmd, args, env, internalPort) the
// ProjectManager should use. When auth is not enabled, it returns the
// original cmd/args + the same env + internalPort=0 (no port to
// hide). When auth IS enabled, it rewrites cmd to authproxy, stuffs
// the original command into AGENTRY_AUTHPROXY_EXEC, and returns the
// upstream port (3001) so the caller can elide it from
// ProjectStatus.Ports — the public port is 3000.
//
// Hiding the upstream port matters because anything that scans the
// project's ports (LLM picking a share target, the dashboard's port
// picker) would otherwise see TWO listeners — 3000 (authproxy) and
// 3001 (the user's app) — and could route to the wrong one. The
// public-facing port is always 3000 when the wrap is on.
//
// `startCommand` is the raw config.StartCommand; `env` is the full
// env slice the ProjectManager already built.
func maybeWrapAuthSidecar(startCommand []string, env []string) (string, []string, []string, int) {
	cmd := startCommand[0]
	args := startCommand[1:]

	if !authEnabledInEnv(env) {
		return cmd, args, env, 0
	}
	if _, err := os.Stat(authproxyBinary); err != nil {
		// Binary missing (older image; misconfigured CI). Fall back to
		// the un-wrapped command so the user's app still runs and the
		// failure is visible elsewhere (missing /auth/login, etc.)
		// rather than as a hard project-start error.
		return cmd, args, env, 0
	}
	joined := joinAuthproxyExec(startCommand)
	env = setEnvKey(env, "AGENTRY_AUTHPROXY_EXEC", joined)
	return authproxyBinary, nil, env, authproxyDefaultUpstreamPort
}

// authEnabledInEnv checks for AGENTRY_AUTH_ENABLED ∈ {true, 1, yes}.
// Same case-insensitive truthy set the sidecar's loadConfig accepts.
func authEnabledInEnv(env []string) bool {
	for _, kv := range env {
		if !strings.HasPrefix(kv, "AGENTRY_AUTH_ENABLED=") {
			continue
		}
		v := strings.ToLower(strings.TrimPrefix(kv, "AGENTRY_AUTH_ENABLED="))
		return v == "true" || v == "1" || v == "yes"
	}
	return false
}

// joinAuthproxyExec stringifies the argv so splitArgv on the
// authproxy side can reverse it. Args containing spaces are
// double-quoted; double quotes inside args are stripped (rare; we
// don't try to escape, by design — keeps the parser tiny).
func joinAuthproxyExec(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		a = strings.ReplaceAll(a, `"`, ``)
		if strings.Contains(a, " ") {
			a = `"` + a + `"`
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// filterInternalPort removes `internal` from a list of listening ports.
// Returns the original slice when internal is 0 (no wrap). Pre-m2 there
// was only ever one listener per project; with the wrap, authproxy + the
// user's app both bind. We hide the user app's port so any consumer
// scanning "the project's ports" (LLM share-target picker, dashboard
// port selector) only sees the public-facing one.
func filterInternalPort(ports []int, internal int) []int {
	if internal == 0 || len(ports) == 0 {
		return ports
	}
	out := ports[:0]
	for _, p := range ports {
		if p == internal {
			continue
		}
		out = append(out, p)
	}
	return out
}

// setEnvKey replaces an existing K=… entry or appends one. Modifies
// in place but returns the (possibly grown) slice — same shape as
// append.
func setEnvKey(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
