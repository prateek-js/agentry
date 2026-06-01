// Package mcp implements the ad-sandbox MCP server: a thin Go binary that
// exposes the runtime + provisioner HTTP APIs as Model Context Protocol
// tools over stdio.
//
// The package is split so the HTTP-level client (this file) and the tool
// schemas (tools.go) can be tested independently of the MCP SDK plumbing.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTimeout caps a single HTTP call from MCP tools. Shell exec can be
// long-running; MCP clients are expected to set their own deadlines too.
const DefaultTimeout = 120 * time.Second

// Client is a small HTTP wrapper around the ad-sandbox APIs. It is safe
// for concurrent use.
type Client struct {
	provisionerURL string
	httpClient     *http.Client
	apiKey         string

	// PostCreateHook, if set, runs after a successful sandbox_create.
	// agentry mcp uses this to auto-apply cluster-default service binds
	// the user staged with `agentry service bind <service>`. Returning an
	// error logs but does NOT fail the create — partial wiring is
	// better than no sandbox at all.
	PostCreateHook func(ctx context.Context, info SandboxInfo) error
}

// Config drives Client construction.
type Config struct {
	ProvisionerURL string
	APIKey         string // optional; required when servers run with SANDBOX_API_KEY
	HTTPClient     *http.Client

	// PostCreateHook is invoked from sandboxCreate after the create
	// succeeds. See Client.PostCreateHook for semantics.
	PostCreateHook func(ctx context.Context, info SandboxInfo) error
}

// NewClient builds a Client. Empty ProvisionerURL is fine — sandbox_*
// tools will simply error until set.
func NewClient(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{
		provisionerURL: strings.TrimRight(cfg.ProvisionerURL, "/"),
		httpClient:     hc,
		apiKey:         cfg.APIKey,
		PostCreateHook: cfg.PostCreateHook,
	}
}

// SandboxInfo mirrors provisioner.SandboxInfo. Duplicated here so MCP
// callers don't transitively need the provisioner package.
type SandboxInfo struct {
	SandboxID  string `json:"sandbox_id"`
	SandboxURL string `json:"sandbox_url"`
	Status     string `json:"status"`
	ExpiresAt  string `json:"expires_at,omitempty"`
}

// CreateRequest is the JSON body for POST /api/sandboxes. Only the fields
// MCP currently surfaces are included; richer args (resources, volumes)
// can pass through as `extra` raw JSON if needed.
type CreateRequest struct {
	SandboxID    string `json:"sandbox_id"`
	TTLSeconds   int64  `json:"ttl_seconds,omitempty"`
	RuntimeClass string `json:"runtime_class,omitempty"`
}

// RenewRequest is the JSON body for POST /api/sandboxes/{id}/renew.
type RenewRequest struct {
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// CreateSandbox calls POST /api/sandboxes.
func (c *Client) CreateSandbox(ctx context.Context, req CreateRequest) (SandboxInfo, error) {
	var out SandboxInfo
	err := c.do(ctx, http.MethodPost, c.provisionerURL+"/api/sandboxes", req, &out)
	return out, err
}

// ListSandboxes calls GET /api/sandboxes.
func (c *Client) ListSandboxes(ctx context.Context) ([]SandboxInfo, error) {
	var wrap struct {
		Sandboxes []SandboxInfo `json:"sandboxes"`
	}
	if err := c.do(ctx, http.MethodGet, c.provisionerURL+"/api/sandboxes", nil, &wrap); err != nil {
		return nil, err
	}
	return wrap.Sandboxes, nil
}

// GetSandbox calls GET /api/sandboxes/{id}.
func (c *Client) GetSandbox(ctx context.Context, id string) (SandboxInfo, error) {
	var out SandboxInfo
	err := c.do(ctx, http.MethodGet, c.provisionerURL+"/api/sandboxes/"+id, nil, &out)
	return out, err
}

// DeleteSandbox calls DELETE /api/sandboxes/{id}.
func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, c.provisionerURL+"/api/sandboxes/"+id, nil, nil)
}

