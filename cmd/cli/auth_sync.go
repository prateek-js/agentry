package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/agentry/agentry/pkg/tunnel"
)

// auth_sync.go — push current auth state to already-running sandboxes.
//
// Without this, auth env vars only land on a sandbox at create time
// (via applyClusterEnvDefaults). An operator who runs
// `agentry auth providers add google` after sandboxes exist would
// otherwise have to recreate every sandbox to surface the new
// provider — silently broken UX.
//
// We hook this onto every mutator: enable, disable, providers add,
// providers remove. It's also exposed as `agentry auth sync` for
// manual re-stamps after a partial failure or out-of-band edit.
//
// Best-effort: log per-sandbox failures, return the first error.
// Don't block the mutator if one sandbox is unreachable — the
// other ones still get the new state.

// runAuthSyncForActiveProfile is the "called after a mutator"
// entry point. Resolves cluster+profile from config, opens the
// tunnel, and dispatches. Returns silently with a log when there's
// nothing to sync (no cluster, no running sandboxes).
func runAuthSyncForActiveProfile(reason string) {
	cfg, _, err := LoadConfig()
	if err != nil {
		// Auth mutator already succeeded; don't drag down its exit
		// code over a config-load glitch on the sync path.
		fmt.Fprintf(os.Stderr, "agentry: auth-sync skipped (load config: %v)\n", err)
		return
	}
	if cfg.Cluster == "" {
		return
	}
	profile := resolveProfile(cfg, "")
	n, err := syncAuthToRunningSandboxes(cfg, profile)
	if n == 0 && err == nil {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentry: auth-sync (%s): %d sandbox(es) updated, %v\n", reason, n, err)
		return
	}
	fmt.Fprintf(os.Stderr, "agentry: auth-sync (%s): updated %d running sandbox(es)\n", reason, n)
}

// syncAuthToRunningSandboxes does the actual work. Returns the count
// of sandboxes successfully updated + the first per-sandbox error if
// any. The caller decides whether to surface the error.
//
// "Update" means: post the current auth-state's env vars (or clear
// them if disabled) to /api/sandboxes/<id>/secrets — same path
// applyClusterEnvDefaults uses for fresh sandboxes.
func syncAuthToRunningSandboxes(cfg *Config, profile string) (int, error) {
	list, err := fetchSandboxes(cfg)
	if err != nil {
		return 0, fmt.Errorf("list sandboxes: %w", err)
	}
	running := make([]sandboxInfo, 0, len(list))
	for _, s := range list {
		if sandboxIsLive(s) {
			running = append(running, s)
		}
	}
	if len(running) == 0 {
		return 0, nil
	}

	state, _ := loadAuthState(cfg.Cluster, profile)
	keys, values := authEnvForState(state)

	sess, err := openTunnel(cfg)
	if err != nil {
		return 0, fmt.Errorf("dial broker: %w", err)
	}
	defer sess.Close()
	rt := &clusterStampedRT{next: tunnel.NewRoundTripper(sess), cluster: cfg.Cluster}
	hc := &http.Client{Transport: rt, Timeout: 20 * time.Second}

	var firstErr error
	applied := 0
	for _, s := range running {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		// On enable / providers change, push every key+value. On
		// disable, push empty strings so the sandbox forgets the
		// state (we use empty rather than a delete endpoint because
		// the runtime stores the file → empty means "no env value"
		// which the sidecar treats as "not set").
		var perErr error
		for _, k := range keys {
			body := map[string]any{
				"name":   k,
				"value":  values[k],
				"source": "cli-auth-sync",
			}
			if e := postSecret(ctx, hc, s.SandboxID, body); e != nil {
				perErr = e
				fmt.Fprintf(os.Stderr, "agentry: auth-sync sandbox=%s key=%s failed: %v\n",
					s.SandboxID, k, e)
				if firstErr == nil {
					firstErr = e
				}
				break
			}
		}
		cancel()
		if perErr == nil {
			applied++
		}
	}
	return applied, firstErr
}

// authEnvForState returns the ordered key list + (key → value) map of
// auth env vars to stamp. When state is nil (auth disabled), values
// are empty strings — semantically "clear this var on the sandbox."
//
// Keys are sorted so the stamping order is deterministic across runs
// (useful for log scraping). The list is built from the union of:
//
//   - The three always-on auth vars (ENABLED, DB, SECRET)
//   - Per-provider _CLIENT_ID / _CLIENT_SECRET (+ _ISSUER / _SCOPES
//     for the providers that carry those)
func authEnvForState(state *AuthState) ([]string, map[string]string) {
	out := map[string]string{}

	if state != nil && state.Enabled {
		out["AGENTRY_AUTH_ENABLED"] = "true"
		out["AGENTRY_AUTH_DB"] = state.DBBinding
		out["AGENTRY_AUTH_SECRET"] = state.Secret
		for name, val := range authStateProviderEnv(state) {
			out[name] = val
		}
	} else {
		// Disabled or missing — push empties so the sandbox forgets.
		out["AGENTRY_AUTH_ENABLED"] = ""
		out["AGENTRY_AUTH_DB"] = ""
		out["AGENTRY_AUTH_SECRET"] = ""
		// Provider env vars are NOT cleared here — we don't know which
		// providers were ever set on each sandbox (the CLI doesn't
		// track per-sandbox state, only per-profile). Leaving stale
		// provider creds is safe: the sidecar in passthrough mode
		// ignores them. If the operator wants them gone, they can
		// recreate the sandbox or run `agentry env unset`.
	}

	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, out
}

// sandboxIsLive answers "should we bother stamping this sandbox?"
// Statuses come from the control plane proxy; we treat any non-empty
// "running"-shaped value as live and skip terminal ones.
func sandboxIsLive(s sandboxInfo) bool {
	switch s.Status {
	case "running", "starting", "ready", "":
		// Empty status is common when the bridge hasn't synced the
		// row yet; assume live and let the POST fail loudly if it
		// isn't.
		return true
	case "stopped", "stopping", "failed", "errored", "deleted":
		return false
	}
	// Anything novel — push and let the runtime decide.
	return true
}

// ── public sync command ────────────────────────────────────────────────

func authSync(args []string) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		fmt.Fprintln(os.Stdout, "Usage: agentry auth sync")
		fmt.Fprintln(os.Stdout, "  Re-stamp the active profile's auth env vars onto every")
		fmt.Fprintln(os.Stdout, "  running sandbox in the cluster. Run after fixing a partial")
		fmt.Fprintln(os.Stdout, "  failure or to confirm state is in sync.")
		return 0
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no server pinned — run `agentry server use NAME` first")
	}
	profile := resolveProfile(cfg, "")
	state, _ := loadAuthState(cfg.Cluster, profile)
	switch {
	case state == nil || !state.Enabled:
		fmt.Printf("auth disabled on cluster %q (profile %q) — clearing AGENTRY_AUTH_* on running sandboxes.\n",
			cfg.Cluster, profile)
	default:
		providers := sortedProviderKeys(state.Providers)
		if len(providers) == 0 {
			fmt.Printf("auth enabled on cluster %q (profile %q), no providers — stamping core vars.\n",
				cfg.Cluster, profile)
		} else {
			fmt.Printf("auth enabled on cluster %q (profile %q), providers: %v — stamping all.\n",
				cfg.Cluster, profile, providers)
		}
	}
	n, err := syncAuthToRunningSandboxes(cfg, profile)
	if err != nil {
		return die("partial sync (%d updated): %v", n, err)
	}
	fmt.Printf("updated %d running sandbox(es).\n", n)
	return 0
}
