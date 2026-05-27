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
