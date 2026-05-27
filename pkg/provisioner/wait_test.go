package provisioner

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// TestWaitPortReachableSucceeds spins up a real net.Listener and
// confirms waitPortReachable returns nil immediately.
func TestWaitPortReachableSucceeds(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Don't bother accepting — for TCP dial readiness we only care
	// that the kernel completes the SYN/SYN-ACK handshake, which a
	// listening socket will always do.
	url := "http://" + l.Addr().String()
	if err := waitPortReachable(context.Background(), url, 2*time.Second); err != nil {
		t.Fatalf("listener up but probe failed: %v", err)
	}
}

// TestWaitPortReachableTimesOut points the probe at a port nothing is
// listening on. The kernel rejects with ECONNREFUSED fast, so this
// should still complete near the timeout budget.
func TestWaitPortReachableTimesOut(t *testing.T) {
	// Grab a free port and immediately close it — nobody should reclaim
	// 127.0.0.1:<that> in the next second.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	start := time.Now()
	err = waitPortReachable(context.Background(), url, 500*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("error = %q; want 'not reachable'", err)
	}
	if elapsed < 400*time.Millisecond || elapsed > 1*time.Second {
		t.Errorf("elapsed = %s; want ~500ms", elapsed)
	}
}

func TestWaitPortReachableHonorsCtxCancel(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(120 * time.Millisecond)
		cancel()
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	start := time.Now()
	err = waitPortReachable(ctx, url, 5*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected ctx cancel error")
	}
	if elapsed > 1*time.Second {
		t.Errorf("ctx cancel ignored — elapsed = %s", elapsed)
	}
}

func TestWaitPortReachableRejectsBadURL(t *testing.T) {
	// No scheme, no host — url.Parse swallows this. waitPortReachable
	// has to catch and reject before dialing.
	if err := waitPortReachable(context.Background(), "::not a url", 1*time.Second); err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

// TestWaitPortReachableAcceptsCloseImmediately covers the edge case
// where the listener accepts a connection and immediately closes it
// (some servers do this during very early init). We should still
// report "reachable" — a TCP handshake completing is the contract.
func TestWaitPortReachableAcceptsCloseImmediately(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Accept loop that just closes every conn.
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	url := "http://" + l.Addr().String()
	if err := waitPortReachable(context.Background(), url, 1*time.Second); err != nil {
		t.Fatalf("accept-and-close listener should still be 'reachable': %v", err)
	}
}
