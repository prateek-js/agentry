package mcp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// sseFrame helper writes one SSE event in the canonical
// "event: NAME\ndata: JSON\n\n" form.
func sseFrame(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func TestExecCodeAggregatesSSE(t *testing.T) {
	// A fake runtime that returns the full event sequence ipykernel
	// would produce for `print("hi"); 1+1`.
	stream := strings.Join([]string{
		sseFrame("status", `{"state":"busy"}`),
		sseFrame("stream", `{"name":"stdout","text":"hi\n"}`),
		sseFrame("result", `{"data":{"text/plain":"2"},"metadata":{},"execution_count":3}`),
		sseFrame("status", `{"state":"idle"}`),
		sseFrame("reply", `{"status":"ok","execution_count":3}`),
		sseFrame("done", `{"dropped":0}`),
	}, "")

	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/code/contexts/abc/exec": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(stream))
		},
	})
	defer srv.Close()

	c := NewClient(Config{})
	res, err := c.ExecCode(context.Background(), srv.URL, "abc", CodeExecRequest{
		Code: `print("hi")\n1+1`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Errorf("status = %q; want ok", res.Status)
	}
	if res.ExecutionCount != 3 {
		t.Errorf("execution_count = %d; want 3", res.ExecutionCount)
	}
	if res.Stdout != "hi\n" {
		t.Errorf("stdout = %q; want %q", res.Stdout, "hi\n")
	}
	if res.Result == nil || res.Result["text/plain"] != "2" {
		t.Errorf("result = %+v; want text/plain=2", res.Result)
	}
}

func TestExecCodeCapturesError(t *testing.T) {
	stream := strings.Join([]string{
		sseFrame("status", `{"state":"busy"}`),
		sseFrame("error", `{"ename":"ValueError","evalue":"boom","traceback":["Traceback…","ValueError: boom"]}`),
		sseFrame("status", `{"state":"idle"}`),
		sseFrame("reply", `{"status":"error","execution_count":1}`),
		sseFrame("done", `{}`),
	}, "")

	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/code/contexts/ctx/exec": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(stream))
		},
	})
	defer srv.Close()

	c := NewClient(Config{})
	res, err := c.ExecCode(context.Background(), srv.URL, "ctx", CodeExecRequest{Code: "raise ValueError('boom')"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "error" {
		t.Errorf("status = %q; want error", res.Status)
	}
	if res.Error == nil || res.Error.Ename != "ValueError" {
		t.Errorf("error = %+v; want ValueError", res.Error)
	}
	if len(res.Error.Traceback) != 2 {
		t.Errorf("traceback len = %d; want 2", len(res.Error.Traceback))
	}
}

func TestExecCodeCapturesDisplayData(t *testing.T) {
	stream := strings.Join([]string{
		sseFrame("status", `{"state":"busy"}`),
		sseFrame("display", `{"data":{"text/plain":"<Figure>","image/png":"base64bytes"},"metadata":{}}`),
		sseFrame("status", `{"state":"idle"}`),
		sseFrame("reply", `{"status":"ok"}`),
		sseFrame("done", `{}`),
	}, "")

	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/code/contexts/x/exec": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(stream))
		},
	})
	defer srv.Close()

	c := NewClient(Config{})
	res, err := c.ExecCode(context.Background(), srv.URL, "x", CodeExecRequest{Code: "plt.plot([1,2,3])"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Displays) != 1 {
		t.Fatalf("displays = %d; want 1", len(res.Displays))
	}
	data, _ := res.Displays[0]["data"].(map[string]any)
	if data["image/png"] != "base64bytes" {
		t.Errorf("display image missing: %+v", res.Displays[0])
	}
}

func TestExecCodeHandlesIncompleteStream(t *testing.T) {
	// Stream that ends abruptly — no reply, no done.
	stream := sseFrame("stream", `{"name":"stdout","text":"partial output"}`)

	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/code/contexts/y/exec": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(stream))
		},
	})
	defer srv.Close()

	c := NewClient(Config{})
	res, err := c.ExecCode(context.Background(), srv.URL, "y", CodeExecRequest{Code: "x=1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "incomplete" {
		t.Errorf("status = %q; want incomplete", res.Status)
	}
	if res.Stdout != "partial output" {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestExecCodeMergesStderr(t *testing.T) {
	stream := strings.Join([]string{
		sseFrame("stream", `{"name":"stdout","text":"out1\n"}`),
		sseFrame("stream", `{"name":"stderr","text":"warn1\n"}`),
		sseFrame("stream", `{"name":"stdout","text":"out2\n"}`),
		sseFrame("stream", `{"name":"stderr","text":"warn2\n"}`),
		sseFrame("reply", `{"status":"ok"}`),
		sseFrame("done", `{}`),
	}, "")

	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/code/contexts/z/exec": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(stream))
		},
	})
	defer srv.Close()

	c := NewClient(Config{})
	res, _ := c.ExecCode(context.Background(), srv.URL, "z", CodeExecRequest{Code: "x"})
	if res.Stdout != "out1\nout2\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.Stderr != "warn1\nwarn2\n" {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestExecCodePropagatesHTTPErrors(t *testing.T) {
	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/code/contexts/missing/exec": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"message":"context not found"}`))
		},
	})
	defer srv.Close()

	c := NewClient(Config{})
	_, err := c.ExecCode(context.Background(), srv.URL, "missing", CodeExecRequest{Code: "1"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "context not found") {
		t.Errorf("error = %q", err)
	}
}

func TestExecCodeWithLargeTraceback(t *testing.T) {
	// Traceback bigger than the default bufio.Scanner line cap (64 KiB)
	// to confirm our enlarged buffer handles it.
	huge := strings.Repeat("frame ", 20_000) // ~120 KB
	tb := fmt.Sprintf(`["%s"]`, huge)
	stream := strings.Join([]string{
		sseFrame("error", `{"ename":"X","evalue":"big","traceback":`+tb+`}`),
		sseFrame("reply", `{"status":"error"}`),
		sseFrame("done", `{}`),
	}, "")

	srv := fakeBackend(t, map[string]http.HandlerFunc{
		"POST /v1/code/contexts/big/exec": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(stream))
		},
	})
	defer srv.Close()

	c := NewClient(Config{})
	res, err := c.ExecCode(context.Background(), srv.URL, "big", CodeExecRequest{Code: "raise"})
	if err != nil {
		t.Fatalf("oversized traceback should still parse: %v", err)
	}
	if res.Error == nil || res.Error.Ename != "X" {
		t.Errorf("error not parsed: %+v", res.Error)
	}
}
