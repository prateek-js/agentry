package main

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsUpToMaxThenBlocks(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if rl.allow("1.2.3.4") {
		t.Error("4th attempt should be blocked")
	}
	// A different key is unaffected.
	if !rl.allow("5.6.7.8") {
		t.Error("a different IP should have its own budget")
	}
}

func TestRateLimiter_WindowSlides(t *testing.T) {
	cur := time.Unix(1_000_000, 0)
	rl := newRateLimiter(2, time.Minute)
	rl.now = func() time.Time { return cur }

	if !rl.allow("ip") || !rl.allow("ip") {
		t.Fatal("first two should be allowed")
	}
	if rl.allow("ip") {
		t.Fatal("third within window should be blocked")
	}
	// Advance past the window; the old hits age out.
	cur = cur.Add(2 * time.Minute)
	if !rl.allow("ip") {
		t.Error("after the window slides, attempts should be allowed again")
	}
}

func TestRateLimiter_ZeroMaxDisables(t *testing.T) {
	rl := newRateLimiter(0, time.Minute)
	for i := 0; i < 100; i++ {
		if !rl.allow("ip") {
			t.Fatal("max=0 should disable the limiter (always allow)")
		}
	}
}
