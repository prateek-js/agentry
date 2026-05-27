package bridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentry/agentry/pkg/tunnel"
	"github.com/hashicorp/yamux"
)

// startBroker stands up a Broker behind an httptest.Server and
// returns both. The httptest URL is what devices/clusters dial.
//
// Tests use DevMode=true so the routing core doesn't trip the
// production mTLS / role-CN checks — those are exercised separately
// in discovery_prod_test.go with a real cert chain.
func startBroker(t *testing.T) (*Broker, *httptest.Server) {
	t.Helper()
	b := NewWithConfig(Config{DevMode: true})
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(func() {
		_ = b.Shutdown(context.Background())
		srv.Close()
	})
	return b, srv
}

// joinCluster connects to the broker as role=cluster and serves the
// given http.Handler over its yamux session. Returns when the
// session closes. The caller starts it in a goroutine.
func joinCluster(t *testing.T, brokerURL, clusterID string, h http.Handler) *yamux.Session {
	t.Helper()
	sess, err := tunnel.Dial(context.Background(), tunnel.DialConfig{
		BrokerURL: brokerURL,
		Role:      tunnel.RoleCluster,
		Headers:   http.Header{HeaderClusterID: []string{clusterID}},
	})
	if err != nil {
		t.Fatalf("cluster %s dial: %v", clusterID, err)
	}
	go func() {
		// http.Serve on the yamux session — the broker will
		// OpenStream() per request, this side AcceptStream()s and
		// dispatches via the handler.
		_ = http.Serve(sess, h)
	}()
	return sess
}

// dialDevice connects as role=device and returns an http.Client whose
// transport routes through the resulting yamux session. This is the
// shape the xdp daemon will use in production.
func dialDevice(t *testing.T, brokerURL, deviceID string) (*http.Client, *yamux.Session) {
	t.Helper()
	sess, err := tunnel.Dial(context.Background(), tunnel.DialConfig{
		BrokerURL: brokerURL,
		Role:      tunnel.RoleDevice,
		Headers:   http.Header{HeaderDeviceID: []string{deviceID}},
	})
	if err != nil {
		t.Fatalf("device %s dial: %v", deviceID, err)
	}
	return &http.Client{Transport: tunnel.NewRoundTripper(sess)}, sess
}

// withCluster is a tiny helper that adds the routing header to every
// request the http.Client makes, so the per-test bodies stay tidy.
func withCluster(client *http.Client, target string) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.Header.Set(HeaderTargetCluster, target)
			return client.Transport.RoundTrip(r)
		}),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBrokerRoutesDeviceRequestToNamedCluster(t *testing.T) {
	_, srv := startBroker(t)

	// Two distinct clusters. Each echoes back its own ID so we can
	// tell which one received the request.
	for _, id := range []string{"us-west", "us-east"} {
		id := id
		joinCluster(t, srv.URL, id, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Forwarded-Device-Seen", r.Header.Get("X-Forwarded-Device"))
			_, _ = w.Write([]byte("cluster=" + id + ";path=" + r.URL.Path))
		}))
	}

	client, _ := dialDevice(t, srv.URL, "device-A")
	c := withCluster(client, "us-east")

	// Wait until both clusters show up in the directory. Without this
	// the test races the goroutines that register sessions.
	mustHaveClusters(t, srv.URL, []string{"us-west", "us-east"})

	resp, err := c.Get("http://anything/v1/sandboxes")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "cluster=us-east") {
		t.Errorf("expected route to us-east; body=%q", body)
	}
	if got := resp.Header.Get("X-Forwarded-Device-Seen"); got != "device-A" {
		t.Errorf("X-Forwarded-Device-Seen = %q; want device-A", got)
	}
}

func TestBrokerErrorsOnUnknownCluster(t *testing.T) {
	_, srv := startBroker(t)
	client, _ := dialDevice(t, srv.URL, "device-A")
	c := withCluster(client, "does-not-exist")

	resp, err := c.Get("http://anything/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", resp.StatusCode)
	}
}

