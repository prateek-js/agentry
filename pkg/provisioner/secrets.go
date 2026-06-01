package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/agentry/agentry/pkg/errcode"
)

// SecretRequest is the body for POST /api/sandboxes/{id}/secrets.
// Source distinguishes user-staged (terminal) entries from MCP-driven
// ones; production-deploy code rejects MCP-driven values that look
// like secrets.
type SecretRequest struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source,omitempty"` // "cli" | "mcp" — defaults to "cli"
}

// SecretResponse echoes back the name (never the value). LLM tooling
// uses this to learn that a secret is now available without seeing
// what it is.
type SecretResponse struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// SecretListResponse — names only, NEVER values. The list endpoint
// is safe to expose via MCP so the LLM knows what's available.
type SecretListResponse struct {
	Names []string `json:"names"`
}

// secretPatterns matches common shapes for things callers shouldn't
// paste into an LLM tool. Used to reject MCP env::set calls that
// look like real secrets — those have to go through `agentry env set`
// on the user's terminal where the value never enters chat context.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^sk-[A-Za-z0-9_-]{20,}$`),           // OpenAI-style
	regexp.MustCompile(`^sk-ant-[A-Za-z0-9_-]{20,}$`),       // Anthropic-style
	regexp.MustCompile(`^xoxb-[A-Za-z0-9-]{20,}$`),          // Slack bot
	regexp.MustCompile(`^xoxp-[A-Za-z0-9-]{20,}$`),          // Slack user
	regexp.MustCompile(`^ghp_[A-Za-z0-9]{30,}$`),            // GitHub PAT (classic)
	regexp.MustCompile(`^github_pat_[A-Za-z0-9_]{50,}$`),    // GitHub PAT (fine-grained)
	regexp.MustCompile(`^AKIA[0-9A-Z]{16}$`),                // AWS Access Key
	regexp.MustCompile(`^[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}$`), // JWT
}

// LooksLikeSecret returns true when the value matches any of the
// well-known secret patterns. Used to gate the MCP env_set tool.
func LooksLikeSecret(v string) bool {
	if v == "" {
		return false
	}
	for _, p := range secretPatterns {
		if p.MatchString(v) {
			return true
		}
	}
	return false
}

// handleSecretSet is POST /api/sandboxes/{id}/secrets. Writes the
// value to /etc/sandbox/creds/agentry/secrets/<NAME> inside the sandbox.
// The shell shim exports them under the same name on next shell
// start. At deploy, they're declared in the manifest as `secrets:`.
func (p *Provisioner) handleSecretSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		errcode.WriteJSON(w, errcode.New(errcode.BindingInvalidRequest, "sandbox id missing in path"))
		return
	}
	var req SecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errcode.WriteJSON(w, errcode.New(errcode.BindingInvalidRequest, "bad request body: %v", err))
		return
	}
	if req.Name == "" {
		errcode.WriteJSON(w, errcode.New(errcode.BindingInvalidRequest, "name is required"))
		return
	}
	if !envNameOK(req.Name) {
		errcode.WriteJSON(w, errcode.New(errcode.InvalidValue,
			"name must match [A-Z_][A-Z0-9_]* (got %q)", req.Name))
		return
	}
	source := req.Source
	if source == "" {
		source = "cli"
	}
	// MCP-driven setters can't smuggle secrets — that's the whole
	// point of the user-only `agentry env set` path. CLI source is
	// trusted to have come from a human-entered prompt.
	if source == "mcp" && LooksLikeSecret(req.Value) {
		errcode.WriteJSON(w, errcode.New(errcode.SecretLooksLikeSecret,
			"value looks like a secret (matches a well-known token pattern); set it via `agentry env set %s` in your terminal instead so it doesn't enter chat context",
			req.Name))
		return
	}

	filePath := "/var/run/agentry/secrets/" + req.Name
	if err := p.runtimeFileWrite(r.Context(), id, filePath, []byte(req.Value)); err != nil {
		errcode.WriteJSON(w, errcode.New(errcode.BindingInternal,
			"write secret into sandbox: %v", err))
		return
	}
	if err := p.recordSecretInLockfile(r.Context(), id, req.Name); err != nil {
		_ = err
	}
	writeJSON(w, 200, SecretResponse{Name: req.Name, Source: source})
}

// handleSecretList is GET /api/sandboxes/{id}/secrets. Names only.
func (p *Provisioner) handleSecretList(w http.ResponseWriter, r *http.Request) {
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
	names := []string{}
	if lock != nil {
		names = append(names, lock.Secrets...)
	}
	sort.Strings(names)
	writeJSON(w, 200, SecretListResponse{Names: names})
}

// recordSecretInLockfile dedups by name. Re-setting the same secret
// doesn't add a duplicate entry.
func (p *Provisioner) recordSecretInLockfile(ctx context.Context, sandboxID, name string) error {
	return p.updateLockfile(ctx, sandboxID, func(l *Lockfile) {
		for _, n := range l.Secrets {
			if n == name {
				return
			}
		}
		l.Secrets = append(l.Secrets, name)
	})
}

// envNameOK validates the env var name shape. Limits to the POSIX
// shell-safe pattern so the shim's `export $name=$value` is always
// well-formed.
func envNameOK(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	if strings.ContainsAny(name, " \t\n=/") {
		return false
	}
	for i, c := range name {
		if i == 0 && (c >= '0' && c <= '9') {
			return false
		}
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '_':
		default:
			return false
		}
	}
	return true
}

