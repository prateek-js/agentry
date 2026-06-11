package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// project_logs gained grep + tail_lines filtering. These pin the
// filtering math (case-insensitive grep, tail after grep) and the bad-
// regex error path, faking the runtime's project/logs envelope.

func logsBackend(t *testing.T, lines []string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"message": "logs",
			"data":    map[string]any{"name": "app", "lines": lines},
		})
	}
}

func callProjectLogs(t *testing.T, c *Client, a projectLogsArgs) map[string]any {
	t.Helper()
	_, data, err := projectLogs(c)(context.Background(), nil, a)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := data.(map[string]any)
	if !ok {
		t.Fatalf("expected map result; got %T", data)
	}
	return m
}

func TestProjectLogs_GrepFiltersCaseInsensitive(t *testing.T) {
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"GET /v1/project/logs": logsBackend(t, []string{
			"INFO server started",
			"ERROR cannot connect to db",
			"info handling request",
			"Error: second failure",
			"INFO ok",
		}),
	})
	defer srv.Close()

	c := NewClient(Config{})
	m := callProjectLogs(t, c, projectLogsArgs{SandboxURL: srv.URL, Name: "app", Grep: "error"})
	lines := m["lines"].([]string)
	if len(lines) != 2 {
		t.Fatalf("grep 'error' should match 2 lines (case-insensitive); got %d: %v", len(lines), lines)
	}
	if m["returned_lines"].(int) != 2 {
		t.Errorf("returned_lines = %v; want 2", m["returned_lines"])
	}
}

func TestProjectLogs_TailLimitsAfterGrep(t *testing.T) {
	all := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		all = append(all, "ERROR line")
	}
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"GET /v1/project/logs": logsBackend(t, all),
	})
	defer srv.Close()

	c := NewClient(Config{})
	m := callProjectLogs(t, c, projectLogsArgs{SandboxURL: srv.URL, Name: "app", Grep: "error", TailLines: 5})
	if got := len(m["lines"].([]string)); got != 5 {
		t.Errorf("tail_lines=5 should cap to 5; got %d", got)
	}
}

func TestProjectLogs_TailWithoutGrep(t *testing.T) {
	all := []string{"a", "b", "c", "d", "e"}
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"GET /v1/project/logs": logsBackend(t, all),
	})
	defer srv.Close()

	c := NewClient(Config{})
	m := callProjectLogs(t, c, projectLogsArgs{SandboxURL: srv.URL, Name: "app", TailLines: 2})
	lines := m["lines"].([]string)
	if len(lines) != 2 || lines[0] != "d" || lines[1] != "e" {
		t.Errorf("tail 2 should be the LAST two lines; got %v", lines)
	}
}

func TestProjectLogs_BadRegexErrors(t *testing.T) {
	c := NewClient(Config{})
	res, _, err := projectLogs(c)(context.Background(), nil, projectLogsArgs{
		SandboxURL: "http://x", Name: "app", Grep: "(unclosed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("an invalid regex should be a tool error, not a silent passthrough")
	}
}

func TestExtractLogLines_Shapes(t *testing.T) {
	// Nested under data (the real envelope).
	nested := map[string]any{"data": map[string]any{"lines": []any{"x", "y"}}}
	if got := extractLogLines(nested); len(got) != 2 || got[0] != "x" {
		t.Errorf("nested extract failed: %v", got)
	}
	// Top-level fallback.
	top := map[string]any{"lines": []any{"z"}}
	if got := extractLogLines(top); len(got) != 1 || got[0] != "z" {
		t.Errorf("top-level extract failed: %v", got)
	}
	// Missing → empty, never nil-panic.
	if got := extractLogLines(map[string]any{}); got == nil || len(got) != 0 {
		t.Errorf("missing lines should yield empty slice; got %v", got)
	}
}