// CatalogEntry mirrors provisioner.CatalogEntry. Duplicated here so
// MCP callers don't transitively need the provisioner package.
type CatalogEntry struct {
	Kind        string         `json:"kind"`
	Name        string         `json:"name"`
	Version     string         `json:"version"`
	Description string         `json:"description"`
	Tags        []string       `json:"tags,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// ListCatalog calls GET /api/catalog (optionally filtered by kind).
// Returns the per-cluster catalog of services / dev-deps / skills.
func (c *Client) ListCatalog(ctx context.Context, kind string) ([]CatalogEntry, error) {
	url := c.provisionerURL + "/api/catalog"
	if kind != "" {
		url += "?kind=" + kind
	}
	var wrap struct {
		Entries []CatalogEntry `json:"entries"`
	}
	if err := c.do(ctx, http.MethodGet, url, nil, &wrap); err != nil {
		return nil, err
	}
	return wrap.Entries, nil
}

// BindingResponse documents what BindService returned.
type BindingResponse struct {
	Service   string   `json:"service"`
	Version   string   `json:"version,omitempty"`
	EnvVars   []string `json:"env_vars"`
	ExpiresAt string   `json:"expires_at,omitempty"`
}

// BindService calls POST /api/sandboxes/{id}/bindings. Wires the
// named cluster service into the sandbox; the LLM's code then reads
// the canonical env vars listed in env_vars.
func (c *Client) BindService(ctx context.Context, sandboxID, service, version string) (BindingResponse, error) {
	body := map[string]string{"service": service}
	if version != "" {
		body["version"] = version
	}
	var out BindingResponse
	err := c.do(ctx, http.MethodPost, c.provisionerURL+"/api/sandboxes/"+sandboxID+"/bindings", body, &out)
	return out, err
}

// SetSecret calls POST /api/sandboxes/{id}/secrets. Source defaults
// to "mcp" — the provisioner uses this to reject secret-shaped
// values (forces them through `agentry env set` on the user's terminal).
func (c *Client) SetSecret(ctx context.Context, sandboxID, name, value, source string) error {
	if source == "" {
		source = "mcp"
	}
	body := map[string]string{"name": name, "value": value, "source": source}
	return c.do(ctx, http.MethodPost, c.provisionerURL+"/api/sandboxes/"+sandboxID+"/secrets", body, nil)
}

// ListSecrets returns names only — never values — by design.
func (c *Client) ListSecrets(ctx context.Context, sandboxID string) ([]string, error) {
	var wrap struct {
		Names []string `json:"names"`
	}
	err := c.do(ctx, http.MethodGet, c.provisionerURL+"/api/sandboxes/"+sandboxID+"/secrets", nil, &wrap)
	return wrap.Names, err
}

// BuildResponse mirrors provisioner.BuildResponse — duplicated to
// avoid the mcp package depending on the provisioner package.
type BuildResponse struct {
	Image      string         `json:"image"`
	Manifest   map[string]any `json:"manifest"`
	Dockerfile string         `json:"dockerfile,omitempty"`
}

// Build calls POST /api/sandboxes/{id}/build.
func (c *Client) Build(ctx context.Context, sandboxID string) (BuildResponse, error) {
	var out BuildResponse
	err := c.do(ctx, http.MethodPost, c.provisionerURL+"/api/sandboxes/"+sandboxID+"/build", map[string]any{}, &out)
	return out, err
}

// DeployResponse is what the deploy endpoint returns. The stub XDP
// at the broker synthesises this; production swaps in real values.
type DeployResponse struct {
	DeploymentID string `json:"deployment_id"`
	Cluster      string `json:"cluster"`
	Name         string `json:"name"`
	PublicURL    string `json:"public_url"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
}

// Deploy calls POST /api/sandboxes/{id}/deploy. Builds implicitly.
func (c *Client) Deploy(ctx context.Context, sandboxID, cluster string) (DeployResponse, error) {
	body := map[string]any{}
	if cluster != "" {
		body["cluster"] = cluster
	}
	var out DeployResponse
	err := c.do(ctx, http.MethodPost, c.provisionerURL+"/api/sandboxes/"+sandboxID+"/deploy", body, &out)
	return out, err
}

// RenewSandbox calls POST /api/sandboxes/{id}/renew.
func (c *Client) RenewSandbox(ctx context.Context, id string, req RenewRequest) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, c.provisionerURL+"/api/sandboxes/"+id+"/renew", req, &out)
	return out, err
}

// Runtime-side operations. They take a sandboxURL discovered via
// CreateSandbox or ListSandboxes — the MCP server itself is stateless.

