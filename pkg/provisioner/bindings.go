package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/agentry/agentry/pkg/errcode"
)

// BindingRequest is the body for POST /api/sandboxes/{id}/bindings.
//
// If Env is non-empty, those values are used VERBATIM — caller (i.e.
// the user, via `agentry service bind`) supplied them. When empty, the
// provisioner falls back to the dev-mint stub (or, in production, a
// call to the XDP control plane to mint dev-tier credentials).
//
// Why expose Env: real dev work often needs real connection info
// (the user's own Trino, a personal Spark cluster, a staging DB).
// Stub-mint is only useful for the cluster-managed case.
type BindingRequest struct {
	Service string            `json:"service"`
	Version string            `json:"version,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// BindingResponse documents which env var names the binding wired up.
// The values themselves are NEVER in the response — they live as
// files inside the sandbox at /var/run/agentry/<service>/<key> and
// surface as env vars via the shell shim.
type BindingResponse struct {
	Service   string   `json:"service"`
	Version   string   `json:"version,omitempty"`
	EnvVars   []string `json:"env_vars"`
	ExpiresAt string   `json:"expires_at,omitempty"`
}

// BindingListResponse is the GET response. Lists what's bound on this
// sandbox WITHOUT exposing credential values. Used by the dashboard's
// "deploy from sandbox" form to show the user which env vars will be
// inherited (and which service supplied each one) before they click
// Deploy. The values themselves stay in /var/run/agentry/<svc>/<key>
// inside the sandbox until expandBindingEnv reads them at deploy time.
type BindingListResponse struct {
	Bindings []BindingInfo `json:"bindings"`
}

// BindingInfo is one bound service on a sandbox.
type BindingInfo struct {
	Service string   `json:"service"`
	Version string   `json:"version,omitempty"`
	EnvVars []string `json:"env_vars"`
}

// handleBindingList is GET /api/sandboxes/{id}/bindings. Returns the
// bindings the sandbox currently has — service → env-var-names. NO
// values cross the wire.
//
// Source of truth is the FILESYSTEM (/var/run/agentry/<svc>/<key>),
// not the lockfile. The lockfile is metadata written best-effort at
// bind time; if that write ever fails silently (it has — see
// handleBindingCreate's swallowed err), the lockfile and reality
// diverge. We start from the directory listing so the dashboard's
// "what's bound here" answer matches what the sandbox actually has.
// The lockfile contributes Version info for services it knows about.
func (p *Provisioner) handleBindingList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		errcode.WriteJSON(w, errcode.New(errcode.BindingInvalidRequest, "sandbox id missing in path"))
		return
	}

	// Filesystem-first scan. /var/run/agentry/<svc>/<key> = one cred
	// file per env var, grouped by service. ListDir on a missing dir
	// returns ([], nil), so a fresh sandbox just yields an empty list.
	fsBindings, err := p.scanBindingFiles(r.Context(), id)
	if err != nil {
		// Don't fail the whole call — fall through to lockfile-only.
		// Log so a degraded mode is visible.
		log.Printf("bindings: scan /var/run/agentry on sandbox=%s: %v", id, err)
	}

	// Layer in version metadata from the lockfile when we have it.
	versionFor := map[string]string{}
	if lock, _ := p.readLockfile(r.Context(), id); lock != nil {
		for _, b := range lock.Bindings {
			versionFor[b.Service] = b.Version
		}
	}

	out := BindingListResponse{Bindings: make([]BindingInfo, 0, len(fsBindings))}
	for _, b := range fsBindings {
		out.Bindings = append(out.Bindings, BindingInfo{
			Service: b.Service,
			Version: versionFor[b.Service],
			EnvVars: b.EnvVars,
		})
	}
	writeJSON(w, 200, out)
}

// scanBindingFiles enumerates /var/run/agentry/*/* inside the sandbox
// via the runtime's /v1/file/list. Each top-level directory is a
// service name; each file inside is one env var the bind wrote.
// Returns ([], nil) when /var/run/agentry doesn't exist (fresh sandbox).
func (p *Provisioner) scanBindingFiles(ctx context.Context, sandboxID string) ([]BindingInfo, error) {
	entries, err := p.runtimeListDir(ctx, sandboxID, "/var/run/agentry", 2)
	if err != nil {
		return nil, err
	}
	// Group: service dir → env var files. Entries from a depth-2 list
	// include both the service dirs themselves and the files under them.
	envBySvc := map[string][]string{}
	for _, e := range entries {
		if e.IsDir {
			if _, ok := envBySvc[e.Name]; !ok {
				envBySvc[e.Name] = []string{} // ensure empty dirs show up
			}
			continue
		}
		// File: split path /var/run/agentry/<svc>/<key>
		// Take parent dir's name as service, leaf as env var.
		// e.Path is the full path; we split.
		rest := strings.TrimPrefix(e.Path, "/var/run/agentry/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		envBySvc[parts[0]] = append(envBySvc[parts[0]], parts[1])
	}
	out := make([]BindingInfo, 0, len(envBySvc))
	for svc, envs := range envBySvc {
		sort.Strings(envs)
		out = append(out, BindingInfo{Service: svc, EnvVars: envs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// listDirEntry is the trimmed shape we need from the runtime's
// /v1/file/list response: just enough to group bindings by service.
type listDirEntry struct {
	Name  string
	Path  string
	IsDir bool
}

// runtimeListDir calls POST /v1/file/list inside the sandbox.
// maxDepth=2 is enough to see /var/run/agentry/<svc>/<key> in one
// round trip.
func (p *Provisioner) runtimeListDir(ctx context.Context, sandboxID, path string, maxDepth int) ([]listDirEntry, error) {
	port, err := p.backend.GetNodePort(ctx, p.config.Namespace, "sandbox-"+sandboxID+"-svc")
	if err != nil || port == 0 {
		return nil, fmt.Errorf("sandbox %q not found", sandboxID)
	}
	base := fmt.Sprintf("http://%s:%d", p.config.NodeHost, port)
	showHidden := true
	body, _ := json.Marshal(map[string]any{
		"path":        path,
		"max_depth":   maxDepth,
		"show_hidden": showHidden,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/file/list", bytes.NewReader(body))
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
		return []listDirEntry{}, nil // dir doesn't exist == no bindings yet
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("file/list %s: status %d", path, resp.StatusCode)
	}
	// Runtime envelopes the body under {"data": {"files": [...]}}. Each
	// file row has at least `name`, `path`, `is_directory`.
	var wrap struct {
		Data struct {
			Files []struct {
				Name        string `json:"name"`
				Path        string `json:"path"`
				IsDirectory bool   `json:"is_directory"`
			} `json:"files"`
		} `json:"data"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("decode file/list: %w", err)
	}
	out := make([]listDirEntry, 0, len(wrap.Data.Files))
	for _, f := range wrap.Data.Files {
		out = append(out, listDirEntry{Name: f.Name, Path: f.Path, IsDir: f.IsDirectory})
	}
	return out, nil
}

