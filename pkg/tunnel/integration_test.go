package tunnel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// startBroker stands up an httptest server whose /tunnel handler does
// the Accept-side of the handshake and exposes the resulting yamux
// session through ch. The "broker" is otherwise as dumb as possible —
// no auth, no routing, no role inspection — because this test is about
// the wire protocol.
func startBroker(t *testing.T, ch chan<- *yamux.Session) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/tunnel", func(w http.ResponseWriter, r *http.Request) {
		sess, err := Accept(w, r, AcceptConfig{})
		if err != nil {
			t.Logf("Accept failed: %v", err)
			return
		}
		ch <- sess
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestDialAcceptRoundTrip is the load-bearing test: it stands up a
// broker, dials it from a "client", and proves that a real HTTP
// request issued through NewRoundTripper(session) reaches a handler
// running INSIDE the broker. That's exactly the shape of
// xdp-daemon → broker → MCP-handler that we want in production.
func TestDialAcceptRoundTrip(t *testing.T) {
	sessCh := make(chan *yamux.Session, 1)
	srv := startBroker(t, sessCh)

	// The "upstream" the broker pretends to proxy to. Inside this test
	// it's just a goroutine that http.Serves over the yamux session
	// the broker accepted; in production, this is the per-cluster
	// provisioner on the other side of a second yamux session.
	upstream := http.NewServeMux()
	upstream.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Path", r.URL.Path)
		_, _ = w.Write([]byte("got=" + string(body)))
	})

	// Run an http.Server over the broker-side session. yamux.Session
	// implements net.Listener, so http.Serve eats it directly.
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		sess := <-sessCh
		_ = http.Serve(sess, upstream)
	}()

	// Dial the broker.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientSess, err := Dial(ctx, DialConfig{
		BrokerURL: srv.URL,
		Role:      RoleDevice,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { clientSess.Close() })

	// http.Client backed by the tunnel. This is the production shape:
	// MCP / provisioner clients build *http.Client{Transport: rt} and
	// don't know the bytes traverse a yamux session.
	rt := NewRoundTripper(clientSess)
	client := &http.Client{Transport: rt}

	resp, err := client.Post("http://upstream/echo", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("client.Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Path"); got != "/echo" {
		t.Errorf("X-Path = %q; want /echo", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "got=hello" {
		t.Errorf("body = %q; want got=hello", body)
	}

	// Multiple concurrent requests over the same session — proves
	// yamux multiplexing is wired up correctly.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			resp, err := client.Get("http://upstream/echo")
			if err != nil {
				t.Errorf("concurrent get %d: %v", n, err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}(i)
	}
	wg.Wait()
}

// TestDialRejectsNon200 verifies the dialer's error path: a broker
// that returns 401 (or any non-200) must fail with ErrHandshakeFailed
// so callers can distinguish "auth problem, don't retry" from "tcp
// drop, do retry."
func TestDialRejectsNon200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/tunnel", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Dial(ctx, DialConfig{BrokerURL: srv.URL, Role: RoleDevice})
	if err == nil {
		t.Fatal("expected ErrHandshakeFailed; got nil")
	}
	// Sentinel error so callers can errors.Is-match.
	if !strings.Contains(err.Error(), "handshake failed") {
		t.Errorf("err did not wrap ErrHandshakeFailed: %v", err)
	}
}
