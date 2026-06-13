package tunnel

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

// BackoffConfig parameterizes exponential backoff with a stable-window
// reset. The zero value is unusable — pick one of the canned configs
// (DialerBackoff / StreamBackoff) or set every field.
type BackoffConfig struct {
	// Base is the first delay. Doubles each failure.
	Base time.Duration
	// Max caps the per-attempt delay.
	Max time.Duration
	// ResetAfter: if Next() is called after this much time has elapsed
	// since the previous attempt, the attempt counter is reset to zero
	// (we treat the long quiet window as evidence that the connection
	// was stable, so the next failure starts from Base, not from where
	// the last burst left off).
	ResetAfter time.Duration
	// MaxAttempts: 0 means infinite. Set when the caller wants to give
	// up after N tries (used for one-shot operations like login).
	MaxAttempts int
	// Jitter, in (0,1], spreads the delay down by up to this fraction:
	// the returned delay is uniform in [d*(1-Jitter), d]. Without it,
	// a fleet of clients that all dropped at the same instant (e.g. a
	// bridge restart) retries in lockstep at t=Base,2·Base,… and can
	// re-collide on every wave. 0 disables jitter (deterministic).
	Jitter float64
}

// DialerBackoff is the right config for the long-lived tunnel session
// (xdp daemon → broker, provisioner → broker). Slow rampup, generous
// ceiling — we expect to recover from a network outage rather than
// trying to drown the broker in retries.
func DialerBackoff() BackoffConfig {
	return BackoffConfig{
		Base:        2 * time.Second,
		Max:         5 * time.Minute,
		ResetAfter:  10 * time.Minute,
		MaxAttempts: 0,
		Jitter:      0.3, // ±up-to-30% so a fleet doesn't reconnect in lockstep
	}
}

// StreamBackoff is the right config for per-request retries inside a
// single tunnel session. Fast rampup, low ceiling — if the upstream is
// flaky we want to retry quickly and surface the error fast.
func StreamBackoff() BackoffConfig {
	return BackoffConfig{
		Base:        500 * time.Millisecond,
		Max:         30 * time.Second,
		ResetAfter:  5 * time.Minute,
		MaxAttempts: 0,
	}
}

// Backoff is the runtime state for one retry loop. Safe for concurrent
// use; the caller is expected to be a single retry loop in practice.
type Backoff struct {
	cfg BackoffConfig

	mu       sync.Mutex
	attempts int
	last     time.Time
}

// NewBackoff returns a Backoff with the given config. nil-cfg falls
// back to DialerBackoff so misconfigured callers don't infinite-loop
// at zero delay.
func NewBackoff(cfg BackoffConfig) *Backoff {
	if cfg.Base <= 0 {
		cfg = DialerBackoff()
	}
	return &Backoff{cfg: cfg}
}

// Next returns the delay to wait before the next attempt. Returns 0
// when MaxAttempts is set and exceeded — the caller should treat that
// as "give up". The stable-window reset means a long quiet period
// between calls resets the counter.
func (b *Backoff) Next() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cfg.ResetAfter > 0 && !b.last.IsZero() &&
		time.Since(b.last) > b.cfg.ResetAfter {
		b.attempts = 0
	}
	if b.cfg.MaxAttempts > 0 && b.attempts >= b.cfg.MaxAttempts {
		return 0
	}

	d := time.Duration(float64(b.cfg.Base) * math.Pow(2, float64(b.attempts)))
	if d > b.cfg.Max {
		d = b.cfg.Max
	}
	b.attempts++
	b.last = time.Now()
	if b.cfg.Jitter > 0 {
		if spread := int64(float64(d) * b.cfg.Jitter); spread > 0 {
			d -= time.Duration(rand.Int63n(spread))
		}
	}
	return d
}

// Reset returns the backoff to its initial state. Callers do this on
// successful operation so the next failure starts from Base.
func (b *Backoff) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts = 0
	b.last = time.Time{}
}

// Attempts returns how many times Next has been called since the last
// Reset (or stable-window reset). Useful for log lines / observability.
func (b *Backoff) Attempts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}