// handleBindingResolve is GET /api/sandboxes/{id}/bindings/env. Reads
// the lockfile AND the credential files, returning the full env map.
// This is the privileged endpoint the control plane calls at deploy
// time to expand inheritance. Routed under bindings/env (not query
// param) so it's obviously separate from the safe list endpoint.
//
// Sensitive: keep the route auth-gated (the broker already requires a
// cluster admin cert to reach the provisioner; runtime tools cannot
// reach this from inside a sandbox).
func (p *Provisioner) handleBindingResolve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		errcode.WriteJSON(w, errcode.New(errcode.BindingInvalidRequest, "sandbox id missing in path"))
		return
	}
	lock, err := p.readLockfile(r.Context(), id)
	if err != nil {
		errcode.WriteJSON(w, errcode.New(errcode.BindingInternal, "read lockfile: %v", err))
		return
	}
	env := map[string]string{}
	sources := map[string]string{} // key → "<service>" or "secret"
	if lock != nil {
		for _, b := range lock.Bindings {
			for _, k := range b.EnvVars {
				path := fmt.Sprintf("/var/run/agentry/%s/%s", b.Service, k)
				raw, ferr := p.runtimeFileRead(r.Context(), id, path)
				if ferr != nil || len(raw) == 0 {
					continue // skip — caller can't override what doesn't exist
				}
				env[k] = string(raw)
				sources[k] = b.Service
			}
		}

		// Sandbox-staged secrets live alongside bindings under
		// /var/run/agentry/secrets/<NAME>, but they're tracked in
		// lock.Secrets, not lock.Bindings. Without this loop, secrets
		// the user set via `agentry env set` / the dashboard's Secrets
		// panel would show up in the sandbox runtime but vanish at
		// deploy time — the dashboard would silently fail to ship a
		// SESSION_KEY the user pasted into the sandbox a minute ago.
		// Source tag is "secret" so the dashboard can render it
		// distinctly from a service binding.
		for _, name := range lock.Secrets {
			if _, already := env[name]; already {
				// A service binding with the same key wins (service
				// bindings are managed; user secrets are user-managed).
				// The control plane's "Custom > inherited" precedence
				// still applies on top.
				continue
			}
			path := fmt.Sprintf("/var/run/agentry/secrets/%s", name)
			raw, ferr := p.runtimeFileRead(r.Context(), id, path)
			if ferr != nil || len(raw) == 0 {
				continue
			}
			env[name] = string(raw)
			sources[name] = "secret"
		}
	}
	writeJSON(w, 200, map[string]any{
		"env":     env,
		"sources": sources,
	})
}

