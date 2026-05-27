package runtime

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// envelope mirrors models.Response without importing it (avoid cycles).
type envelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func postJSON(t *testing.T, ts *httptest.Server, path string, body any) *http.Response {
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

func decode(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	defer resp.Body.Close()
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.Success {
		t.Fatalf("status=%d msg=%q", resp.StatusCode, env.Message)
	}
	if into != nil {
		if err := json.Unmarshal(env.Data, into); err != nil {
			t.Fatalf("decode data: %v (raw=%s)", err, env.Data)
		}
	}
}

func TestBgEndToEnd(t *testing.T) {
	ts := newTestServer(t, "")

	// Start a background command that writes a few lines.
	resp := postJSON(t, ts, "/v1/shell/background", map[string]any{
		"command": "for i in 1 2 3; do echo line$i; done",
	})
	var start struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	decode(t, resp, &start)
	if start.ID == "" || start.Status != "running" {
		t.Fatalf("start = %+v", start)
	}

	// Poll until completed.
	var final struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get(ts.URL + "/v1/shell/background/" + start.ID)
		if err != nil {
			t.Fatal(err)
		}
		decode(t, r, &final)
		if final.Status != "running" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if final.Status != "completed" || final.ExitCode != 0 {
		t.Fatalf("final = %+v", final)
	}

	// Pull all logs (cursor=0).
	r, err := http.Get(ts.URL + "/v1/shell/background/" + start.ID + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	var logs struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Cursor   int64  `json:"cursor"`
	}
	decode(t, r, &logs)
	if logs.Encoding != "utf-8" {
		t.Errorf("encoding = %s; want utf-8", logs.Encoding)
	}
	for _, s := range []string{"line1", "line2", "line3"} {
		if !strings.Contains(logs.Content, s) {
			t.Errorf("missing %q in transcript: %q", s, logs.Content)
		}
	}

	// Tail with the cursor should yield empty data (caught up).
	r2, err := http.Get(ts.URL + "/v1/shell/background/" + start.ID + "/logs?cursor=" + iToA(logs.Cursor))
	if err != nil {
		t.Fatal(err)
	}
	var tail struct {
		Content string `json:"content"`
		Cursor  int64  `json:"cursor"`
	}
	decode(t, r2, &tail)
	if tail.Content != "" || tail.Cursor != logs.Cursor {
		t.Errorf("tail = %+v; want empty content + same cursor", tail)
	}

	// Forget — should remove the record.
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/shell/background/"+start.ID, nil)
	r3, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != 200 {
		t.Fatalf("delete status = %d", r3.StatusCode)
	}

	// Subsequent status fetch should 404.
	r4, err := http.Get(ts.URL + "/v1/shell/background/" + start.ID)
	if err != nil {
		t.Fatal(err)
	}
	r4.Body.Close()
	if r4.StatusCode != 404 {
		t.Errorf("status after forget = %d; want 404", r4.StatusCode)
	}
}

func TestBgInterruptHTTP(t *testing.T) {
	ts := newTestServer(t, "")

	resp := postJSON(t, ts, "/v1/shell/background", map[string]any{
		"command": "sleep 30",
	})
	var start struct{ ID string }
	decode(t, resp, &start)

	r, err := http.Post(ts.URL+"/v1/shell/background/"+start.ID+"/interrupt",
		"application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s, err := http.Get(ts.URL + "/v1/shell/background/" + start.ID)
		if err != nil {
			t.Fatal(err)
		}
		var st struct{ Status string }
		decode(t, s, &st)
		if st.Status == "interrupted" {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("command did not transition to interrupted")
}

func TestBgValidationErrors(t *testing.T) {
	ts := newTestServer(t, "")

	r := postJSON(t, ts, "/v1/shell/background", map[string]any{})
	defer r.Body.Close()
	if r.StatusCode != 400 {
		t.Errorf("missing command: status = %d", r.StatusCode)
	}

	r2, err := http.Get(ts.URL + "/v1/shell/background/nope/logs?cursor=-1")
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 400 {
		t.Errorf("negative cursor: status = %d", r2.StatusCode)
	}

	r3, err := http.Get(ts.URL + "/v1/shell/background/nope/logs")
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != 404 {
		t.Errorf("unknown id: status = %d", r3.StatusCode)
	}
}

// iToA is here so we don't pull in strconv just for an int64 → string.
func iToA(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
