package main

import "time"

// lockout.go — account-lockout policy. One place so the SQL store, the
// mongo store, and the login handler all compute the same deadline.
//
// Model: the first few wrong passwords are free (legitimate typos).
// After lockoutThreshold consecutive failures the account locks, and
// each further failure doubles the wait, capped at lockoutMaxDelay. A
// successful login or a password reset clears the counter.

const (
	lockoutThreshold = 5
	lockoutBaseDelay = 1 * time.Minute
	lockoutMaxDelay  = 30 * time.Minute
)

// lockoutUntil returns the time an account should stay locked until after
// `attempts` consecutive failures, or the zero time when still below the
// threshold (no lock). `now` is injected for testability.
func lockoutUntil(now time.Time, attempts int) time.Time {
	if attempts < lockoutThreshold {
		return time.Time{}
	}
	over := attempts - lockoutThreshold // 0 on the first lock, then 1, 2, …
	d := lockoutBaseDelay
	for i := 0; i < over; i++ {
		d *= 2
		if d >= lockoutMaxDelay {
			d = lockoutMaxDelay
			break
		}
	}
	if d <= 0 || d > lockoutMaxDelay {
		d = lockoutMaxDelay
	}
	return now.Add(d)
}

// isLocked reports whether a LockedUntil deadline (nil = never locked)
// is still in the future relative to now.
func isLocked(lockedUntil *time.Time, now time.Time) bool {
	return lockedUntil != nil && lockedUntil.After(now)
}
