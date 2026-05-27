package provisioner

import (
	"fmt"
	"time"
)

// Pod annotations used to track sandbox TTL state.
//
// expires-at carries the absolute deadline (RFC3339 UTC); ttl-seconds carries
// the original duration so renew-without-body can extend by the same amount.
const (
	AnnotationExpiresAt = "ad-sandbox.io/expires-at"
	AnnotationTTLSec    = "ad-sandbox.io/ttl-seconds"
)

// expiresAtFromTTL returns the RFC3339-UTC deadline for a sandbox created
// `now` with a TTL of `ttlSeconds`. A non-positive ttlSeconds returns "" to
// signal "no TTL" — callers should not set the annotation.
func expiresAtFromTTL(now time.Time, ttlSeconds int64) string {
	if ttlSeconds <= 0 {
		return ""
	}
	return now.UTC().Add(time.Duration(ttlSeconds) * time.Second).Format(time.RFC3339)
}

// parseExpiresAt parses an annotation value back into a time.Time. Empty
// input returns a zero Time and no error — callers treat that as "no TTL".
func parseExpiresAt(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid expires-at %q: %w", v, err)
	}
	return t.UTC(), nil
}

// isExpired reports whether `expiresAt` is set and in the past relative to
// `now`. A zero `expiresAt` (no TTL) is never expired.
func isExpired(expiresAt, now time.Time) bool {
	if expiresAt.IsZero() {
		return false
	}
	return !now.Before(expiresAt)
}

// ttlAnnotations builds the annotation map for a freshly created sandbox.
// Returns nil when no TTL is requested so callers can skip setting any
// annotations at all.
func ttlAnnotations(now time.Time, ttlSeconds int64) map[string]string {
	if ttlSeconds <= 0 {
		return nil
	}
	return map[string]string{
		AnnotationExpiresAt: expiresAtFromTTL(now, ttlSeconds),
		AnnotationTTLSec:    fmt.Sprintf("%d", ttlSeconds),
	}
}
