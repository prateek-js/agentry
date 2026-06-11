package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// startServerWithMockBackend wires:
//
//	test MCP client ⇄ (in-memory) ⇄ MCP server ⇄ HTTP ⇄ mock provisioner+runtime
//
// All four moving pieces live in the test process so we can assert end-to-end
// behaviour without subprocess setup.
func startServerWithMockBackend(t *testing.T) (*sdkmcp.ClientSession, *httptest.Server) {
	t.Helper()

	var backendBaseURL string // set after backend starts (closure-captured)
	backend := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /api/sandboxes": func(w http.ResponseWriter, _ *http.Request) {
			// SandboxURL points back at the same fake backend with a
			// per-sandbox prefix. That lets us mock runtime-side
			// endpoints (file/*, etc.) here too — and lets file tool
			// tests assert that sandbox_url defaulted from the cache.
			_ = json.NewEncoder(w).Encode(SandboxInfo{
				SandboxID: "sb1", SandboxURL: backendBaseURL + "/sb1", Status: "Running",
			})
		},
		// Runtime-side endpoints reached at SandboxURL+"/v1/...".
		// They live on the same test server so the mock POST /api/sandboxes
		// can hand back a URL that actually responds.
		"POST /sb1/v1/file/read": func(w http.ResponseWriter, r *http.Request) {
			// Echo the body's `format` field back inside `data.content`
			// so tests can assert the MCP layer set the right default.
			b, _ := io.ReadAll(r.Body)
			fmtSeen := "raw"
			if strings.Contains(string(b), `"format":"numbered"`) {
				fmtSeen = "numbered"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"file":        "/x",
					"content":     "format=" + fmtSeen,
					"total_lines": 1,
				},
			})
		},
		"POST /sb1/v1/file/grep": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			// Surface what we received so the test can match on regex.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"path": "/x",
					"matches": []map[string]any{
						{"file": "/x/a.go", "line": 1, "text": "echo:" + string(b)},
					},
					"total_found": 1,
				},
			})
		},
		"POST /sb1/v1/file/multi_edit": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"file":  "/x",
					"steps": []map[string]any{{"old_str": "echo", "replaced_count": 1}},
					"echo":  string(b),
				},
			})
		},
		"GET /api/sandboxes": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sandboxes": []SandboxInfo{{SandboxID: "sb1", SandboxURL: "http://sb/sb1"}},
			})
		},
		"GET /api/sandboxes/sb1": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(SandboxInfo{SandboxID: "sb1", Status: "Running"})
		},
		"GET /api/sandboxes/sb1/bindings": func(w http.ResponseWriter, _ *http.Request) {
			// Default snapshot used by every test: the user staged a
			// mongodb cluster-default, post-create hook applied it.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"bindings": []SandboxBinding{
					{Service: "mongodb", EnvVars: []string{"DATABASE_URL", "MONGODB_URI", "MONGODB_URL"}},
				},
			})
		},
		"GET /api/sandboxes/sb1/secrets": func(w http.ResponseWriter, _ *http.Request) {
			// Cluster-default env vars the operator staged via
			// `agentry env set NAME` — names only, never values.
			// sandbox_create must surface these so the LLM doesn't
			// announce a credential is missing while it's already
			// in process.env.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"names": []string{"JIRA_TOKEN", "OPENAI_API_KEY"},
			})
		},
		"POST /api/sandboxes/sb1/renew": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sandbox_id":  "sb1",
				"expires_at":  "2026-05-13T00:00:00Z",
				"body_echo":   string(b),
				"ttl_seconds": 7200,
			})
		},
		"DELETE /api/sandboxes/sb1": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		},
	})
	// Tell the runtime-side handlers above where to find themselves.
	// Closure capture means later updates are visible at request time.
	backendBaseURL = backend.URL
	t.Cleanup(backend.Close)

	c := NewClient(Config{ProvisionerURL: backend.URL})
	server := NewServer(c)

	clientT, serverT := sdkmcp.NewInMemoryTransports()
	ctx := context.Background()
	go func() {
		if err := server.Run(ctx, serverT); err != nil &&
			!strings.Contains(err.Error(), "closed") {
			t.Logf("server.Run returned: %v", err)
		}
	}()

	mcpClient := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := mcpClient.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = session.Close()
	})
	return session, backend
}

func callTool(t *testing.T, s *sdkmcp.ClientSession, name string, args map[string]any) *sdkmcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

