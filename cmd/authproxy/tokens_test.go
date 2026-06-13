package main

import (
	"net/http/httptest"
	"testing"
)

func TestHashToken_StableAndDistinct(t *testing.T) {
	a := hashToken("abc")
	if a != hashToken("abc") {
		t.Error("hashToken should be deterministic")
	}
	if a == hashToken("abd") {
		t.Error("different inputs should hash differently")
	}
	if len(a) != 64 { // sha256 hex
		t.Errorf("hash len = %d; want 64", len(a))
	}
}

func TestNewRawToken_UniqueAndLong(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := newRawToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 64 { // 32 bytes hex
			t.Fatalf("token len = %d; want 64", len(tok))
		}
		if seen[tok] {
			t.Fatal("duplicate token from newRawToken")
		}
		seen[tok] = true
	}
}

func TestPublicBaseURL(t *testing.T) {
	// Forwarded headers (the bridge stamps these) win.
	r := httptest.NewRequest("GET", "http://127.0.0.1:3000/auth/forgot", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "app-abc.agentry.live")
	if got := publicBaseURL(r); got != "https://app-abc.agentry.live" {
		t.Errorf("publicBaseURL = %q; want the forwarded host", got)
	}
	// Without forwards, fall back to the request host.
	r2 := httptest.NewRequest("GET", "http://localhost:3000/auth/forgot", nil)
	if got := publicBaseURL(r2); got != "http://localhost:3000" {
		t.Errorf("fallback publicBaseURL = %q", got)
	}
}
