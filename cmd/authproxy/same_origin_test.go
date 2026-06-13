package main

import (
	"errors"
	"net/http/httptest"
	"testing"
)

func TestValidateSameOrigin(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		origin  string
		referer string
		want    error // nil = pass; sentinel otherwise
	}{
		{
			name:   "Origin matches Host — pass",
			host:   "dvd-rental.preview.agentry.live",
			origin: "https://dvd-rental.preview.agentry.live",
		},
		{
			name:   "Origin matches Host case-insensitive",
			host:   "DVD-RENTAL.preview.agentry.live",
			origin: "https://dvd-rental.preview.agentry.live",
		},
		{
			name:   "Origin different host — reject",
			host:   "myapp.agentry.live",
			origin: "https://attacker.com",
			want:   errOriginMismatch,
		},
		{
			name:    "Origin missing, Referer matches — pass",
			host:    "myapp.agentry.live",
			referer: "https://myapp.agentry.live/auth/login",
		},
		{
			name:    "Origin missing, Referer different host — reject",
			host:    "myapp.agentry.live",
			referer: "https://attacker.com/page",
			want:    errOriginMismatch,
		},
		{
			name: "no Origin, no Referer — defer to token",
			host: "myapp.agentry.live",
			want: errNoOriginOrReferer,
		},
		{
			name:   "Origin = 'null' — treat as missing, fall through",
			host:   "myapp.agentry.live",
			origin: "null",
			want:   errNoOriginOrReferer,
		},
		{
			name:    "Origin = 'null' + Referer matches — pass via Referer",
			host:    "myapp.agentry.live",
			origin:  "null",
			referer: "https://myapp.agentry.live/",
		},
		{
			name:   "Origin malformed — reject (defensive)",
			host:   "myapp.agentry.live",
			origin: "not a url at all",
			want:   errOriginMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+"_xforwardedhost", func(t *testing.T) {
			// Repeat every case via X-Forwarded-Host: r.Host is the
			// runtime's loopback target, X-Forwarded-Host carries the
			// real public host. Same expected outcome.
			r := httptest.NewRequest("POST", "/auth/login", nil)
			r.Host = "127.0.0.1:3000"
			r.Header.Set("X-Forwarded-Host", tc.host)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			got := validateSameOrigin(r)
			if tc.want == nil && got != nil {
				t.Fatalf("want nil, got %v", got)
			}
			if tc.want != nil && !errors.Is(got, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestPublicHostPrefersXForwardedHost(t *testing.T) {
	r := httptest.NewRequest("POST", "/auth/login", nil)
	r.Host = "127.0.0.1:3000"
	r.Header.Set("X-Forwarded-Host", "my-preview.agentry.live")
	if got := publicHost(r); got != "my-preview.agentry.live" {
		t.Fatalf("got %q, want X-Forwarded-Host", got)
	}
}

func TestPublicHostFallsBackToHost(t *testing.T) {
	r := httptest.NewRequest("POST", "/auth/login", nil)
	r.Host = "raw-host.example.com"
	if got := publicHost(r); got != "raw-host.example.com" {
		t.Fatalf("got %q, want fallback to Host", got)
	}
}

// TestValidateSameOriginBridgeChain pins the real-world fix: when the
// request arrives at authproxy via bridge → runtime → app_proxy, the
// runtime rewrites Host to 127.0.0.1:3000 but the bridge stamps
// X-Forwarded-Host with the public hostname. validateSameOrigin must
// honor X-Forwarded-Host so it doesn't 403 every legit signup.
func TestValidateSameOriginBridgeChain(t *testing.T) {
	r := httptest.NewRequest("POST", "/auth/signup", nil)
	r.Host = "127.0.0.1:3000" // what the runtime app_proxy rewrites Host to
	r.Header.Set("X-Forwarded-Host", "akira-dvd-rental.agentry.live")
	r.Header.Set("Origin", "https://akira-dvd-rental.agentry.live")
	if err := validateSameOrigin(r); err != nil {
		t.Fatalf("bridge-chain request should pass, got %v", err)
	}
}

func TestValidateSameOriginBridgeChainMismatch(t *testing.T) {
	// Same setup but Origin doesn't match X-Forwarded-Host. Still 403.
	r := httptest.NewRequest("POST", "/auth/signup", nil)
	r.Host = "127.0.0.1:3000"
	r.Header.Set("X-Forwarded-Host", "akira-dvd-rental.agentry.live")
	r.Header.Set("Origin", "https://attacker.com")
	if err := validateSameOrigin(r); err == nil {
		t.Fatal("Origin mismatching X-Forwarded-Host must be rejected")
	}
}