func TestServerListsExpectedTools(t *testing.T) {
	session, _ := startServerWithMockBackend(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	got := make(map[string]struct{}, len(list.Tools))
	for _, tool := range list.Tools {
		got[tool.Name] = struct{}{}
	}
	for _, want := range []string{
		// Lifecycle (sandbox_preflight removed — preflight is now an
		// internal step of the dashboard's Deploy flow, not an LLM
		// tool, since the pause/cleanup choreography around it only
		// makes sense inside the full deploy).
		"sandbox_create", "sandbox_list", "sandbox_delete",
		// Catalog + bindings + secrets (build/deploy MCP tools were
		// removed; deploy lives in the dashboard).
		"service_list", "service_bind", "secret_set", "secret_list",
		// Shell
		"command_run", "command_start", "command_logs", "command_interrupt",
		// Docs + files
		"docs_read",
		"file_read", "file_write", "file_list", "file_grep", "file_replace", "file_multi_edit",
		// Ports
		"port_wait",
		// Project manager (one project per sandbox — no project_start_all)
		"project_create", "project_start", "project_stop", "project_list", "project_logs",
		// Code interpreter (Jupyter)
		"code_exec", "code_close",
		// Observability probes + deployment status
		"app_probe", "service_probe", "deployment_status",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing tool %q", want)
		}
	}
	if len(got) != 29 {
		t.Errorf("tool count = %d; want 29 (26 + app_probe + service_probe + deployment_status)", len(got))
	}
}

func TestSandboxCreateRoundTrip(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	res := callTool(t, session, "sandbox_create", map[string]any{
		"sandbox_id":    "sb1",
		"ttl_seconds":   3600,
		"runtime_class": "gvisor",
	})
	if res.IsError {
		t.Fatalf("IsError; content=%+v", res.Content)
	}
	text := contentText(t, res)
	if !strings.Contains(text, `"sandbox_id": "sb1"`) {
		t.Errorf("text result missing sandbox_id: %s", text)
	}
}

// TestSandboxCreateSurfacesBindings is the load-bearing assertion
// for the "LLM scaffolds mongodb-memory-server while a real binding
// exists" fix. The post-create hook applies the staged binding, then
// sandbox_create's response MUST include it so the LLM can read the
// env-var names instead of reaching for an in-process substitute.
func TestSandboxCreateSurfacesBindings(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	res := callTool(t, session, "sandbox_create", map[string]any{"sandbox_id": "sb1"})
	if res.IsError {
		t.Fatalf("IsError; content=%+v", res.Content)
	}
	text := contentText(t, res)
	// Both the service name and the env var names must round-trip so
	// the LLM has everything it needs in one response.
	for _, want := range []string{"mongodb", "DATABASE_URL", "MONGODB_URI", "MONGODB_URL"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in sandbox_create response: %s", want, text)
		}
	}
}

// TestSandboxCreateSurfacesStagedEnvNames is the load-bearing
// assertion for the "LLM tells the user JIRA isn't configured while
// JIRA_TOKEN sits in the cluster-default env store" fix. The
// post-create hook applies cluster-default secrets; sandbox_create's
// response MUST surface the names (values stay server-side) so the
// model can match a user-named service against what's already loaded
// before declaring it missing.
func TestSandboxCreateSurfacesStagedEnvNames(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	res := callTool(t, session, "sandbox_create", map[string]any{"sandbox_id": "sb1"})
	if res.IsError {
		t.Fatalf("IsError; content=%+v", res.Content)
	}
	text := contentText(t, res)
	for _, want := range []string{`"env"`, "JIRA_TOKEN", "OPENAI_API_KEY"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in sandbox_create response: %s", want, text)
		}
	}
}

func TestSandboxCreateMissingIDIsErrorResult(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	res := callTool(t, session, "sandbox_create", map[string]any{})
	if !res.IsError {
		t.Fatalf("missing sandbox_id should produce IsError result, got %+v", res)
	}
}

// TestSandboxListWithoutIDHidesURL is the load-bearing assertion for
// the "LLM grabs a random sandbox URL from a list call and starts
// writing files into it" fix. Without an explicit sandbox_id, the
// list endpoint must NOT surface sandbox_url — every URL the model
// sees is an invitation to write into someone else's project.
func TestSandboxListWithoutIDHidesURL(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	res := callTool(t, session, "sandbox_list", map[string]any{})
	if res.IsError {
		t.Fatalf("IsError; content=%+v", res.Content)
	}
	text := contentText(t, res)
	if !strings.Contains(text, "sb1") {
		t.Errorf("expected sandbox_id in list output: %s", text)
	}
	// The mock POST handler returns "http://sb/sb1" for sandboxes; if
	// that URL leaks through, our strip-on-list contract is broken.
	if strings.Contains(text, "http://sb/sb1") || strings.Contains(text, `"sandbox_url"`) {
		t.Errorf("sandbox_list without sandbox_id leaked sandbox_url: %s", text)
	}
}

// With an explicit sandbox_id the LLM is signalling "I know which
// sandbox I want" — the URL must come back. The asymmetry is the
// whole point of the no-URL-on-list rule.
func TestSandboxListWithIDReturnsURL(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	res := callTool(t, session, "sandbox_list", map[string]any{"sandbox_id": "sb1"})
	if res.IsError {
		t.Fatalf("IsError; content=%+v", res.Content)
	}
	text := contentText(t, res)
	if !strings.Contains(text, `"sandbox_url"`) {
		t.Errorf("sandbox_list with sandbox_id should return sandbox_url: %s", text)
	}
}