// ExecRequest is the JSON body for POST /v1/shell/exec on the runtime.
type ExecRequest struct {
	Command string  `json:"command"`
	ID      string  `json:"id,omitempty"`
	ExecDir string  `json:"exec_dir,omitempty"`
	Timeout float64 `json:"timeout,omitempty"`
}

// ExecResult is the data payload of /v1/shell/exec.
type ExecResult struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Output    string `json:"output"`
	ExitCode  int    `json:"exit_code"`
}

// shellResponse mirrors models.Response wrapping ExecResult.
type shellResponse struct {
	Success bool       `json:"success"`
	Message string     `json:"message"`
	Data    ExecResult `json:"data"`
}

// Exec runs a command against a specific sandbox runtime.
func (c *Client) Exec(ctx context.Context, sandboxURL string, req ExecRequest) (ExecResult, error) {
	var wrap shellResponse
	if err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/shell/exec", req, &wrap); err != nil {
		return ExecResult{}, err
	}
	if !wrap.Success && wrap.Message != "" {
		return wrap.Data, fmt.Errorf("%s", wrap.Message)
	}
	return wrap.Data, nil
}

// FileReadRequest is the JSON body for POST /v1/file/read.
type FileReadRequest struct {
	File      string `json:"file"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

// FileWriteRequest is the JSON body for POST /v1/file/write.
type FileWriteRequest struct {
	File    string `json:"file"`
	Content string `json:"content"`
	Append  bool   `json:"append,omitempty"`
}

// FileListRequest is the JSON body for POST /v1/file/list.
type FileListRequest struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// ReadFile calls /v1/file/read and returns the raw JSON response so we can
// keep the contract loose (the runtime returns a wrapped Response{Data}).
func (c *Client) ReadFile(ctx context.Context, sandboxURL string, req FileReadRequest) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/file/read", req, &out)
	return out, err
}

func (c *Client) WriteFile(ctx context.Context, sandboxURL string, req FileWriteRequest) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/file/write", req, &out)
	return out, err
}

func (c *Client) ListFiles(ctx context.Context, sandboxURL string, req FileListRequest) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/file/list", req, &out)
	return out, err
}

// ── File search / replace ──────────────────────────────────────────────────

type FileFindRequest struct {
	Path string `json:"path"`
	Glob string `json:"glob"`
}

type FileSearchRequest struct {
	File  string `json:"file"`
	Regex string `json:"regex"`
}

type FileReplaceRequest struct {
	File   string `json:"file"`
	OldStr string `json:"old_str"`
	NewStr string `json:"new_str"`
}

func (c *Client) FindFiles(ctx context.Context, sandboxURL string, req FileFindRequest) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/file/find", req, &out)
	return out, err
}

func (c *Client) SearchInFile(ctx context.Context, sandboxURL string, req FileSearchRequest) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/file/search", req, &out)
	return out, err
}

func (c *Client) ReplaceInFile(ctx context.Context, sandboxURL string, req FileReplaceRequest) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/file/replace", req, &out)
	return out, err
}

// ── Background commands ────────────────────────────────────────────────────

type BgStartRequest struct {
	Command string            `json:"command"`
	ExecDir string            `json:"exec_dir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

func (c *Client) BgStart(ctx context.Context, sandboxURL string, req BgStartRequest) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/shell/background", req, &out)
	return out, err
}

func (c *Client) BgStatus(ctx context.Context, sandboxURL, id string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, sandboxURL+"/v1/shell/background/"+id, nil, &out)
	return out, err
}

func (c *Client) BgLogs(ctx context.Context, sandboxURL, id string, cursor int64) (map[string]any, error) {
	url := fmt.Sprintf("%s/v1/shell/background/%s/logs?cursor=%d", sandboxURL, id, cursor)
	var out map[string]any
	err := c.do(ctx, http.MethodGet, url, nil, &out)
	return out, err
}

func (c *Client) BgInterrupt(ctx context.Context, sandboxURL, id string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/shell/background/"+id+"/interrupt", nil, &out)
	return out, err
}

func (c *Client) BgList(ctx context.Context, sandboxURL string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, sandboxURL+"/v1/shell/background", nil, &out)
	return out, err
}

// ── Port management ────────────────────────────────────────────────────────

