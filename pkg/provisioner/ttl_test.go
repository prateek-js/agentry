package provisioner

import (
	"testing"
	"time"
)

func TestExpiresAtFromTTL(t *testing.T) {
	base := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)

	got := expiresAtFromTTL(base, 3600)
	want := "2026-05-12T11:00:00Z"
	if got != want {
		t.Errorf("expiresAt = %q; want %q", got, want)
	}
}

func TestExpiresAtFromTTLNoTTL(t *testing.T) {
	if v := expiresAtFromTTL(time.Now(), 0); v != "" {
		t.Errorf("ttl=0 should yield empty string, got %q", v)
	}
	if v := expiresAtFromTTL(time.Now(), -1); v != "" {
		t.Errorf("negative ttl should yield empty string, got %q", v)
	}
}

func TestParseExpiresAtRoundTrip(t *testing.T) {
	base := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	s := expiresAtFromTTL(base, 60)
	got, err := parseExpiresAt(s)
	if err != nil {
		t.Fatal(err)
	}
	want := base.Add(60 * time.Second).UTC()
	if !got.Equal(want) {
		t.Errorf("round-trip: got %s; want %s", got, want)
	}
}

func TestParseExpiresAtEmpty(t *testing.T) {
	got, err := parseExpiresAt("")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Errorf("empty string should yield zero time, got %s", got)
	}
}

func TestParseExpiresAtInvalid(t *testing.T) {
	if _, err := parseExpiresAt("not-a-time"); err == nil {
		t.Fatal("expected error for malformed input")
	}
}

func TestIsExpired(t *testing.T) {
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"zero time -> never expired", time.Time{}, false},
		{"future -> not expired", now.Add(time.Hour), false},
		{"exactly now -> expired (inclusive)", now, true},
		{"past -> expired", now.Add(-time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExpired(tc.expiresAt, now); got != tc.want {
				t.Errorf("isExpired(%v, now) = %v; want %v", tc.expiresAt, got, tc.want)
			}
		})
	}
}

func TestTTLAnnotations(t *testing.T) {
	if got := ttlAnnotations(time.Now(), 0); got != nil {
		t.Errorf("ttl=0 should yield nil map, got %v", got)
	}
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	got := ttlAnnotations(now, 60)
	if got[AnnotationExpiresAt] != "2026-05-12T10:01:00Z" {
		t.Errorf("annotation expires-at = %q", got[AnnotationExpiresAt])
	}
	if got[AnnotationTTLSec] != "60" {
		t.Errorf("annotation ttl-seconds = %q", got[AnnotationTTLSec])
	}
}
