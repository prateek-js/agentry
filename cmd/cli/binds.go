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

	"github.com/agentry/agentry/pkg/mcp"
)

// Cluster-default service bindings live on the laptop, one JSON file
// per service under ~/.ad-sandbox/services/<cluster>/<service>.json.
//
// When the LLM creates a new sandbox via `agentry stdio`, the post-create
// hook walks this directory for the active cluster and POSTs each
// stored binding to the sandbox. The result: every sandbox in a
// cluster gets the user's saved Trino / Spark / … env vars without
// per-sandbox prompts.
//
// Real credentials never leave the laptop; only the provisioner's
// /api/sandboxes/{id}/bindings endpoint sees them, over the tunneled
// HTTP path that already terminates at the provisioner. Same trust
// boundary as `agentry service bind --sandbox <id>`, just amortized.

// StoredBind is the on-disk shape for one cluster-default service.
// Version is the catalog version captured at save time so build
// manifests stay reproducible even if the catalog later bumps.
type StoredBind struct {
	Service string            `json:"service"`
	Version string            `json:"version,omitempty"`
	Env     map[string]string `json:"env"`
}

// bindsDir returns the cluster + profile-scoped service bind
// directory, honouring $AGENTRY_CONFIG. Does NOT create the
// directory — saveBind() does that lazily. Runs a one-time migration
// that moves legacy <cluster>/<service>.json files (pre-profile
// layout) into <cluster>/default/<service>.json. Idempotent.
func bindsDir(cluster, profile string) string {
	if cluster == "" {
		return ""
	}
	if profile == "" {
		profile = defaultProfile
	}
	migrateLegacyProfileLayout()
	base := filepath.Dir(ConfigPath())
	return filepath.Join(base, "services", cluster, profile)
}

func bindFilePath(cluster, profile, service string) string {
	return filepath.Join(bindsDir(cluster, profile), service+".json")
}

// loadBind reads one stored bind. Returns (nil, nil) when the file
// doesn't exist — "not staged" is a valid state, not an error.
func loadBind(cluster, profile, service string) (*StoredBind, error) {
	raw, err := os.ReadFile(bindFilePath(cluster, profile, service))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b StoredBind
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("parse %s: %w", bindFilePath(cluster, profile, service), err)
	}
	return &b, nil
}

// saveBind writes a bind atomically with mode 0600. Creates the
// parent dir with 0700.
func saveBind(cluster, profile string, b *StoredBind) error {
	if cluster == "" {
		return fmt.Errorf("no server selected; run `agentry server use <name>` first")
	}
	if b == nil || b.Service == "" {
		return fmt.Errorf("service is required")
	}
	dir := bindsDir(cluster, profile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	path := bindFilePath(cluster, profile, b.Service)
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

// deleteBind removes the on-disk file. Missing file is not an error
// — the user wanted it gone, it's gone.
func deleteBind(cluster, profile, service string) error {
	err := os.Remove(bindFilePath(cluster, profile, service))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// applyClusterDefaults returns the PostCreateHook used by `agentry stdio`.
// On every successful sandbox_create the hook reads stored binds for
// the active (cluster, profile) pair and POSTs each to
// /api/sandboxes/{id}/bindings through the same tunneled HTTP client.
//
// getCtx is called PER INVOCATION so the binds match whichever
// cluster + profile the request was actually routed to — important
// because the stdio process is long-running and `agentry server use`
// or `agentry profile use` can change the answer between
// sandbox_creates. A nil getter or empty cluster skips the hook
// entirely (degrades gracefully).
//
// The hook is best-effort: a missing config, an empty store, or one
// failing service all log + continue. The LLM still gets a working
// sandbox; the user just sees stderr noise about which bind didn't
// land. The strictness floor is: NEVER block sandbox creation on a
// laptop-side issue, because that's the worst UX of any failure
// mode here.
func applyClusterDefaults(getCtx func() (cluster, profile string), hc *http.Client) func(context.Context, mcp.SandboxInfo) error {
	return func(ctx context.Context, info mcp.SandboxInfo) error {
		if getCtx == nil {
			return nil
		}
		cluster, profile := getCtx()
		if cluster == "" {
			return nil
		}
		binds, err := listBinds(cluster, profile)
		if err != nil {
			return fmt.Errorf("list binds: %w", err)
		}
		if len(binds) == 0 {
			return nil
		}
		var firstErr error
		applied := 0
		for _, b := range binds {
			body := map[string]any{"service": b.Service, "env": b.Env}
			if b.Version != "" {
				body["version"] = b.Version
			}
			if err := postBinding(ctx, hc, info.SandboxID, body); err != nil {
				log.Printf("agentry: cluster-default bind %q → sandbox %s failed: %v",
					b.Service, info.SandboxID, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			applied++
		}
		if applied > 0 {
			log.Printf("agentry: applied %d cluster-default bind(s) to sandbox %s",
				applied, info.SandboxID)
		}
		return firstErr
	}
}

// postBinding fires one /api/sandboxes/{id}/bindings POST against
// the broker-invalid host (which the tunnel transport rewrites).
// Keeps the hook independent of pkg/mcp.Client to avoid a circular
// import: mcp.Client embeds the hook that would otherwise call back
// into mcp.Client.
func postBinding(ctx context.Context, hc *http.Client, sandboxID string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		"http://bridge.invalid/api/sandboxes/"+sandboxID+"/bindings",
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

// listBinds returns every stored bind for a (cluster, profile),
// sorted by service name so output is deterministic.
func listBinds(cluster, profile string) ([]*StoredBind, error) {
	dir := bindsDir(cluster, profile)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*StoredBind
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		service := strings.TrimSuffix(e.Name(), ".json")
		b, err := loadBind(cluster, profile, service)
		if err != nil {
			return nil, err
		}
		if b != nil {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}