type PortWaitRequest struct {
	Port           int `json:"port"`
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

func (c *Client) PortWait(ctx context.Context, sandboxURL string, req PortWaitRequest) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/ports/wait", req, &out)
	return out, err
}

// ── Project manager ────────────────────────────────────────────────────────

type ProjectNameRequest struct {
	Name string `json:"name"`
}

func (c *Client) ProjectStart(ctx context.Context, sandboxURL, name string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/project/start", ProjectNameRequest{Name: name}, &out)
	return out, err
}

func (c *Client) ProjectStop(ctx context.Context, sandboxURL, name string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/project/stop", ProjectNameRequest{Name: name}, &out)
	return out, err
}

func (c *Client) ProjectRestart(ctx context.Context, sandboxURL, name string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/project/restart", ProjectNameRequest{Name: name}, &out)
	return out, err
}

func (c *Client) ProjectList(ctx context.Context, sandboxURL string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, sandboxURL+"/v1/project/list", nil, &out)
	return out, err
}

func (c *Client) ProjectLogs(ctx context.Context, sandboxURL, name string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, sandboxURL+"/v1/project/logs?name="+name, nil, &out)
	return out, err
}

// ProjectStartAll starts every project under /workspace/projects/,
// respecting each project's depends_on ordering. The right verb when
// the user wants the whole workspace up in one call.
func (c *Client) ProjectStartAll(ctx context.Context, sandboxURL string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/project/start-all", nil, &out)
	return out, err
}

// ── Code interpreter (Jupyter kernels) ────────────────────────────────────

type CodeContextCreateRequest struct {
	Language  string `json:"language,omitempty"`   // default: python
	ContextID string `json:"context_id,omitempty"` // optional; server generates if empty
}

type CodeExecRequest struct {
	Code           string `json:"code"`
	TimeoutSeconds int    `json:"timeout,omitempty"`
}

