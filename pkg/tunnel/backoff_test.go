package tunnel

import (
	"testing"
	"time"
)

func TestBackoffNextGrowsExponentiallyAndCaps(t *testing.T) {
	b := NewBackoff(BackoffConfig{
		Base: 100 * time.Millisecond,
		Max:  800 * time.Millisecond,
	})

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		800 * time.Millisecond, // capped
		800 * time.Millisecond, // still capped
	}
	for i, w := range want {
		got := b.Next()
		if got != w {
			t.Errorf("attempt %d: got %s; want %s", i, got, w)
		}
	}
}

func TestBackoffResetReturnsToBase(t *testing.T) {
	b := NewBackoff(BackoffConfig{Base: 1 * time.Second, Max: 10 * time.Second})
	b.Next()
	b.Next()
	b.Next()
	b.Reset()
	if got := b.Next(); got != 1*time.Second {
		t.Fatalf("after Reset got %s; want 1s", got)
	}
}

func TestBackoffMaxAttemptsGivesUp(t *testing.T) {
	b := NewBackoff(BackoffConfig{
		Base:        50 * time.Millisecond,
		Max:         1 * time.Second,
		MaxAttempts: 3,
	})
	for i := 0; i < 3; i++ {
		if b.Next() == 0 {
			t.Fatalf("attempt %d should have a delay", i)
		}
	}
	if got := b.Next(); got != 0 {
		t.Errorf("4th attempt should signal give-up (0); got %s", got)
	}
}

func TestBackoffStableWindowResets(t *testing.T) {
	// Use a small reset window so the test isn't slow.
	b := NewBackoff(BackoffConfig{
		Base:       100 * time.Millisecond,
		Max:        1 * time.Second,
		ResetAfter: 50 * time.Millisecond,
	})
	b.Next() // 100ms
	b.Next() // 200ms
	// Sleep past ResetAfter; the next call should see the long quiet
	// period and start from Base.
	time.Sleep(100 * time.Millisecond)
	if got := b.Next(); got != 100*time.Millisecond {
		t.Errorf("after stable window got %s; want 100ms (reset)", got)
	}
}

// Jitter keeps the delay in [Base*(1-Jitter), Base] on the first attempt
// and varies across independent backoffs, so a fleet that dropped at the
// same instant doesn't reconnect in lockstep.
func TestBackoffJitterStaysInBandAndVaries(t *testing.T) {
	cfg := BackoffConfig{Base: 2 * time.Second, Max: 5 * time.Minute, Jitter: 0.3}
	lo, hi := 1400*time.Millisecond, 2*time.Second
	seen := map[time.Duration]bool{}
	for i := 0; i < 32; i++ {
		d := NewBackoff(cfg).Next()
		if d < lo || d > hi {
			t.Fatalf("jittered first delay %s outside [%s,%s]", d, lo, hi)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Error("jitter produced identical delays across 32 fresh backoffs")
	}
}

// Zero Jitter (the default for raw configs) stays exact — the existing
// deterministic tests rely on this.
func TestBackoffNoJitterIsExact(t *testing.T) {
	b := NewBackoff(BackoffConfig{Base: 100 * time.Millisecond, Max: time.Second})
	if got := b.Next(); got != 100*time.Millisecond {
		t.Fatalf("no-jitter first delay = %s; want exactly 100ms", got)
	}
}