// handleBindingCreate is POST /api/sandboxes/{id}/bindings. Looks up
// the service in the catalog, mints dev creds (stub for v1, XDP call
// later), writes the cred files into the sandbox via the runtime's
// file_write endpoint, and returns the env var names that the LLM /
// app code should read.
func (p *Provisioner) handleBindingCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		errcode.WriteJSON(w, errcode.New(errcode.BindingInvalidRequest, "sandbox id missing in path"))
		return
	}
	var req BindingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errcode.WriteJSON(w, errcode.New(errcode.BindingInvalidRequest, "bad request body: %v", err))
		return
	}
	if req.Service == "" {
		errcode.WriteJSON(w, errcode.New(errcode.BindingInvalidRequest, "service is required"))
		return
	}

	entry := p.catalog.Find("service", req.Service, req.Version)
	if entry == nil {
		errcode.WriteJSON(w, errcode.New(errcode.BindingServiceNotInCatalog,
			"service %q (version %q) not in cluster catalog", req.Service, req.Version))
		return
	}

	var creds map[string]string
	var expiresAt string
	if len(req.Env) > 0 {
		// User-supplied creds (agentry service bind interactive path).
		// Validate the keys match the catalog's declared env_vars —
		// keeps the LLM-readable contract honest and prevents typos
		// like TRINOURL from sneaking through.
		if err := validateBindingEnv(entry, req.Env); err != nil {
			errcode.WriteJSON(w, errcode.New(errcode.BindingInvalidRequest, "%v", err))
			return
		}
		creds = req.Env
	} else {
		// Idempotent re-bind: if the service is already bound for this
		// sandbox, return the existing env-var contract without
		// touching anything. Common when agentry mcp's PostCreateHook
		// applied a cluster-default before the user ran service_add.
		if existing := p.findExistingBinding(r.Context(), id, req.Service); existing != nil {
			writeJSON(w, 200, BindingResponse{
				Service: req.Service,
				Version: existing.Version,
				EnvVars: existing.EnvVars,
			})
			return
		}
		// External-only model: every bind must carry user-supplied
		// credentials. There's no stub-mint path because there are no
		// managed sibling services — the user (or operator default)
		// always provides the connection details.
		errcode.WriteJSON(w, errcode.New(errcode.BindingInvalidRequest,
			"env is required for service %q — pass field values as env map", req.Service))
		return
	}

	if err := p.writeBindingFiles(r.Context(), id, req.Service, creds); err != nil {
		errcode.WriteJSON(w, errcode.New(errcode.BindingInternal,
			"write creds into sandbox: %v", err))
		return
	}

	envVars := make([]string, 0, len(creds))
	for k := range creds {
		envVars = append(envVars, k)
	}
	// Pin the binding in the lockfile so build/deploy can emit a
	// reproducible manifest. Best-effort — a lockfile-write failure
	// shouldn't fail the bind itself, the creds are already on disk.
	if err := p.recordBindingInLockfile(r.Context(), id, req.Service, entry.Version, envVars); err != nil {
		// Log but continue; the operator can re-bind to refresh.
		// (We don't have a logger handle here; the error surfaces
		// in the next request's response if it matters.)
		_ = err
	}
	writeJSON(w, 200, BindingResponse{
		Service:   req.Service,
		Version:   entry.Version,
		EnvVars:   envVars,
		ExpiresAt: expiresAt,
	})
}

// writeBindingFiles writes each env var as a single-line file at
// /var/run/agentry/<service>/<env-var-name> inside the sandbox. The
// shell shim sources every file under /var/run/agentry/<*>/ on shell
// start and exports them as env vars.
//
// Why /var/run/agentry and not /etc/sandbox/creds/agentry: /var/run is NOT
// bind-mounted from the host. Only the provisioner writes here, so
// the binding pattern can't be subverted by host-side cred staging.
// Matches the convention XDP uses for K8s Secret-as-Volume mounts
// on the deploy side — same path in dev sandbox and prod pod.
func (p *Provisioner) writeBindingFiles(ctx context.Context, sandboxID, service string, creds map[string]string) error {
	for name, value := range creds {
		filePath := fmt.Sprintf("/var/run/agentry/%s/%s", service, name)
		if err := p.runtimeFileWrite(ctx, sandboxID, filePath, []byte(value)); err != nil {
			return err
		}
	}
	return nil
}

// findExistingBinding returns the lockfile entry for (sandbox, service)
// or nil if the service hasn't been bound yet. Errors from the runtime
// read collapse to "not found" — the caller treats that as "go ahead
// and bind", which is the safe default.
func (p *Provisioner) findExistingBinding(ctx context.Context, sandboxID, service string) *LockedBinding {
	lock, err := p.readLockfile(ctx, sandboxID)
	if err != nil || lock == nil {
		return nil
	}
	for i := range lock.Bindings {
		if lock.Bindings[i].Service == service {
			return &lock.Bindings[i]
		}
	}
	return nil
}

// validateBindingEnv checks that user-supplied env keys are within
// the catalog's declared env_vars list. Extra keys are rejected (the
// LLM-readable contract says "this service exposes exactly these
// vars"); missing keys are allowed (you might choose not to set a
// catalog field, e.g. omit TRINO_CATALOG to use the upstream default).
func validateBindingEnv(entry *CatalogEntry, env map[string]string) error {
	expected := map[string]bool{}
	if raw, ok := entry.Extra["env_vars"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				expected[s] = true
			}
		}
	}
	if len(expected) == 0 {
		return nil // catalog didn't declare an env contract — accept whatever
	}
	for k := range env {
		if !expected[k] {
			return fmt.Errorf("env var %q not in catalog for service %q (expected one of %v)",
				k, entry.Name, expected)
		}
	}
	return nil
}
