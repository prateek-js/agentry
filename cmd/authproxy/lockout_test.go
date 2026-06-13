package main

import (
	"testing"
	"time"
)

func TestLockoutUntil_BelowThresholdNoLock(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for attempts := 0; attempts < lockoutThreshold; attempts++ {
		if got := lockoutUntil(now, attempts); !got.IsZero() {
			t.Errorf("attempts=%d should not lock; got %v", attempts, got)
		}
	}
}

func TestLockoutUntil_BackoffDoublesAndCaps(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	// First lock = base delay.
	if got := lockoutUntil(now, lockoutThreshold).Sub(now); got != lockoutBaseDelay {
		t.Errorf("first lock = %v; want %v", got, lockoutBaseDelay)
	}
	// Next doubles.
	if got := lockoutUntil(now, lockoutThreshold+1).Sub(now); got != 2*lockoutBaseDelay {
		t.Errorf("second lock = %v; want %v", got, 2*lockoutBaseDelay)
	}
	// Way past threshold caps at the max.
	if got := lockoutUntil(now, lockoutThreshold+50).Sub(now); got != lockoutMaxDelay {
		t.Errorf("far-past lock = %v; want cap %v", got, lockoutMaxDelay)
	}
}

func TestIsLocked(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	future := now.Add(time.Minute)
	past := now.Add(-time.Minute)
	if isLocked(nil, now) {
		t.Error("nil lockedUntil should never be locked")
	}
	if !isLocked(&future, now) {
		t.Error("future deadline should be locked")
	}
	if isLocked(&past, now) {
		t.Error("past deadline should not be locked")
	}
}