// TestFileToolImplicitSandboxURL is the load-bearing assertion for
// the "stop pasting bridge.invalid into every call" change. After
// sandbox_create caches the URL, follow-up file tools called WITHOUT
// sandbox_url must route to that same sandbox via the cache.
func TestFileToolImplicitSandboxURL(t *testing.T) {
	session, _ := startServerWithMockBackend(t)

	// Prime the cache via sandbox_create.
	if res := callTool(t, session, "sandbox_create", map[string]any{"sandbox_id": "sb1"}); res.IsError {
		t.Fatalf("sandbox_create failed: %+v", res.Content)
	}

	// file_read WITHOUT sandbox_url — must succeed by hitting the
	// cached URL.
	res := callTool(t, session, "file_read", map[string]any{"file": "/x"})
	if res.IsError {
		t.Fatalf("file_read errored without sandbox_url: %+v", res.Content)
	}
	text := contentText(t, res)
	// The mock echoes back the format it saw. Default MUST be numbered.
	if !strings.Contains(text, "format=numbered") {
		t.Errorf("expected default format=numbered in response: %s", text)
	}
}

// TestFileToolMissingSandboxURL exercises the friendly-error path: if
// the LLM forgets to call sandbox_create AND omits sandbox_url, the
// error message must tell it what to do.
func TestFileToolMissingSandboxURL(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	// Skip sandbox_create — cache is empty.
	res := callTool(t, session, "file_read", map[string]any{"file": "/x"})
	if !res.IsError {
		t.Fatalf("expected error result, got success: %s", contentText(t, res))
	}
}

func TestFileGrepRoundTrip(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	if res := callTool(t, session, "sandbox_create", map[string]any{"sandbox_id": "sb1"}); res.IsError {
		t.Fatalf("sandbox_create failed: %+v", res.Content)
	}

	res := callTool(t, session, "file_grep", map[string]any{
		"path":  "/workspace",
		"regex": "TODO",
	})
	if res.IsError {
		t.Fatalf("file_grep errored: %+v", res.Content)
	}
	text := contentText(t, res)
	// Mock echoes the request body in the matched text; we just check
	// the structured shape round-tripped through the tool layer.
	for _, want := range []string{`"matches"`, `"total_found"`, `TODO`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in file_grep response: %s", want, text)
		}
	}
}

func TestFileMultiEditRoundTrip(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	if res := callTool(t, session, "sandbox_create", map[string]any{"sandbox_id": "sb1"}); res.IsError {
		t.Fatalf("sandbox_create failed: %+v", res.Content)
	}

	res := callTool(t, session, "file_multi_edit", map[string]any{
		"file": "/x",
		"edits": []map[string]any{
			{"old_str": "echo", "new_str": "ECHO"},
			{"old_str": "TODO", "new_str": "DONE", "replace_all": true},
		},
	})
	if res.IsError {
		t.Fatalf("file_multi_edit errored: %+v", res.Content)
	}
	text := contentText(t, res)
	// Mock echoes the request body so we can confirm replace_all was
	// forwarded as a real bool (not its zero value omitted). The
	// echo is a nested JSON string, so the quotes are escaped — match
	// on the substring that survives the escape.
	if !strings.Contains(text, `replace_all\":true`) {
		t.Errorf("replace_all wasn't forwarded; response: %s", text)
	}
}

// TestFileMultiEditRequiresEdits is the cheap "called it wrong"
// guard — empty edits array should fail before we make an HTTP call.
func TestFileMultiEditRequiresEdits(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	if res := callTool(t, session, "sandbox_create", map[string]any{"sandbox_id": "sb1"}); res.IsError {
		t.Fatalf("sandbox_create failed: %+v", res.Content)
	}
	res := callTool(t, session, "file_multi_edit", map[string]any{
		"file":  "/x",
		"edits": []map[string]any{},
	})
	if !res.IsError {
		t.Fatalf("expected error on empty edits, got success: %s", contentText(t, res))
	}
}

func TestSandboxDeleteRoundTrip(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	res := callTool(t, session, "sandbox_delete", map[string]any{"sandbox_id": "sb1"})
	if res.IsError {
		t.Fatalf("IsError: %+v", res.Content)
	}
	if !strings.Contains(contentText(t, res), "sb1") {
		t.Errorf("delete result missing sandbox id: %s", contentText(t, res))
	}
}

// contentText pulls the first TextContent string out of a tool result.
func contentText(t *testing.T, res *sdkmcp.CallToolResult) string {
	t.Helper()
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatalf("no TextContent in result %+v", res)
	return ""
}
