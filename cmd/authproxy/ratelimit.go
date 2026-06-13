package main

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// loginRateMax / loginRateWindow bound credential attempts per client IP.
// Defaults: 10 attempts per minute — comfortably above a human fat-
// fingering their password, far below a brute-force rate. Operators can
// retune via AGENTRY_AUTH_LOGIN_RATE_MAX (0 disables the throttle).
var (
	loginRateMax    = envInt("AGENTRY_AUTH_LOGIN_RATE_MAX", 10)
	loginRateWindow = time.Minute
)

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// ratelimit.go — a small in-memory sliding-window limiter keyed by
// client IP. Fronts the credential endpoints (/auth/login, /auth/signup,
// /auth/forgot) so a single host can't grind passwords or spray the
// mailer. Per-account lockout (store side) is the deeper defense; this is
// the cheap per-source throttle in front of it.
//
// In-memory is correct here: an authproxy is one process per app
// (sandbox project or deployment container), so there's no fleet to
// share state across. State resets on restart — acceptable, restarts are
// rare and an attacker gains at most one fresh window.

type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
	// now is injected for tests; defaults to time.Now.
	now func() time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		hits:   make(map[string][]time.Time),
		max:    max,
		window: window,
		now:    time.Now,
	}
}

// allow records an attempt for key and reports whether it's within the
// limit. The check + record are one locked operation so concurrent
// requests can't both slip past the threshold.
func (r *rateLimiter) allow(key string) bool {
	if r.max <= 0 {
		return true // disabled
	}
	now := r.now()
	cutoff := now.Add(-r.window)

	r.mu.Lock()
	defer r.mu.Unlock()

	// Prune this key's timestamps older than the window.
	times := r.hits[key]
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= r.max {
		r.hits[key] = kept
		return false
	}
	r.hits[key] = append(kept, now)

	// Opportunistic GC: when the map grows, drop keys whose newest hit
	// is already past the window so abandoned IPs don't accumulate.
	if len(r.hits) > 1024 {
		for k, ts := range r.hits {
			if len(ts) == 0 || ts[len(ts)-1].Before(cutoff) {
				delete(r.hits, k)
			}
		}
	}
	return true
}
