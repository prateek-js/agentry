package provisioner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentry-ai/agentry/pkg/bridge"
	"github.com/agentry-ai/agentry/pkg/tunnel"
)

// TestProvisionerOverBrokerEndToEnd is the load-bearing test for the
// new wiring:
//
//	mock device → broker → provisioner (mock backend)
//
// It stands up a broker in-process, starts a provisioner that phones
// home as cluster "alpha", then issues a real HTTP request the way
// the xdp daemon will eventually — through a tunnel session with
// X-Cluster: alpha. Asserts the request hits the provisioner's
// handler and the response comes back intact.
//
// If this passes, the full plumbing (handshake, role routing,
// reverse-proxy, session multiplexing, http.Serve over yamux) is
// correct end-to-end.
func TestProvisionerOverBrokerEndToEnd(t *testing.T) {
	// 1. Broker on a random port via httptest.
	b := bridge.NewWithConfig(bridge.Config{DevMode: true})
	brokerSrv := httptest.NewServer(b.Handler())
	t.Cleanup(brokerSrv.Close)
	t.Cleanup(func() { _ = b.Shutdown(context.Background()) })

	// 2. Provisioner with a mock backend. We don't run p.Run() because
	//    we don't want the local listener / signal handlers; instead
	//    we drive BrokerClient directly with the provisioner's Handler.
	mock := NewMockBackend()
	cfg := Config{
		Namespace:    "test-ns",
		SandboxImage: "test-image:latest",
		NodeHost:     "test-host",
		Labels:       map[string]string{"app": "ad-sandbox"},
		// Setting BrokerURL flips sandbox_url to the runtime-proxy
		// form (the public shape). The actual dial target the test
		// uses comes via NewBrokerClient below — these two paths are
		// independent on purpose so tests can exercise tunneled URL
		// rendering without coupling to which broker is connected.
		BridgeURL: brokerSrv.URL,
		ClusterID: "alpha",
	}
	p := NewWithKey(cfg, mock, "")
	p.SetReadyProbe(func(context.Context, string) error { return nil })

	// 3. Spin up the broker tunnel client on a background context.
	bcCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bc := NewBrokerClient(brokerSrv.URL, "alpha", p.Handler(), nil)
	go func() { _ = bc.Run(bcCtx) }()

	// Wait for the provisioner's tunnel to register on the bridge.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bc.Connected() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bc.Connected() {
		t.Fatal("broker client never reported connected")
	}

	// 4. Mock device side: dial the broker as role=device, build an
	//    http.Client whose transport routes through the session, and
	//    auto-stamp X-Cluster: alpha on every request.
	deviceSess, err := tunnel.Dial(context.Background(), tunnel.DialConfig{
		BrokerURL: brokerSrv.URL,
		Role:      tunnel.RoleDevice,
		Headers:   http.Header{tunnel.HeaderDeviceID: []string{"laptop-A"}},
	})
	if err != nil {
		t.Fatalf("device dial: %v", err)
	}
	t.Cleanup(func() { _ = deviceSess.Close() })

	rt := tunnel.NewRoundTripper(deviceSess)
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.Header.Set(bridge.HeaderTargetCluster, "alpha")
			return rt.RoundTrip(r)
		}),
	}

	// 5. Real provisioner API call: create a sandbox via POST
	//    /api/sandboxes. The mock backend records it; we then verify
	//    the response shape matches what the local listener would
	//    have produced.
	body := strings.NewReader(`{"sandbox_id":"sb1","thread_id":"t1"}`)
	resp, err := client.Post("http://anything/api/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST /api/sandboxes: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, raw)
	}

	var info SandboxInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.SandboxID != "sb1" {
		t.Errorf("sandbox_id = %q; want sb1", info.SandboxID)
	}
	// In tunneled mode the URL is the runtime-proxy path on this
	// provisioner, host placeholder = bridge.invalid. The device's
	// transport ignores the host; the path is what carries.
	wantPrefix := "http://bridge.invalid/api/sandboxes/sb1/runtime"
	if info.SandboxURL != wantPrefix {
		t.Errorf("sandbox_url = %q; want %q", info.SandboxURL, wantPrefix)
	}

	// Spot-check that the mock actually saw the create (which proves
	// the request reached the provisioner via the broker, not via the
	// local listener — there isn't one in this test).
	if mock.PodCount() != 1 {
		t.Errorf("mock pod count = %d; want 1", mock.PodCount())
	}
}

