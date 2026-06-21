package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentry-ai/agentry/pkg/mcp"
)

// Cluster-default env vars (JIRA tokens, custom API keys, anything
// the user wants on every sandbox in the cluster). Lives on the
// laptop, one JSON file per env var under
// ~/.agentry/envs/<cluster>/<NAME>.json.
//
// Same trust model + persistence shape as binds.go's cluster-default
// service bindings. The PostCreate hook replays each stored env on
// every successful sandbox_create, so the user runs `agentry env set
// JIRA_TOKEN` once and every future sandbox in the cluster has it.
//
// Sandbox-scoped values (the old `agentry env set --sandbox <id>`
// shape) keep working untouched — those go straight to the
// provisioner over the tunnel and don't touch this directory.

// StoredEnv is the on-disk shape for one cluster-default env var.
type StoredEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// envsDir returns the cluster + profile-scoped env-var directory,
// honouring $AGENTRY_CONFIG. Sibling of bindsDir; both anchor under
// the agentry config root and both run the same legacy → default-
// profile migration on first call.
func envsDir(cluster, profile string) string {
	if cluster == "" {
		return ""
	}
	if profile == "" {
		profile = defaultProfile
	}
	migrateLegacyProfileLayout()
	base := filepath.Dir(ConfigPath())
	return filepath.Join(base, "envs", cluster, profile)
}

func envFilePath(cluster, profile, name string) string {
	return filepath.Join(envsDir(cluster, profile), name+".json")
}

// saveEnv writes one cluster-default env var to disk atomically with
// mode 0600. Creates the parent dir with 0700 so anyone else on the
// laptop can't even list which envs are staged. Name is validated
// against the conventional shell shape — empty / lowercase / dotted
// names get bounced so the resulting filename and the env-var name
// don't drift.
func saveEnv(cluster, profile string, e *StoredEnv) error {
	if cluster == "" {
		return fmt.Errorf("cluster is empty; run `agentry server use <name>` first")
	}
	if e == nil || e.Name == "" {
		return fmt.Errorf("env var name is required")
	}
	if !isValidEnvName(e.Name) {
		return fmt.Errorf("invalid env var name %q (must match [A-Z_][A-Z0-9_]*)", e.Name)
	}
	dir := envsDir(cluster, profile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	path := envFilePath(cluster, profile, e.Name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// loadEnv reads one stored env var. Returns (nil, nil) when the file
// doesn't exist — "not staged" is a valid state.
func loadEnv(cluster, profile, name string) (*StoredEnv, error) {
	raw, err := os.ReadFile(envFilePath(cluster, profile, name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e StoredEnv
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("parse %s: %w", envFilePath(cluster, profile, name), err)
	}
	return &e, nil
}

// deleteEnv removes the on-disk file. Missing file is not an error.
func deleteEnv(cluster, profile, name string) error {
	err := os.Remove(envFilePath(cluster, profile, name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// listEnvs returns every stored env for a (cluster, profile),
// sorted by name so `agentry env ls` is deterministic.
func listEnvs(cluster, profile string) ([]*StoredEnv, error) {
	dir := envsDir(cluster, profile)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*StoredEnv
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(ent.Name(), ".json")
		e, err := loadEnv(cluster, profile, name)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// applyClusterEnvDefaults returns the PostCreate hook that replays
// every stored env var onto a freshly-created sandbox. Same shape +
// semantics as applyClusterDefaults in binds.go — runs per
// sandbox_create, uses the live cluster (not the boot-time one), best
// effort with stderr logging, never blocks the create on a partial
// failure.
func applyClusterEnvDefaults(getCtx func() (cluster, profile string), hc *http.Client) func(context.Context, mcp.SandboxInfo) error {
	return func(ctx context.Context, info mcp.SandboxInfo) error {
		if getCtx == nil {
			return nil
		}
		cluster, profile := getCtx()
		if cluster == "" {
			return nil
		}
		envs, err := listEnvs(cluster, profile)
		if err != nil {
			return fmt.Errorf("list envs: %w", err)
		}
		// Stamp AGENTRY_PROFILE on every sandbox even when nothing
		// else is staged — app code branching on prod-only features
		// shouldn't depend on the operator having set other envs.
		envs = append(envs, &StoredEnv{Name: "AGENTRY_PROFILE", Value: profile})
		// When auth is enabled on this (cluster, profile), pull the
		// state and stamp the sidecar's contract: AGENTRY_AUTH_ENABLED
		// + the DB-URL pointer + the HMAC secret + each provider's
		// client_id / client_secret. The sidecar inside the sandbox
		// reads these at boot.
		if authState, _ := loadAuthState(cluster, profile); authState != nil && authState.Enabled {
			envs = append(envs,
				&StoredEnv{Name: "AGENTRY_AUTH_ENABLED", Value: "true"},
				&StoredEnv{Name: "AGENTRY_AUTH_DB", Value: authState.DBBinding},
				&StoredEnv{Name: "AGENTRY_AUTH_SECRET", Value: authState.Secret},
			)
			for name, val := range authStateProviderEnv(authState) {
				envs = append(envs, &StoredEnv{Name: name, Value: val})
			}
		}
		var firstErr error
		applied := 0
		for _, e := range envs {
			body := map[string]any{
				"name":   e.Name,
				"value":  e.Value,
				"source": "cli-cluster-default",
			}
			if err := postSecret(ctx, hc, info.SandboxID, body); err != nil {
				log.Printf("agentry: cluster-default env %q → sandbox %s failed: %v",
					e.Name, info.SandboxID, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			applied++
		}
		if applied > 0 {
			log.Printf("agentry: applied %d cluster-default env var(s) to sandbox %s",
				applied, info.SandboxID)
		}
		return firstErr
	}
}

// postSecret fires one POST /api/sandboxes/{id}/secrets to the
// provisioner over the existing tunneled HTTP client. Kept local
// (rather than reaching into pkg/mcp.Client) to avoid the same
// circular import binds.go's postBinding sidesteps.
func postSecret(ctx context.Context, hc *http.Client, sandboxID string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		"http://bridge.invalid/api/sandboxes/"+sandboxID+"/secrets",
		bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, b)
	}
	return nil
}

// chainHooks composes two PostCreate hooks into one. Both run; both
// errors get logged via the individual hooks' own log.Printf calls.
// The combined hook returns whichever error happens first so the
// caller (sandboxCreate in pkg/mcp/tools.go) still sees one error
// for its "post-create hook" log line.
func chainHooks(hooks ...func(context.Context, mcp.SandboxInfo) error) func(context.Context, mcp.SandboxInfo) error {
	return func(ctx context.Context, info mcp.SandboxInfo) error {
		var firstErr error
		for _, h := range hooks {
			if h == nil {
				continue
			}
			if err := h(ctx, info); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
}

// isValidEnvName mirrors the runtime's accepted shape: starts with
// A-Z or _, then any of A-Z, 0-9, _. Keeps the laptop and the
// runtime in agreement so a save here never produces a file the
// runtime would later reject at apply time.
func isValidEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
