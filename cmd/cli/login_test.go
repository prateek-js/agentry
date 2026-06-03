package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive cmdLogin in-process: a fake "dashboard" goroutine
// substitutes for the browser. It listens for the openBrowser call by
// reading the URL we'd hand to the OS, then POSTs back to the local
// listener the same way the real /cli-login page does.

func loginEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENTRY_CONFIG", filepath.Join(dir, "agentry.json"))
	return dir
}

// runLoginInBackground starts cmdLogin in a goroutine and returns the
// loginURL written to stderr so the test can act as the "browser".
// We use os.Pipe to capture stderr without colliding with the package-
// level test runner output.
func runLoginInBackground(t *testing.T, args []string) (loginURL string, exitCh chan int) {
	t.Helper()
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = wPipe
	t.Cleanup(func() { os.Stderr = origStderr })

	exitCh = make(chan int, 1)
	go func() {
		exitCh <- cmdLogin(args)
		_ = wPipe.Close()
	}()

	// cmdLogin prints two lines before the URL line. Read until we
	// find the http://… token.
	buf := make([]byte, 4096)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = rPipe.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, _ := rPipe.Read(buf)
		if n == 0 {
			continue
		}
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "http") {
				return line, exitCh
			}
		}
	}
	t.Fatal("never saw login URL on stderr")
	return "", nil
}

func TestLogin_HappyPath(t *testing.T) {
	dir := loginEnv(t)
	defer os.Setenv("AGENTRY_APP_URL", "")
	t.Setenv("AGENTRY_APP_URL", "http://app.test.invalid")

	loginURL, exitCh := runLoginInBackground(t, nil)

	// Parse state + callback out of the URL the CLI handed us.
	parsed, err := url.Parse(loginURL)
	if err != nil {
		t.Fatal(err)
	}
	state := parsed.Query().Get("state")
	callback := parsed.Query().Get("callback")
	if state == "" || callback == "" {
		t.Fatalf("missing state/callback in %s", loginURL)
	}
	if !strings.HasPrefix(callback, "http://127.0.0.1:") {
		t.Errorf("callback %q not on loopback", callback)
	}

	// Pretend to be the dashboard: POST the token + state back.
	body, _ := json.Marshal(map[string]string{
		"token":      "pat_tok_smoke_aaa",
		"app_url":    "http://app.test.invalid",
		"org_name":   "Test Org",
		"user_email": "alice@example.com",
		"state":      state,
	})
	req, _ := http.NewRequest("POST", callback, bytes.NewReader(body))
	// Set the Origin the CORS check expects.
	req.Header.Set("Origin", "http://app.test.invalid")
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 2 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("POST callback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d", resp.StatusCode)
	}

	select {
	case code := <-exitCh:
		if code != 0 {
			t.Errorf("cmdLogin exit code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cmdLogin did not exit after a successful callback")
	}

	// Config should now carry the PAT + org metadata.
	raw, err := os.ReadFile(filepath.Join(dir, "agentry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.APIToken != "pat_tok_smoke_aaa" {
		t.Errorf("APIToken = %q", got.APIToken)
	}
	if got.Org != "Test Org" || got.UserEmail != "alice@example.com" {
		t.Errorf("metadata = %+v", got)
	}
	if got.DeviceID == "" {
		t.Errorf("DeviceID was not populated")
	}
}

func TestLogin_StateMismatchRejected(t *testing.T) {
	_ = loginEnv(t)
	t.Setenv("AGENTRY_APP_URL", "http://app.test.invalid")

	loginURL, exitCh := runLoginInBackground(t, nil)
	parsed, _ := url.Parse(loginURL)
	callback := parsed.Query().Get("callback")

	// Wrong state. Server must 403; cmdLogin must NOT exit 0.
	body, _ := json.Marshal(map[string]string{
		"token": "pat_tok_smoke_aaa", "app_url": "http://app.test.invalid",
		"state": "totally-not-the-real-state",
	})
	req, _ := http.NewRequest("POST", callback, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 2 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("state mismatch: server returned %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()

	select {
	case code := <-exitCh:
		if code == 0 {
			t.Error("cmdLogin returned success after state mismatch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cmdLogin did not exit after rejecting state mismatch")
	}
}

func TestLogin_TimeoutWithoutCallback(t *testing.T) {
	_ = loginEnv(t)
	t.Setenv("AGENTRY_APP_URL", "http://app.test.invalid")

	// Manually call cmdLogin with a very short timeout so the test
	// stays fast. We don't touch the listener — cmdLogin should
	// time out by itself.
	done := make(chan int, 1)
	go func() {
		done <- cmdLogin([]string{"--timeout", "300ms"})
	}()
	select {
	case code := <-done:
		if code == 0 {
			t.Error("timeout path should not exit 0")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cmdLogin did not exit after timeout")
	}
}

// TestRevokeOwnToken_ParsesPATPrefix covers the secret-extraction in
// cmdLogout. We don't need a real server — the fakeSrv records the
// path the CLI hit and the Authorization header it sent.
func TestRevokeOwnToken_ParsesPATPrefix(t *testing.T) {
	var (
		gotPath string
		gotAuth string
		mu      sync.Mutex
	)
	fake := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	defer fake.Close()

	cfg := &Config{
		AppURL:   fake.URL,
		APIToken: "pat_tok_smoke_aaa",
	}
	if err := revokeOwnToken(cfg); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/api/v1/cli-tokens/tok_smoke" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer pat_tok_smoke_aaa" {
		t.Errorf("auth = %q", gotAuth)
	}
}

// fakeServer is a tiny http.Server we use to stand in for app.agentry.run
// in CLI tests. Returns the test URL + a cleanup hook.
type fakeServer struct {
	URL    string
	server *http.Server
}

func (f *fakeServer) Close() { _ = f.server.Shutdown(context.Background()) }

func newFakeServer(t *testing.T, h http.HandlerFunc) *fakeServer {
	t.Helper()
	ln, err := newLocalListener()
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	return &fakeServer{URL: "http://" + ln.Addr().String(), server: srv}
}

func newLocalListener() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

