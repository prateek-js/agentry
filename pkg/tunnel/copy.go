package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// CopyOptions controls the bidirectional pump. Zero-value defaults are
// the right answer for tunneled HTTP requests; only override if you
// have a specific reason (long-poll streams, raw TCP, …).
type CopyOptions struct {
	// BufSize is the per-direction copy buffer. Default 32 KiB matches
	// the io.Copy default and is what cloud-am has been running with.
	BufSize int

	// DeadlineRefresh is how often the pump pushes the read deadline on
	// each side forward. Without this, a long-lived idle stream gets
	// killed by yamux's own keepalive cancellation; with it, the
	// deadline tracks activity. Default 10s.
	DeadlineRefresh time.Duration

	// IdleTimeout is the read deadline that gets refreshed. Default 60s
	// — matches yamux's ConnectionWriteTimeout so the two layers agree
	// on liveness.
	IdleTimeout time.Duration
}

func (o CopyOptions) withDefaults() CopyOptions {
	if o.BufSize <= 0 {
		o.BufSize = 32 * 1024
	}
	if o.DeadlineRefresh <= 0 {
		o.DeadlineRefresh = 10 * time.Second
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 60 * time.Second
	}
	return o
}

// CopyStreams pipes a↔b bidirectionally until one side errors or the
// context cancels. Returns when both halves have completed. Closes both
// streams exactly once, even under racy teardown.
//
// The shape is the cloud-am pattern: one ctx cancels both directions,
// a periodic deadline refresh keeps idle reads alive, and a sync.Once
// guards the symmetric Close. Errors that smell like normal teardown
// (EOF, "use of closed network connection") are dropped from the
// returned error so callers can `if err != nil` cleanly.
func CopyStreams(ctx context.Context, a, b io.ReadWriteCloser, opts CopyOptions) error {
	opts = opts.withDefaults()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	defer closeBoth()

	refreshDeadlines := func() {
		dl := time.Now().Add(opts.IdleTimeout)
		if c, ok := a.(net.Conn); ok {
			_ = c.SetReadDeadline(dl)
		}
		if c, ok := b.(net.Conn); ok {
			_ = c.SetReadDeadline(dl)
		}
	}
	refreshDeadlines()

	// One goroutine handles both background tasks for the pump:
	//   - ticking the read deadlines forward so an idle stream doesn't
	//     get cut by yamux's own liveness layer
	//   - watching ctx.Done so an external cancel breaks the io.Copy
	//     loops (net.Conn.Read doesn't honor context; the only way to
	//     unblock it is to close the conn)
	ticker := time.NewTicker(opts.DeadlineRefresh)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				closeBoth()
				return
			case <-ticker.C:
				refreshDeadlines()
			}
		}
	}()

	// Two pump goroutines, each sends exactly one result to errs on
	// exit. We don't race them against ctx.Done() in the outer loop —
	// the pumps' own defer closeBoth() will unblock the other side via
	// the closed conn, then both will report and we collect them.
	errs := make(chan error, 2)
	pump := func(dst io.Writer, src io.Reader, name string) {
		buf := make([]byte, opts.BufSize)
		_, err := io.CopyBuffer(dst, src, buf)
		errs <- benignFilter(name, err)
		cancel() // tell the ticker goroutine to stop refreshing deadlines
		closeBoth()
	}

	go pump(a, b, "b→a")
	go pump(b, a, "a→b")

	var first error
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// benignFilter drops EOF and closed-network-connection errors, which
// are how a normal client/server-driven close shows up. We want a
// non-nil return only when something unexpected happened (a partial
// write, a protocol error, a context cancellation that isn't ours).
func benignFilter(direction string, err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	// net.errClosed isn't exported. The textual match is the
	// idiomatic way to identify it; the Go stdlib does the same.
	if isClosedConnErr(err) {
		return nil
	}
	return err
}

func isClosedConnErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return s == "use of closed network connection" ||
		// yamux wraps it
		s == "stream closed" ||
		s == "session shutdown"
}
