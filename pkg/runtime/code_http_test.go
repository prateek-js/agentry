//go:build !windows
// +build !windows

package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// requireIPyKernel is the same gate the jupyter integration tests use:
// skip when python3/ipykernel isn't installed on the host.
func requireIPyKernel(t *testing.T) {
	t.Helper()
	cmd := exec.Command("python3", "-c", "import ipykernel")
	if err := cmd.Run(); err != nil {
		t.Skipf("python3/ipykernel not available: %v", err)
	}
}

// sseEvent is one parsed Server-Sent Event frame.
type sseEvent struct {
	Name string
	Data json.RawMessage
}

// streamSSE reads SSE frames from r until ctx fires or the body closes,
// pushing each event into the returned channel.
func streamSSE(t *testing.T, r io.ReadCloser) chan sseEvent {
	t.Helper()
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		defer r.Close()
		br := bufio.NewReader(r)
		var ev sseEvent
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				if ev.Name != "" || len(ev.Data) > 0 {
					out <- ev
				}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if ev.Name != "" || len(ev.Data) > 0 {
					out <- ev
					ev = sseEvent{}
				}
			case strings.HasPrefix(line, "event: "):
				ev.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return out
}

// postJSONResp mirrors postJSON but returns the raw response (so the
// caller can drive a streaming body).
func postJSONResp(t *testing.T, ts *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// createContext spawns a kernel via HTTP and returns its id.
func createContext(t *testing.T, ts *httptest.Server, language string) string {
	t.Helper()
	resp := postJSONResp(t, ts, "/v1/code/contexts", map[string]any{"language": language})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create context: status=%d body=%s", resp.StatusCode, b)
	}
	var env struct {
		Data struct {
			ContextID string `json:"context_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Data.ContextID == "" {
		t.Fatal("create returned empty context_id")
	}
	return env.Data.ContextID
}

// drainCodeExec runs code and collects every SSE event until "done".
// Returns events in order and the merged stdout text.
func drainCodeExec(t *testing.T, ts *httptest.Server, ctxID, code string) ([]sseEvent, string) {
	t.Helper()
	resp := postJSONResp(t, ts, "/v1/code/contexts/"+ctxID+"/exec",
		map[string]any{"code": code})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("exec: status=%d body=%s", resp.StatusCode, b)
	}
	events := streamSSE(t, resp.Body)

	deadline := time.After(15 * time.Second)
	var all []sseEvent
	var stdout strings.Builder
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out; events so far=%+v", all)
		case ev, ok := <-events:
			if !ok {
				return all, stdout.String()
			}
			all = append(all, ev)
			if ev.Name == "stream" {
				var c struct {
					Name string `json:"name"`
					Text string `json:"text"`
				}
				_ = json.Unmarshal(ev.Data, &c)
				if c.Name == "stdout" {
					stdout.WriteString(c.Text)
				}
			}
			if ev.Name == "done" {
				return all, stdout.String()
			}
		}
	}
}

func TestCodeExecPrintsHello(t *testing.T) {
	requireIPyKernel(t)
	ts := newTestServer(t, "")

	ctxID := createContext(t, ts, "python")
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/code/contexts/"+ctxID, nil)
		http.DefaultClient.Do(req)
	})

	events, stdout := drainCodeExec(t, ts, ctxID, `print("hello-from-jupyter")`)
	if !strings.Contains(stdout, "hello-from-jupyter") {
		t.Errorf("stdout = %q", stdout)
	}
	// Must include the shell reply event.
	found := false
	for _, e := range events {
		if e.Name == "reply" {
			var r struct{ Status string }
			_ = json.Unmarshal(e.Data, &r)
			if r.Status == "ok" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("never saw reply event with status=ok")
	}
}

func TestCodeExecExecuteResult(t *testing.T) {
	requireIPyKernel(t)
	ts := newTestServer(t, "")
	ctxID := createContext(t, ts, "python")

	events, _ := drainCodeExec(t, ts, ctxID, "21 * 2")
	for _, e := range events {
		if e.Name == "result" {
			var r struct {
				Data map[string]any `json:"data"`
			}
			_ = json.Unmarshal(e.Data, &r)
			if v, ok := r.Data["text/plain"].(string); ok && v == "42" {
				return
			}
		}
	}
	t.Errorf("no execute_result with value 42; events=%+v", events)
}

func TestCodeExecExceptionEmitsError(t *testing.T) {
	requireIPyKernel(t)
	ts := newTestServer(t, "")
	ctxID := createContext(t, ts, "python")

	events, _ := drainCodeExec(t, ts, ctxID, "raise ValueError('boom')")
	sawError := false
	for _, e := range events {
		if e.Name == "error" {
			var c struct{ Ename string }
			_ = json.Unmarshal(e.Data, &c)
			if c.Ename == "ValueError" {
				sawError = true
			}
		}
	}
	if !sawError {
		t.Errorf("no error event with ValueError; events=%+v", events)
	}
}

func TestCodeContextStatePersists(t *testing.T) {
	requireIPyKernel(t)
	ts := newTestServer(t, "")
	ctxID := createContext(t, ts, "python")

	_, _ = drainCodeExec(t, ts, ctxID, "x = 7")
	_, stdout := drainCodeExec(t, ts, ctxID, "print(x * 6)")
	if !strings.Contains(stdout, "42") {
		t.Errorf("state did not persist; stdout=%q", stdout)
	}
}

func TestCodeContextLifecycle(t *testing.T) {
	requireIPyKernel(t)
	ts := newTestServer(t, "")

	ctxID := createContext(t, ts, "python")
	// LIST should include it.
	resp, _ := http.Get(ts.URL + "/v1/code/contexts")
	var env struct {
		Data struct {
			Contexts []struct {
				ID string `json:"id"`
			} `json:"contexts"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	found := false
	for _, c := range env.Data.Contexts {
		if c.ID == ctxID {
			found = true
		}
	}
	if !found {
		t.Errorf("created context not in list")
	}

	// DELETE.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/code/contexts/"+ctxID, nil)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Errorf("delete status = %d", r2.StatusCode)
	}

	// Subsequent exec must 404.
	resp3 := postJSONResp(t, ts, "/v1/code/contexts/"+ctxID+"/exec",
		map[string]any{"code": "1"})
	defer resp3.Body.Close()
	// The shutdown goroutine cleans the map asynchronously; we poll up
	// to ~1s for 404.
	if resp3.StatusCode == 200 {
		time.Sleep(300 * time.Millisecond)
		resp3.Body.Close()
		resp3 = postJSONResp(t, ts, "/v1/code/contexts/"+ctxID+"/exec",
			map[string]any{"code": "1"})
	}
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("post-delete exec = %d; want 404", resp3.StatusCode)
	}
}

func TestCodeCreateUnknownLanguage(t *testing.T) {
	ts := newTestServer(t, "")
	resp := postJSONResp(t, ts, "/v1/code/contexts",
		map[string]any{"language": "fortran"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown language: status = %d; want 400", resp.StatusCode)
	}
}

func TestCodeExecMissingContext(t *testing.T) {
	ts := newTestServer(t, "")
	resp := postJSONResp(t, ts, "/v1/code/contexts/nope/exec",
		map[string]any{"code": "1"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// Compile-time references: silence unused import warnings if a future
// edit prunes them from the body of the tests.
var _ = context.Background
