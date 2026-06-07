package main

import (
	"crypto/subtle"
	"errors"

	"golang.org/x/crypto/argon2"
)

// Password-mode bridge verifier. Mirror of agentry-app's
// internal/passhash/passhash.go Verify side — the bridge only needs
// to check, never to mint.
//
// IMPORTANT: the argon2 cost parameters below MUST match what
// agentry-app uses to hash. If they drift, every password ever set
// becomes unverifiable.
//
// Stored bytes layout (from agentry-app):
//
//	[ 16-byte salt | 32-byte key ]   = 48 bytes total
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
	storedLen    = argonSaltLen + argonKeyLen
)

var errBadHash = errors.New("passverify: malformed hash")

// verifyArgon2id checks `attempt` against the salt+key bytes from DB.
// Returns true only on exact match (constant-time). On a malformed
// hash, returns false + errBadHash so callers can log misconfigured
// routes rather than silently failing-open.
func verifyArgon2id(attempt string, stored []byte) (bool, error) {
	if attempt == "" {
		return false, nil
	}
	if len(stored) != storedLen {
		return false, errBadHash
	}
	salt := stored[:argonSaltLen]
	want := stored[argonSaltLen:]
	got := argon2.IDKey([]byte(attempt), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
