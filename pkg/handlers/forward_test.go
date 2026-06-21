package handlers

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/agentry-ai/agentry/pkg/tunnel"
)

// runtimeServer stands up an httptest server whose handler diverts
// CONNECT requests to ForwardConnectHandler. Mirrors the way the
// real runtime wires it via the connectInterceptor in pkg/runtime.
func runtimeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			ForwardConnectHandler(w, r)
			return
		}
		http.NotFound(w, r)
	}))
}

// dialConnect issues a CONNECT to the given runtimeURL targeting
// host:port, validates the 200, and returns the now-raw conn ready
// for byte-pumping.
func dialConnect(t *testing.T, runtimeURL, target string) (net.Conn, *bufio.Reader) {
	t.Helper()
	host := runtimeURL[len("http://"):] // httptest is HTTP
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.WriteConnect(conn, target, nil); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	if err := tunnel.ReadConnectResponse(br); err != nil {
		t.Fatal(err)
	}
	return conn, br
}

func TestForwardConnect_PipesBytesBothWays(t *testing.T) {
	// Fake "user app" listening on a random localhost port. Reads
	// what we send, replies with REVERSED bytes — exercises both
	// directions in the same exchange.
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	appPort := app.Addr().(*net.TCPAddr).Port

	go func() {
		conn, err := app.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 32)
		n, _ := conn.Read(buf)
		// Reverse and echo.
		for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
			buf[i], buf[j] = buf[j], buf[i]
		}
		_, _ = conn.Write(buf[:n])
	}()

	rt := runtimeServer(t)
	defer rt.Close()

	conn, br := dialConnect(t, rt.URL, fmt.Sprintf("127.0.0.1:%d", appPort))
	defer conn.Close()

	// client → app
	if _, err := conn.Write([]byte("hello!!")); err != nil {
		t.Fatal(err)
	}
	// app → client (drain via bufio in case bytes arrived early)
	got := make([]byte, 7)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "!!olleh" {
		t.Errorf("got %q; want %q (reversed)", got, "!!olleh")
	}
}

func TestForwardConnect_RejectsNonLoopbackTarget(t *testing.T) {
	rt := runtimeServer(t)
	defer rt.Close()

	host := rt.URL[len("http://"):]
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = tunnel.WriteConnect(conn, "8.8.8.8:53", nil)
	br := bufio.NewReader(conn)
	err = tunnel.ReadConnectResponse(br)
	if err == nil || err.Error() == "" {
		t.Fatal("expected rejection on non-loopback CONNECT target")
	}
}

func TestForwardConnect_RejectsRuntimePort(t *testing.T) {
	rt := runtimeServer(t)
	defer rt.Close()

	host := rt.URL[len("http://"):]
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = tunnel.WriteConnect(conn, "127.0.0.1:8080", nil)
	br := bufio.NewReader(conn)
	if err := tunnel.ReadConnectResponse(br); err == nil {
		t.Fatal("expected 8080 rejection (runtime API self-loop)")
	}
}

func TestForwardConnect_502OnUnreachableUpstream(t *testing.T) {
	rt := runtimeServer(t)
	defer rt.Close()

	host := rt.URL[len("http://"):]
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Port 1 is reserved + nothing's there.
	_ = tunnel.WriteConnect(conn, "127.0.0.1:1", nil)
	br := bufio.NewReader(conn)
	if err := tunnel.ReadConnectResponse(br); err == nil {
		t.Fatal("expected dial failure for port 1")
	}
}

// TestForwardConnect_LargePayload exercises the byte pump under
// load — 1 MiB through with random bytes, both directions. Catches
// short-read / buffer-resize bugs in the CopyStreams wiring.
func TestForwardConnect_LargePayload(t *testing.T) {
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	port := app.Addr().(*net.TCPAddr).Port

	const N = 1024 * 1024
	payload := make([]byte, N)
	_, _ = rand.Read(payload)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := app.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// echo
		_, _ = io.Copy(conn, conn)
	}()

	rt := runtimeServer(t)
	defer rt.Close()

	conn, br := dialConnect(t, rt.URL, fmt.Sprintf("127.0.0.1:%d", port))
	defer conn.Close()

	// Send + receive in parallel — one-sided drain would deadlock.
	doneRecv := make(chan error, 1)
	go func() {
		got := make([]byte, N)
		_, err := io.ReadFull(br, got)
		if err == nil {
			for i := range got {
				if got[i] != payload[i] {
					doneRecv <- fmt.Errorf("byte %d differs", i)
					return
				}
			}
		}
		doneRecv <- err
	}()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := <-doneRecv; err != nil {
		t.Fatalf("receive: %v", err)
	}
}
