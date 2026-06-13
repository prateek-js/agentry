package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The probe tools shell out to Python inside the sandbox; these tests
// fake the runtime's /v1/shell/exec endpoint so the Python "output" is
// canned. That isolates what we actually own on the Go side: building
// the request, parsing the JSON the probe prints, and attaching the
// next-action hint on failure.

// execReturning fakes the shell exec endpoint, returning `output` as the
// command's stdout. captured (if non-nil) receives the command string.
func execReturning(output string, captured *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			var body ExecRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			*captured = body.Command
		}
		_ = json.NewEncoder(w).Encode(shellResponse{
			Success: true,
			Data:    ExecResult{Output: output, ExitCode: 0, Status: "completed"},
		})
	}
}

func TestAppProbe_ParsesSuccess(t *testing.T) {
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/shell/exec": execReturning(
			`{"ok":true,"status_code":200,"time_ms":12,"content_type":"text/html","body_snippet":"<h1>hi</h1>"}`, nil),
	})
	defer srv.Close()

	c := NewClient(Config{})
	_, data, err := appProbe(c)(context.Background(), nil, appProbeArgs{SandboxURL: srv.URL, Port: 3000})
	if err != nil {
		t.Fatal(err)
	}
	m := data.(map[string]any)
	if ok, _ := m["ok"].(bool); !ok {
		t.Fatalf("ok should be true; got %+v", m)
	}
	if m["status_code"].(float64) != 200 {
		t.Errorf("status_code = %v; want 200", m["status_code"])
	}
	if _, hasHint := m["hint"]; hasHint {
		t.Error("a 200 should NOT carry a failure hint")
	}
}

func TestAppProbe_FailureAttachesHint(t *testing.T) {
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/shell/exec": execReturning(
			`{"ok":false,"time_ms":3,"error":"[Errno 111] Connection refused"}`, nil),
	})
	defer srv.Close()

	c := NewClient(Config{})
	_, data, err := appProbe(c)(context.Background(), nil, appProbeArgs{SandboxURL: srv.URL, Port: 8501, Path: "/health"})
	if err != nil {
		t.Fatal(err)
	}
	m := data.(map[string]any)
	hint, ok := m["hint"].(string)
	if !ok || hint == "" {
		t.Fatalf("a refused probe must carry a hint; got %+v", m)
	}
	// The hint should name the concrete next tools, with the port.
	for _, want := range []string{"project_list", "project_logs", "8501"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q: %s", want, hint)
		}
	}
}

func TestAppProbe_NormalizesPathAndMethod(t *testing.T) {
	var cmd string
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/shell/exec": execReturning(`{"ok":true,"status_code":204}`, &cmd),
	})
	defer srv.Close()

	c := NewClient(Config{})
	// Path without leading slash + lowercase method should be normalized
	// before they reach the (base64'd) Python. We can't read the encoded
	// args directly, but we CAN assert the command ran python3.
	_, _, err := appProbe(c)(context.Background(), nil, appProbeArgs{SandboxURL: srv.URL, Port: 3000, Path: "api/health", Method: "post"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "python3 -c") {
		t.Errorf("probe should run python3; got %q", cmd)
	}
}

func TestAppProbe_RequiresPort(t *testing.T) {
	c := NewClient(Config{})
	res, _, err := appProbe(c)(context.Background(), nil, appProbeArgs{SandboxURL: "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("missing port should be a tool error")
	}
}

func TestServiceProbe_ParsesReachable(t *testing.T) {
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/shell/exec": execReturning(
			`{"reachable":true,"host":"db.internal","port":5432,"scheme":"postgres","latency_ms":4}`, nil),
	})
	defer srv.Close()

	c := NewClient(Config{})
	_, data, err := serviceProbe(c)(context.Background(), nil, serviceProbeArgs{SandboxURL: srv.URL, EnvVar: "DATABASE_URL"})
	if err != nil {
		t.Fatal(err)
	}
	m := data.(map[string]any)
	if reachable, _ := m["reachable"].(bool); !reachable {
		t.Fatalf("reachable should be true; got %+v", m)
	}
	if _, hasHint := m["hint"]; hasHint {
		t.Error("a reachable service should NOT carry a hint")
	}
}

func TestServiceProbe_UnreachableAttachesHint(t *testing.T) {
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/shell/exec": execReturning(
			`{"reachable":false,"host":"db.internal","port":5432,"error":"timed out"}`, nil),
	})
	defer srv.Close()

	c := NewClient(Config{})
	_, data, err := serviceProbe(c)(context.Background(), nil, serviceProbeArgs{SandboxURL: srv.URL, EnvVar: "DATABASE_URL"})
	if err != nil {
		t.Fatal(err)
	}
	m := data.(map[string]any)
	if hint, _ := m["hint"].(string); hint == "" {
		t.Fatalf("an unreachable service must carry a hint; got %+v", m)
	}
}

func TestServiceProbe_RequiresTarget(t *testing.T) {
	c := NewClient(Config{})
	// Neither env_var nor host+port → tool error.
	res, _, err := serviceProbe(c)(context.Background(), nil, serviceProbeArgs{SandboxURL: "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("missing target should be a tool error")
	}
}

func TestRunPython_PicksLastJSONLine(t *testing.T) {
	// A stray warning line before the JSON must not break decoding.
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/shell/exec": execReturning(
			"WARNING: something noisy\n{\"ok\":true,\"status_code\":200}\n", nil),
	})
	defer srv.Close()

	c := NewClient(Config{})
	m, err := runPython(context.Background(), c, srv.URL, "print('x')", 5)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := m["ok"].(bool); !ok {
		t.Errorf("should have parsed the JSON line; got %+v", m)
	}
}

func TestRunPython_NonJSONIsError(t *testing.T) {
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/shell/exec": execReturning("Traceback (most recent call last): boom\n", nil),
	})
	defer srv.Close()

	c := NewClient(Config{})
	_, err := runPython(context.Background(), c, srv.URL, "print('x')", 5)
	if err == nil {
		t.Fatal("non-JSON output should surface as an error")
	}
	if !strings.Contains(err.Error(), "Traceback") {
		t.Errorf("error should include the raw output for debugging; got %v", err)
	}
}
