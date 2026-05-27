package tunnel

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipePair returns two net.Conns wired to each other so the test can
// drive both ends from one process. Use net.Pipe rather than a TCP
// listener; we're testing the pump, not the network.
func pipePair() (net.Conn, net.Conn) {
	return net.Pipe()
}

func TestCopyStreamsPipesBothDirections(t *testing.T) {
	aLeft, aRight := pipePair()
	bLeft, bRight := pipePair()
	t.Cleanup(func() {
		aLeft.Close()
		aRight.Close()
		bLeft.Close()
		bRight.Close()
	})

	// Pump aRight ↔ bLeft. So:
	//   write to aLeft → reads from bRight
	//   write to bRight → reads from aLeft
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pumpDone := make(chan error, 1)
	go func() {
		pumpDone <- CopyStreams(ctx, aRight, bLeft, CopyOptions{})
	}()

	// a → b
	if _, err := io.WriteString(aLeft, "hello"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(bRight, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Errorf("a→b got %q; want hello", buf)
	}

	// b → a
	if _, err := io.WriteString(bRight, "world"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(aLeft, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "world" {
		t.Errorf("b→a got %q; want world", buf)
	}

	// Close one side; pump should return cleanly.
	_ = aLeft.Close()
	select {
	case err := <-pumpDone:
		if err != nil && !strings.Contains(err.Error(), "closed") &&
			!strings.Contains(err.Error(), "EOF") {
			t.Errorf("pump returned unexpected err: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("pump did not return after close")
	}
}

func TestCopyStreamsClosesBothOnCtxCancel(t *testing.T) {
	a1, a2 := pipePair()
	b1, b2 := pipePair()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = CopyStreams(ctx, a2, b1, CopyOptions{
			DeadlineRefresh: 50 * time.Millisecond,
		})
		close(done)
	}()

	// Cancel; the pump should tear down both connections.
	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("pump did not return after ctx cancel")
	}

	// Writes to the user-facing ends should now fail (the inner conns
	// were closed by the pump).
	a1.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
	b2.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := a1.Write([]byte("x")); err == nil {
		t.Error("expected write to a1 to fail after pump teardown")
	}
	if _, err := b2.Write([]byte("x")); err == nil {
		t.Error("expected write to b2 to fail after pump teardown")
	}
}

func TestWConnFiresOnCloseOnce(t *testing.T) {
	a, b := pipePair()
	defer b.Close()

	var calls int
	var mu sync.Mutex
	w := NewWConn(a, func() {
		mu.Lock()
		calls++
		mu.Unlock()
	})

	// Close twice; the hook must only fire once.
	_ = w.Close()
	_ = w.Close()
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("onClose called %d times; want 1", calls)
	}
}
