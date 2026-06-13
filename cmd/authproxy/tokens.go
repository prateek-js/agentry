package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// tokens.go — single-use email tokens for password reset + verification.
//
// Shape: the raw token (32 random bytes, hex) travels in the emailed
// link; we persist only its SHA-256 hash. A leaked DB therefore can't be
// used to forge a live link, and the hash is a fixed-width primary key
// the store can match exactly. Single-use + TTL are enforced in the
// store (ConsumeEmailToken).

const (
	purposeReset  = "reset"
	purposeVerify = "verify"

	resetTokenTTL  = 1 * time.Hour
	verifyTokenTTL = 24 * time.Hour
)

// newRawToken returns 32 hex-less... 32 bytes of randomness hex-encoded
// (64 chars, 256 bits). Unguessable; the security of the whole reset
// flow rests on this being from crypto/rand.
func newRawToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// hashToken is the at-rest form: SHA-256 of the raw token, hex-encoded.
// SHA-256 (not bcrypt) is correct here — the input already has 256 bits
// of entropy, so there's nothing to brute-force; we just need a stable,
// fixed-width, non-reversible key.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// publicBaseURL reconstructs the app's externally-visible origin from
// the headers the bridge stamps (X-Forwarded-Proto + X-Forwarded-Host).
// Email links MUST be absolute + use the public host, never the internal
// listen addr. Falls back to the request Host when the forwards are
// absent (e.g. local dev hitting the sidecar directly).
func publicBaseURL(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return proto + "://" + strings.TrimRight(host, "/")
}
