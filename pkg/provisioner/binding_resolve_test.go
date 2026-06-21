package provisioner

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// handleBindingResolve is the privileged endpoint the control plane
// calls when the user ticks "inherit env from sandbox" in the Deploy
// form. The dashboard's mental model is "everything I staged in this
// sandbox will be there for my deploy". For that to be TRUE the
// resolver has to include three flavours:
//
//   1. Service bindings (mongodb, redis, …) — already covered.
//   2. Sandbox-staged secrets the user set via `agentry env set` or
//      the Secrets panel — these used to silently vanish at deploy
//      time, the exact "I set DATABASE_PASSWORD but my deploy crashes"
//      footgun this test exists to lock down.
//   3. A service binding winning over a same-named secret. Bindings
//      are managed; secrets are user-managed. Letting a stale
//      DATABASE_URL secret silently shadow a freshly-bound mongo
//      would surface as mystery 500s, much harder to debug than
//      "your binding wins".
//
// We stand up a fake runtime that satisfies file_read for the
// lockfile + each binding/secret value file. Asserting on the
// resolver's response gives us the exact "what does the dashboard
// see when the user clicks Deploy" wire.

func TestBindingResolve_IncludesSandboxSecrets(t *testing.T) {
	// Files the resolver will request. We hard-code the responses; a
	// real sandbox would serve these from /var/run/agentry/... .
	files := map[string]string{
		LockfilePath: `{
			"version":1,
			"bindings":[
				{"service":"mongodb","env_vars":["DATABASE_URL","MONGODB_URI"]}
			],
			"secrets":["SESSION_KEY","DATABASE_URL"]
		}`,
		"/var/run/agentry/mongodb/DATABASE_URL": "mongodb://prod-host/db",
		"/var/run/agentry/mongodb/MONGODB_URI":  "mongodb://prod-host/db",
		"/var/run/agentry/secrets/SESSION_KEY":  "s3cret-session-bytes",
		// DATABASE_URL also lives as a sandbox secret; the binding
		// version above MUST win — see test rationale above.
		"/var/run/agentry/secrets/DATABASE_URL": "stale-localhost-fallback",
	}

	var mu sync.Mutex
	var readPaths []string
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/file/read" {
			http.Error(w, "not found", 404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			File string `json:"file"`
		}
		_ = json.Unmarshal(body, &msg)
		mu.Lock()
		readPaths = append(readPaths, msg.File)
		mu.Unlock()
		val, ok := files[msg.File]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"content": val},
		})
	}))
	defer runtime.Close()

	host, port := splitHostPort(t, runtime.URL)
	mock := NewMockBackend()
	mock.preSeed("sb1", host, port)
	p := NewWithKey(Config{Namespace: "test", NodeHost: host}, mock, "")
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/sandboxes/sb1/bindings/env")
	if err != nil {
		t.Fatalf("GET bindings/env: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var got struct {
		Env     map[string]string `json:"env"`
		Sources map[string]string `json:"sources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// 1) Service binding values are present.
	if got.Env["DATABASE_URL"] != "mongodb://prod-host/db" {
		t.Errorf("DATABASE_URL = %q; want the binding value (binding wins over secret)", got.Env["DATABASE_URL"])
	}
	if got.Env["MONGODB_URI"] != "mongodb://prod-host/db" {
		t.Errorf("MONGODB_URI missing or wrong: %q", got.Env["MONGODB_URI"])
	}

	// 2) Sandbox-staged secrets flow through.
	if got.Env["SESSION_KEY"] != "s3cret-session-bytes" {
		t.Errorf("SESSION_KEY secret missing — would silently vanish at deploy: %q", got.Env["SESSION_KEY"])
	}

	// 3) Sources carry the right provenance so the dashboard can
	// render bindings and secrets distinctly.
	if got.Sources["DATABASE_URL"] != "mongodb" {
		t.Errorf("DATABASE_URL source = %q; want mongodb (binding)", got.Sources["DATABASE_URL"])
	}
	if got.Sources["SESSION_KEY"] != "secret" {
		t.Errorf("SESSION_KEY source = %q; want \"secret\"", got.Sources["SESSION_KEY"])
	}

	// 4) The DATABASE_URL secret file is NOT read once the binding
	// has already taken the key — saves a network round-trip in the
	// hot path and underwrites the "binding wins" rule. (We never
	// log secret contents, but a leaked log line on the read code
	// path would otherwise expose the stale value too.)
	mu.Lock()
	defer mu.Unlock()
	for _, p := range readPaths {
		if p == "/var/run/agentry/secrets/DATABASE_URL" {
			t.Errorf("secret file read after binding already populated DATABASE_URL: %v", readPaths)
		}
	}

	// Spot-check the only paths we expect to have been read.
	wantRead := map[string]bool{
		LockfilePath:                            true,
		"/var/run/agentry/mongodb/DATABASE_URL": true,
		"/var/run/agentry/mongodb/MONGODB_URI":  true,
		"/var/run/agentry/secrets/SESSION_KEY":  true,
	}
	for _, p := range readPaths {
		if !wantRead[p] {
			t.Errorf("unexpected file read on resolver hot path: %q", p)
		}
	}
}
