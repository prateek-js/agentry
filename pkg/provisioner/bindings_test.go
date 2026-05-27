package provisioner

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBindingCreate_StubMintAndFileWrite is the load-bearing test for
// the binding endpoint: it stands up a fake "runtime" (records every
// file_write the provisioner sends), pre-seeds a sandbox in the mock
// backend pointing at that runtime, then POSTs a binding for trino
// and asserts:
//
//   1. response carries the expected env var names
//   2. the runtime received POSTs for every env var, each to the
//      right path under /var/run/xdp/trino/
//   3. the values match the stub mint output
func TestBindingCreate_StubMintAndFileWrite(t *testing.T) {
	type written struct {
		File    string
		Content string
	}
	var (
		mu        []byte // no concurrency in this test; ignored
		writeChan = make(chan written, 16)
	)
	_ = mu

	// Fake runtime — only honours POST /v1/file/write and records it.
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/file/write" {
			http.Error(w, "not found", 404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			File    string `json:"file"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal(body, &msg)
		writeChan <- written{File: msg.File, Content: msg.Content}
		w.WriteHeader(200)
	}))
	defer runtime.Close()

	host, port := splitHostPort(t, runtime.URL)

	mock := NewMockBackend()
	mock.preSeed("sb1", host, port)

	p := NewWithKey(Config{
		Namespace: "test",
		NodeHost:  host,
	}, mock, "")
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	// Drive the bind.
	body, _ := json.Marshal(BindingRequest{Service: "trino"})
	resp, err := http.Post(srv.URL+"/api/sandboxes/sb1/bindings", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	var br BindingResponse
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"TRINO_URL": true, "TRINO_USER": true,
		"TRINO_PASSWORD": true, "TRINO_CATALOG": true,
	}
	got := map[string]bool{}
	for _, v := range br.EnvVars {
		got[v] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("response missing env var %q; got %v", k, br.EnvVars)
		}
	}

	// 4 env vars → 4 file_writes.
	seen := map[string]string{}
	for i := 0; i < 4; i++ {
		w := <-writeChan
		seen[w.File] = w.Content
	}
	for k := range want {
		path := "/var/run/xdp/trino/" + k
		if _, ok := seen[path]; !ok {
			t.Errorf("runtime never received write for %q; saw %v", path, keys(seen))
		}
	}
	// Spot-check values are non-empty stubs.
	if !strings.HasPrefix(seen["/var/run/xdp/trino/TRINO_USER"], "dev-") {
		t.Errorf("TRINO_USER stub looks wrong: %q", seen["/var/run/xdp/trino/TRINO_USER"])
	}
}

func TestBindingCreate_RejectsUnknownService(t *testing.T) {
	mock := NewMockBackend()
	mock.preSeed("sb1", "127.0.0.1", 1)
	p := NewWithKey(Config{Namespace: "test", NodeHost: "127.0.0.1"}, mock, "")
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	body, _ := json.Marshal(BindingRequest{Service: "no-such-service"})
	resp, err := http.Post(srv.URL+"/api/sandboxes/sb1/bindings", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (B110 not in catalog)", resp.StatusCode)
	}
	// Body should carry the structured error code.
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "B110") {
		t.Errorf("body should contain code B110: %s", raw)
	}
}

func TestBindingCreate_RejectsMissingService(t *testing.T) {
	mock := NewMockBackend()
	p := NewWithKey(Config{Namespace: "test"}, mock, "")
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/sandboxes/sb1/bindings", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "B001") {
		t.Errorf("body should carry B001: %s", raw)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
