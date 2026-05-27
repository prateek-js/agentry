package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeBackend returns an httptest.Server that records requests and serves
// canned responses. routes maps "METHOD path" -> handler.
func fakeBackend(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for k, h := range routes {
		parts := strings.SplitN(k, " ", 2)
		if len(parts) != 2 {
			t.Fatalf("bad route key %q", k)
		}
		method, path := parts[0], parts[1]
		hh := h
		// Go 1.22+ pattern syntax supports method-prefixed registration.
		mux.HandleFunc(method+" "+path, func(w http.ResponseWriter, r *http.Request) {
			hh(w, r)
		})
	}
	return httptest.NewServer(mux)
}

func TestClientCreateSandboxSendsExpectedBody(t *testing.T) {
	var gotBody string
	var gotAuth string
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /api/sandboxes": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			gotAuth = r.Header.Get("X-Sandbox-API-Key")
			_ = json.NewEncoder(w).Encode(SandboxInfo{
				SandboxID:  "s1",
				SandboxURL: "http://sb/s1",
				Status:     "Running",
				ExpiresAt:  "2026-05-13T00:00:00Z",
			})
		},
	})
	defer srv.Close()

	c := NewClient(Config{ProvisionerURL: srv.URL, APIKey: "secret"})
	info, err := c.CreateSandbox(context.Background(), CreateRequest{
		SandboxID:    "s1",
		TTLSeconds:   3600,
		RuntimeClass: "kata",
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.SandboxID != "s1" || info.Status != "Running" {
		t.Errorf("info = %+v", info)
	}
	if gotAuth != "secret" {
		t.Errorf("auth header = %q; want secret", gotAuth)
	}
	if !strings.Contains(gotBody, `"sandbox_id":"s1"`) ||
		!strings.Contains(gotBody, `"ttl_seconds":3600`) ||
		!strings.Contains(gotBody, `"runtime_class":"kata"`) {
		t.Errorf("body = %s", gotBody)
	}
}

func TestClientListSandboxes(t *testing.T) {
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"GET /api/sandboxes": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 2,
				"sandboxes": []SandboxInfo{
					{SandboxID: "a", SandboxURL: "u/a", Status: "Running"},
					{SandboxID: "b", SandboxURL: "u/b", Status: "Pending"},
				},
			})
		},
	})
	defer srv.Close()

	c := NewClient(Config{ProvisionerURL: srv.URL})
	got, err := c.ListSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SandboxID != "a" || got[1].SandboxID != "b" {
		t.Errorf("list = %+v", got)
	}
}

func TestClientDeleteSandbox(t *testing.T) {
	hit := false
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"DELETE /api/sandboxes/abc": func(w http.ResponseWriter, _ *http.Request) {
			hit = true
			_, _ = w.Write([]byte(`{"ok":true}`))
		},
	})
	defer srv.Close()

	c := NewClient(Config{ProvisionerURL: srv.URL})
	if err := c.DeleteSandbox(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("DELETE handler not invoked")
	}
}

func TestClientPropagatesServerError(t *testing.T) {
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"GET /api/sandboxes/nope": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"message":"sandbox 'nope' not found"}`))
		},
	})
	defer srv.Close()

	c := NewClient(Config{ProvisionerURL: srv.URL})
	_, err := c.GetSandbox(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q; want 'not found' substring", err)
	}
}

func TestClientExec(t *testing.T) {
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/shell/exec": func(w http.ResponseWriter, r *http.Request) {
			var got ExecRequest
			_ = json.NewDecoder(r.Body).Decode(&got)
			if got.Command != "echo hi" || got.ID != "sess1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(shellResponse{
				Success: true,
				Data: ExecResult{
					SessionID: "sess1", Status: "completed",
					Output: "hi\n", ExitCode: 0,
				},
			})
		},
	})
	defer srv.Close()

	c := NewClient(Config{})
	got, err := c.Exec(context.Background(), srv.URL, ExecRequest{
		Command: "echo hi", ID: "sess1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Output != "hi\n" || got.ExitCode != 0 || got.SessionID != "sess1" {
		t.Errorf("result = %+v", got)
	}
}

func TestClientReadFile(t *testing.T) {
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/file/read": func(w http.ResponseWriter, r *http.Request) {
			var got FileReadRequest
			_ = json.NewDecoder(r.Body).Decode(&got)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"file":    got.File,
					"content": "line1\nline2\n",
				},
			})
		},
	})
	defer srv.Close()

	c := NewClient(Config{})
	out, err := c.ReadFile(context.Background(), srv.URL, FileReadRequest{File: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if out["data"] == nil {
		t.Errorf("missing data: %+v", out)
	}
}
