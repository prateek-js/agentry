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

	backend := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /api/sandboxes": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(SandboxInfo{
				SandboxID: "sb1", SandboxURL: "http://sb/sb1", Status: "Running",
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
		"sandbox_create", "sandbox_list", "sandbox_delete", "agentry_auth_setup",
		// Catalog + bindings + secrets (build/deploy MCP tools were
		// removed; deploy lives in the dashboard).
		"service_list", "service_bind", "secret_set", "secret_list",
		// Shell
		"command_run", "command_start", "command_logs", "command_interrupt",
		// Files
		"file_read", "file_write", "file_list", "file_search", "file_replace",
		// Ports
		"port_wait",
		// Project manager
		"project_start", "project_stop", "project_start_all", "project_list", "project_logs",
		// Code interpreter (Jupyter)
		"code_exec", "code_close",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing tool %q", want)
		}
	}
	if len(got) != 25 {
		t.Errorf("tool count = %d; want 25 (20 base + service_list/bind + secret_set/list + agentry_auth_setup)", len(got))
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

func TestSandboxCreateMissingIDIsErrorResult(t *testing.T) {
	session, _ := startServerWithMockBackend(t)
	res := callTool(t, session, "sandbox_create", map[string]any{})
	if !res.IsError {
		t.Fatalf("missing sandbox_id should produce IsError result, got %+v", res)
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