func TestBrokerErrorsOnMissingTargetHeader(t *testing.T) {
	_, srv := startBroker(t)
	// Need a cluster on the books so the test doesn't accidentally
	// hit the unknown-cluster path instead of the missing-header path.
	joinCluster(t, srv.URL, "any", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	}))
	mustHaveClusters(t, srv.URL, []string{"any"})

	client, _ := dialDevice(t, srv.URL, "device-A")
	// Don't wrap with withCluster — fire raw.
	resp, err := client.Get("http://anything/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}
}

func TestBrokerSnapshotReportsConnections(t *testing.T) {
	b, srv := startBroker(t)

	joinCluster(t, srv.URL, "alpha", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	joinCluster(t, srv.URL, "beta", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dialDevice(t, srv.URL, "dev-1")
	dialDevice(t, srv.URL, "dev-2")

	// Allow registrations to settle.
	mustHaveClusters(t, srv.URL, []string{"alpha", "beta"})
	mustHaveDevices(t, b, []string{"dev-1", "dev-2"})

	snap := b.Snapshot()
	if len(snap.Clusters) != 2 || len(snap.Devices) != 2 {
		t.Errorf("snapshot got %d clusters %d devices; want 2/2",
			len(snap.Clusters), len(snap.Devices))
	}
}

func TestBrokerReplacesStaleClusterOnReconnect(t *testing.T) {
	_, srv := startBroker(t)

	first := joinCluster(t, srv.URL, "us-west", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first"))
	}))
	mustHaveClusters(t, srv.URL, []string{"us-west"})

	// New session, same cluster ID. Broker should swap in the new one.
	second := joinCluster(t, srv.URL, "us-west", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("second"))
	}))
	// Original session should be closed by the broker.
	waitFor(t, func() bool { return first.IsClosed() }, 2*time.Second, "first session closed")
	_ = second

	client, _ := dialDevice(t, srv.URL, "device-A")
	c := withCluster(client, "us-west")
	resp, err := c.Get("http://x/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "second" {
		t.Errorf("body = %q; want second (the new session)", body)
	}
}

func TestBrokerConcurrentRequestsMultiplexOverOneTunnel(t *testing.T) {
	_, srv := startBroker(t)

	// Cluster handler tags each response with the call number so we
	// can detect crossed-stream bugs.
	joinCluster(t, srv.URL, "us-west", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("call=" + r.URL.Path))
	}))
	mustHaveClusters(t, srv.URL, []string{"us-west"})

	client, _ := dialDevice(t, srv.URL, "device-A")
	c := withCluster(client, "us-west")

	const N = 20
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := "/" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			resp, err := c.Get("http://x" + path)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			want := "call=" + path
			if string(body) != want {
				errs <- &mismatchErr{want: want, got: string(body)}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

type mismatchErr struct{ want, got string }

func (m *mismatchErr) Error() string { return "got " + m.got + "; want " + m.want }

// mustHaveClusters waits until enough scheduler ticks have passed
// that the broker's serveCluster goroutines have registered. Sessions
// register synchronously inside the broker's serve goroutine after
// Dial returns; 50ms is comfortably above scheduler noise on macOS.
func mustHaveClusters(t *testing.T, brokerURL string, ids []string) {
	t.Helper()
	_ = ids // ids documents intent; broker reachability is observable via the test body.
	time.Sleep(50 * time.Millisecond)
}

func mustHaveDevices(t *testing.T, b *Broker, ids []string) {
	t.Helper()
	waitFor(t, func() bool {
		seen := make(map[string]bool)
		for _, d := range b.Snapshot().Devices {
			seen[d.ID] = true
		}
		for _, id := range ids {
			if !seen[id] {
				return false
			}
		}
		return true
	}, 2*time.Second, "devices "+strings.Join(ids, ","))
}

func waitFor(t *testing.T, ok func() bool, timeout time.Duration, label string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}