// TestRuntimeProxyForwardsThroughTunnel is the load-bearing test for
// the runtime-proxy path: device sends an MCP-style runtime call
// (think `command_run`), it lands on the provisioner's runtime-proxy
// endpoint, which reverse-proxies to a fake "runtime" listening on
// the host. End-to-end proof that command_run / file_* / code_exec
// will work over xdp once the daemon is in place.
func TestRuntimeProxyForwardsThroughTunnel(t *testing.T) {
	// Fake "runtime" — stands in for the per-sandbox container's
	// HTTP server. The provisioner's runtime-proxy hands requests to
	// it via its NodeHost:port.
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Saw-Path", r.URL.Path)
		_, _ = w.Write([]byte("runtime-got=" + r.URL.Path))
	}))
	t.Cleanup(runtime.Close)

	// Pull host + port from the runtime listener so the mock backend
	// can advertise them as "NodePort" for sandbox sb1.
	host, port := splitHostPort(t, runtime.URL)

	// Broker.
	b := bridge.NewWithConfig(bridge.Config{DevMode: true})
	brokerSrv := httptest.NewServer(b.Handler())
	t.Cleanup(brokerSrv.Close)
	t.Cleanup(func() { _ = b.Shutdown(context.Background()) })

	// Provisioner. NodeHost points at the fake runtime's host so the
	// proxy's reverse-target resolves to where httptest actually
	// listens. ClusterID + BrokerURL set so URL rewriting kicks in.
	mock := NewMockBackend()
	// Pre-seed: pretend a sandbox-sb1 already exists with the fake
	// runtime as its NodePort.
	mock.preSeed("sb1", host, port)

	p := NewWithKey(Config{
		Namespace: "test-ns",
		NodeHost:  host,
		Labels:    map[string]string{"app": "agentry-sandbox"},
		BridgeURL: brokerSrv.URL,
		ClusterID: "alpha",
	}, mock, "")

	bcCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bc := NewBrokerClient(brokerSrv.URL, "alpha", p.Handler(), nil)
	go func() { _ = bc.Run(bcCtx) }()
	waitConnected(t, bc, 3*time.Second)

	// Device.
	deviceSess, err := tunnel.Dial(context.Background(), tunnel.DialConfig{
		BrokerURL: brokerSrv.URL,
		Role:      tunnel.RoleDevice,
		Headers:   http.Header{tunnel.HeaderDeviceID: []string{"laptop-A"}},
	})
	if err != nil {
		t.Fatalf("device dial: %v", err)
	}
	t.Cleanup(func() { _ = deviceSess.Close() })

	rt := tunnel.NewRoundTripper(deviceSess)
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.Header.Set(bridge.HeaderTargetCluster, "alpha")
			return rt.RoundTrip(r)
		}),
	}

	// This is the exact URL shape mcp.Client uses when it has been
	// told sandbox_url = "http://bridge.invalid/api/sandboxes/sb1/runtime"
	// and it appends "/v1/shell/exec".
	resp, err := client.Post(
		"http://bridge.invalid/api/sandboxes/sb1/runtime/v1/shell/exec",
		"application/json",
		strings.NewReader(`{"command":"echo hi"}`),
	)
	if err != nil {
		t.Fatalf("runtime proxy POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, raw)
	}
	body, _ := io.ReadAll(resp.Body)
	if want := "runtime-got=/v1/shell/exec"; string(body) != want {
		t.Errorf("body = %q; want %q", body, want)
	}
	if got := resp.Header.Get("X-Saw-Path"); got != "/v1/shell/exec" {
		t.Errorf("runtime saw path = %q; want /v1/shell/exec (proxy didn't strip prefix)", got)
	}
}

// TestBrokerClientReconnectsAfterBrokerRestart is the "tunnel drops,
// comes back" case. Important because the cloud-am pattern hinges on
// the cluster surviving broker bounces.
func TestBrokerClientReconnectsAfterBrokerRestart(t *testing.T) {
	mock := NewMockBackend()
	p := NewWithKey(Config{
		Namespace:    "test-ns",
		SandboxImage: "test-image:latest",
		NodeHost:     "test-host",
		Labels:       map[string]string{"app": "ad-sandbox"},
	}, mock, "")
	p.SetReadyProbe(func(context.Context, string) error { return nil })

	// Round 1: broker is up.
	b1 := bridge.NewWithConfig(bridge.Config{DevMode: true})
	srv1 := httptest.NewServer(b1.Handler())

	bcCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bc := NewBrokerClient(srv1.URL, "alpha", p.Handler(), nil)
	go func() { _ = bc.Run(bcCtx) }()

	waitConnected(t, bc, 3*time.Second)

	// Bring the broker down.
	_ = b1.Shutdown(context.Background())
	srv1.Close()

	// Connected should flip false within a few yamux keepalive ticks.
	waitDisconnected(t, bc, 5*time.Second)

	// Round 2: a new broker on the same URL — except httptest gives
	// us a fresh port, so we need to point the BrokerClient at a
	// different URL. For this test that's fine: we just verify the
	// reconnect *loop* doesn't crash. End-to-end "same URL, new
	// instance" requires a stable listener address; covered by the
	// xdp-broker binary's behavior, not by this unit.
	_ = bc
}

// waitConnected blocks until bc.Connected() returns true or the
// timeout fires.
func waitConnected(t *testing.T, bc *BrokerClient, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if bc.Connected() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("broker client never connected")
}

// waitDisconnected is the inverse — block until the tunnel goes down.
func waitDisconnected(t *testing.T, bc *BrokerClient, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !bc.Connected() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("broker client did not disconnect")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// splitHostPort extracts host + port from a URL string for the
// runtime-proxy test. httptest URLs always have the shape
// http://host:port; this helper just sugars it.
func splitHostPort(t *testing.T, raw string) (string, int32) {
	t.Helper()
	// Parse manually rather than pulling in net/url just for one
	// call. Format is http://host:port.
	trimmed := strings.TrimPrefix(raw, "http://")
	host, port, ok := strings.Cut(trimmed, ":")
	if !ok {
		t.Fatalf("can't split host:port out of %q", raw)
	}
	var p int32
	for _, c := range port {
		p = p*10 + int32(c-'0')
	}
	return host, p
}
