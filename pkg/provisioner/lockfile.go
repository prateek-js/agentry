package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// LockfilePath is where the per-sandbox lockfile lives inside the
// sandbox. Build, deploy, and any "what's bound in this sandbox?"
// operation reads this; every catalog-touching op writes it.
//
// Lives under /workspace so users see it next to their code and so
// it survives sandbox-lifetime even if the provisioner restarts.
const LockfilePath = "/workspace/.sandbox-lock.json"

// Lockfile pins what the sandbox has been bound to. Shape is stable
// — schema version 1 today; bumps when we add fields with semantics
// the deploy side needs to understand.
type Lockfile struct {
	Version  int             `json:"version"`
	Cluster  string          `json:"cluster"`
	Bindings []LockedBinding `json:"bindings,omitempty"`
	Secrets  []string        `json:"secrets,omitempty"`
	Skills   []LockedSkill   `json:"skills,omitempty"`
}

// LockedBinding is one bound cluster service. Env values are NOT
// stored (those live in /var/run/agentry/<service>/<key> inside the
// sandbox); only env var names are recorded so the deploy side knows
// what contract to re-bind against.
type LockedBinding struct {
	Service    string   `json:"service"`
	Version    string   `json:"version"`
	EnvVars    []string `json:"env_vars,omitempty"`
	ResolvedAt string   `json:"resolved_at"`
}

// LockedSkill is a pulled skill version.
type LockedSkill struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	ResolvedAt string `json:"resolved_at"`
}

// runtimeFileRead reads a file from the sandbox's /workspace via the
// runtime's /v1/file/read endpoint. Used for the lockfile reads
// before each mutation. Returns nil, nil if the file doesn't exist.
func (p *Provisioner) runtimeFileRead(ctx context.Context, sandboxID, path string) ([]byte, error) {
	port, err := p.backend.GetNodePort(ctx, p.config.Namespace, "sandbox-"+sandboxID+"-svc")
	if err != nil || port == 0 {
		return nil, fmt.Errorf("sandbox %q not found", sandboxID)
	}
	base := fmt.Sprintf("http://%s:%d", p.config.NodeHost, port)
	body, _ := json.Marshal(map[string]any{"file": path})
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/file/read", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if p.config.RuntimeAPIKey != "" {
		req.Header.Set("X-Sandbox-API-Key", p.config.RuntimeAPIKey)
	}
	resp, err := sandboxHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read %s: status %d", path, resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	// The runtime wraps body content as {data:{content:"..."}}; lift it.
	var wrap struct {
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrap); err == nil && wrap.Data.Content != "" {
		return []byte(wrap.Data.Content), nil
	}
	// Some endpoints return content as a top-level field; tolerate either shape.
	var top struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &top); err == nil && top.Content != "" {
		return []byte(top.Content), nil
	}
	return raw, nil
}

// runtimeFileWrite is the inverse — same shape as writeBindingFiles
// uses, broken out for reuse from lockfile + other handlers.
func (p *Provisioner) runtimeFileWrite(ctx context.Context, sandboxID, path string, content []byte) error {
	port, err := p.backend.GetNodePort(ctx, p.config.Namespace, "sandbox-"+sandboxID+"-svc")
	if err != nil || port == 0 {
		return fmt.Errorf("sandbox %q not found", sandboxID)
	}
	base := fmt.Sprintf("http://%s:%d", p.config.NodeHost, port)
	body, _ := json.Marshal(map[string]any{"file": path, "content": string(content)})
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/file/write", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if p.config.RuntimeAPIKey != "" {
		req.Header.Set("X-Sandbox-API-Key", p.config.RuntimeAPIKey)
	}
	resp, err := sandboxHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("write %s: status %d", path, resp.StatusCode)
	}
	return nil
}

// updateLockfile is the central read-modify-write helper. mut is the
// caller's mutation closure that receives the current lockfile (or a
// fresh one if none exists) and edits in place. The result is written
// back atomically (single file_write).
func (p *Provisioner) updateLockfile(ctx context.Context, sandboxID string, mut func(*Lockfile)) error {
	lock, err := p.readLockfile(ctx, sandboxID)
	if err != nil {
		return err
	}
	if lock == nil {
		lock = &Lockfile{Version: 1, Cluster: p.config.ClusterID}
	}
	mut(lock)
	return p.writeLockfile(ctx, sandboxID, lock)
}

// readLockfile returns the current lockfile or nil if absent.
func (p *Provisioner) readLockfile(ctx context.Context, sandboxID string) (*Lockfile, error) {
	raw, err := p.runtimeFileRead(ctx, sandboxID, LockfilePath)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var lock Lockfile
	if err := json.Unmarshal(raw, &lock); err != nil {
		// Corrupted lockfile — treat as missing rather than failing
		// the operation. Build will fail later if it can't parse;
		// for everything else "no prior state" is the right fallback.
		return nil, nil
	}
	return &lock, nil
}

// writeLockfile marshals + writes. Pretty-printed so users + git
// diffs are readable.
func (p *Provisioner) writeLockfile(ctx context.Context, sandboxID string, lock *Lockfile) error {
	raw, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	return p.runtimeFileWrite(ctx, sandboxID, LockfilePath, append(raw, '\n'))
}

// recordBindingInLockfile is the bind-handler's caller-friendly
// wrapper around updateLockfile. Replaces any previous entry for the
// same service (re-bind = re-pin to new version + creds).
func (p *Provisioner) recordBindingInLockfile(ctx context.Context, sandboxID, service, version string, envVars []string) error {
	return p.updateLockfile(ctx, sandboxID, func(l *Lockfile) {
		filtered := make([]LockedBinding, 0, len(l.Bindings))
		for _, b := range l.Bindings {
			if b.Service != service {
				filtered = append(filtered, b)
			}
		}
		filtered = append(filtered, LockedBinding{
			Service:    service,
			Version:    version,
			EnvVars:    envVars,
			ResolvedAt: time.Now().UTC().Format(time.RFC3339),
		})
		l.Bindings = filtered
	})
}
