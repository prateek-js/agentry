package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agentry/agentry/pkg/errcode"
)

// BindingRequest is the body for POST /api/sandboxes/{id}/bindings.
//
// If Env is non-empty, those values are used VERBATIM — caller (i.e.
// the user, via `xdp service bind`) supplied them. When empty, the
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
// files inside the sandbox at /var/run/xdp/<service>/<key> and
// surface as env vars via the shell shim.
type BindingResponse struct {
	Service   string   `json:"service"`
	Version   string   `json:"version,omitempty"`
	EnvVars   []string `json:"env_vars"`
	ExpiresAt string   `json:"expires_at,omitempty"`
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
		// User-supplied creds (xdp service bind interactive path).
		// Validate the keys match the catalog's declared env_vars —
		// keeps the LLM-readable contract honest and prevents typos
		// like TRINOURL from sneaking through.
		if err := validateBindingEnv(entry, req.Env); err != nil {
			errcode.WriteJSON(w, errcode.New(errcode.BindingInvalidRequest, "%v", err))
			return
		}
		creds = req.Env
	} else {
		// Stub-mint path (LLM-driven service_bind MCP call). If the
		// service is ALREADY bound for this sandbox — typically by
		// xdp stdio's PostCreateHook applying a cluster-default with
		// real creds — return the existing env var contract without
		// overwriting. This keeps the LLM's "did you bind?" probe
		// idempotent and stops it from clobbering real values with
		// stubs after a cluster default has landed.
		if existing := p.findExistingBinding(r.Context(), id, req.Service); existing != nil {
			writeJSON(w, 200, BindingResponse{
				Service: req.Service,
				Version: existing.Version,
				EnvVars: existing.EnvVars,
			})
			return
		}
		var err error
		creds, expiresAt, err = p.mintDevBinding(r.Context(), id, req.Service)
		if err != nil {
			errcode.WriteJSON(w, errcode.New(errcode.BindingMintFailed,
				"mint creds for %s: %v", req.Service, err))
			return
		}
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

// mintDevBinding returns canned dev credentials for a known service.
// This is the stub — production swaps in an HTTP call to XDP's real
// dev-bind API and propagates the user identity from the request.
//
// Each service has a hand-crafted env-var contract that matches the
// catalog's documented env_vars list. Skills point the LLM at those
// canonical names.
func (p *Provisioner) mintDevBinding(_ context.Context, sandboxID, service string) (map[string]string, string, error) {
	expires := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	switch service {
	case "trino":
		return map[string]string{
			"TRINO_URL":      "https://trino-dev.us-west.acceldata.invalid:443",
			"TRINO_USER":     "dev-" + truncatePrefix(sandboxID, 12),
			"TRINO_PASSWORD": "stub-" + truncatePrefix(sandboxID, 16),
			"TRINO_CATALOG":  "hive",
		}, expires, nil
	case "spark":
		return map[string]string{
			"SPARK_MASTER":      "spark://spark-dev.us-west.acceldata.invalid:7077",
			"SPARK_HISTORY_URL": "https://spark-history-dev.us-west.acceldata.invalid",
		}, expires, nil
	}
	return nil, "", fmt.Errorf("no dev-bind stub for service %q", service)
}

// writeBindingFiles writes each env var as a single-line file at
// /var/run/xdp/<service>/<env-var-name> inside the sandbox. The
// shell shim sources every file under /var/run/xdp/<*>/ on shell
// start and exports them as env vars.
//
// Why /var/run/xdp and not /etc/sandbox/creds/xdp: /var/run is NOT
// bind-mounted from the host. Only the provisioner writes here, so
// the binding pattern can't be subverted by host-side cred staging.
// Matches the convention XDP uses for K8s Secret-as-Volume mounts
// on the deploy side — same path in dev sandbox and prod pod.
func (p *Provisioner) writeBindingFiles(ctx context.Context, sandboxID, service string, creds map[string]string) error {
	for name, value := range creds {
		filePath := fmt.Sprintf("/var/run/xdp/%s/%s", service, name)
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

func truncatePrefix(s string, n int) string {
	if len(s) <= n {
		return strings.ReplaceAll(s, "-", "")
	}
	return strings.ReplaceAll(s[:n], "-", "")
}