// CodeExecResult is the structured aggregation of the SSE stream
// returned by /v1/code/contexts/{id}/exec. The runtime streams a
// sequence of stream/result/display/error/status/reply events; we
// fold them into one tidy object so MCP callers (and the model)
// see a single tool result.
type CodeExecResult struct {
	// ContextID is the kernel context the model should pass back to
	// keep state continuity across calls. The MCP layer fills this in
	// from the request side so the model never has to remember it
	// across the original create/exec separation.
	ContextID string `json:"context_id,omitempty"`

	// Status is the shell-side execute_reply outcome. "ok", "error",
	// or "abort". "incomplete" is our local fallback when the stream
	// closed without a reply (shouldn't happen normally).
	Status string `json:"status"`

	// ExecutionCount is the kernel's running count (the `In[N]` from
	// Jupyter). Useful for debugging.
	ExecutionCount int `json:"execution_count,omitempty"`

	// Stdout / Stderr concatenate every `stream` event in order.
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`

	// Result is the data dict of the LAST `execute_result` event
	// (the value of the cell's trailing expression). Keys are MIME
	// types; "text/plain" is always present.
	Result map[string]any `json:"result,omitempty"`

	// Displays accumulates every `display_data` event in order. Each
	// entry's "data" map carries MIME-typed payloads (e.g. PNG bytes
	// under "image/png" as base64 strings).
	Displays []map[string]any `json:"displays,omitempty"`

	// Error is set when execution raised. Traceback is the kernel's
	// formatted multi-line traceback.
	Error *CodeError `json:"error,omitempty"`

	// Dropped tells you if the iopub stream was lossy because the
	// 256-message subscriber buffer overflowed. >0 means there was a
	// gap in the output you saw.
	Dropped int64 `json:"dropped,omitempty"`
}

type CodeError struct {
	Ename     string   `json:"ename"`
	Evalue    string   `json:"evalue"`
	Traceback []string `json:"traceback,omitempty"`
}

// CreateContext spawns a kernel and returns its identifier.
func (c *Client) CreateContext(ctx context.Context, sandboxURL string, req CodeContextCreateRequest) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/code/contexts", req, &out)
	return out, err
}

func (c *Client) ListContexts(ctx context.Context, sandboxURL string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, sandboxURL+"/v1/code/contexts", nil, &out)
	return out, err
}

func (c *Client) DeleteContext(ctx context.Context, sandboxURL, contextID string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodDelete, sandboxURL+"/v1/code/contexts/"+contextID, nil, &out)
	return out, err
}

func (c *Client) InterruptContext(ctx context.Context, sandboxURL, contextID string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodPost, sandboxURL+"/v1/code/contexts/"+contextID+"/interrupt", nil, &out)
	return out, err
}

// ExecCode posts to /v1/code/contexts/{id}/exec, drains the SSE
// stream, and aggregates the events into a single CodeExecResult.
//
// We don't expose a streaming API here because MCP tool calls are
// request/response — the model wants one tidy result per call.
func (c *Client) ExecCode(ctx context.Context, sandboxURL, contextID string, req CodeExecRequest) (CodeExecResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return CodeExecResult{}, fmt.Errorf("marshal: %w", err)
	}
	url := sandboxURL + "/v1/code/contexts/" + contextID + "/exec"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return CodeExecResult{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("X-Sandbox-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return CodeExecResult{}, fmt.Errorf("exec: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		// Try to surface the runtime's `{"message":"…"}` if present.
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(buf, &e) == nil && e.Message != "" {
			return CodeExecResult{}, fmt.Errorf("exec: %s (status %d)", e.Message, resp.StatusCode)
		}
		return CodeExecResult{}, fmt.Errorf("exec: status %d: %s", resp.StatusCode, buf)
	}
	return parseSSEResult(resp.Body)
}

// parseSSEResult drains a text/event-stream into a CodeExecResult.
// Each SSE frame is two lines ("event: NAME\ndata: JSON") followed by
// a blank line. We use bufio.Scanner with a generous buffer so a long
// traceback doesn't bust the default 64 KiB line cap.
func parseSSEResult(r io.Reader) (CodeExecResult, error) {
	out := CodeExecResult{}
	var stdout, stderr strings.Builder

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

	var event, data string
	finalize := func() {
		if event == "" {
			return
		}
		switch event {
		case "stream":
			var s struct{ Name, Text string }
			if json.Unmarshal([]byte(data), &s) == nil {
				if s.Name == "stderr" {
					stderr.WriteString(s.Text)
				} else {
					stdout.WriteString(s.Text)
				}
			}
		case "result":
			var r struct {
				Data           map[string]any `json:"data"`
				ExecutionCount int            `json:"execution_count"`
			}
			if json.Unmarshal([]byte(data), &r) == nil {
				out.Result = r.Data
				if r.ExecutionCount > 0 {
					out.ExecutionCount = r.ExecutionCount
				}
			}
		case "display":
			var d map[string]any
			if json.Unmarshal([]byte(data), &d) == nil {
				out.Displays = append(out.Displays, d)
			}
		case "error":
			var e CodeError
			if json.Unmarshal([]byte(data), &e) == nil {
				out.Error = &e
			}
		case "reply":
			var rep struct {
				Status         string `json:"status"`
				ExecutionCount int    `json:"execution_count"`
			}
			if json.Unmarshal([]byte(data), &rep) == nil {
				out.Status = rep.Status
				if rep.ExecutionCount > 0 {
					out.ExecutionCount = rep.ExecutionCount
				}
			}
		case "done":
			var d struct {
				Dropped int64 `json:"dropped"`
			}
			if json.Unmarshal([]byte(data), &d) == nil {
				out.Dropped = d.Dropped
			}
		}
		event, data = "", ""
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			finalize()
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	finalize()

	out.Stdout = stdout.String()
	out.Stderr = stderr.String()
	if out.Status == "" {
		if out.Error != nil {
			out.Status = "error"
		} else {
			out.Status = "incomplete"
		}
	}
	return out, sc.Err()
}

// do is the single round-trip helper. body may be nil for verbs that send
// no body; out may be nil to discard the response.
func (c *Client) do(ctx context.Context, method, url string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("X-Sandbox-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		// Surface the server's message verbatim where possible. Servers in
		// this codebase wrap errors as `{"success":false,"message":"…"}`.
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(respBody, &e) == nil && e.Message != "" {
			return fmt.Errorf("%s %s: %s (status %d)", method, url, e.Message, resp.StatusCode)
		}
		return fmt.Errorf("%s %s: status %d: %s", method, url, resp.StatusCode, respBody)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode %T: %w", out, err)
	}
	return nil
}
