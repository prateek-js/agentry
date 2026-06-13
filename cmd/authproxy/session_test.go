package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSealOpenRoundTrip(t *testing.T) {
	secret := strings.Repeat("a", 32)
	p := SessionPayload{
		UID:      "u1",
		Email:    "x@example.com",
		Name:     "Some One",
		Provider: "password",
		Exp:      time.Now().Add(time.Hour).Unix(),
	}
	val, err := sealSession(p, secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !strings.Contains(val, ".") {
		t.Fatalf("seal output missing separator: %q", val)
	}
	got, err := openSession(val, secret)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got.UID != p.UID || got.Email != p.Email || got.Provider != p.Provider {
		t.Fatalf("payload roundtrip mismatch: got=%+v want=%+v", got, p)
	}
}

func TestSealRejectsEmptySecret(t *testing.T) {
	if _, err := sealSession(SessionPayload{UID: "u"}, ""); err == nil {
		t.Fatal("expected error on empty secret")
	}
}

func TestOpenSessionExpired(t *testing.T) {
	secret := strings.Repeat("k", 32)
	p := SessionPayload{UID: "u", Exp: time.Now().Add(-time.Second).Unix()}
	val, _ := sealSession(p, secret)
	_, err := openSession(val, secret)
	if !errors.Is(err, errSessionExpired) {
		t.Fatalf("want expired, got %v", err)
	}
}

func TestOpenSessionTampered(t *testing.T) {
	secret := strings.Repeat("k", 32)
	p := SessionPayload{UID: "u", Exp: time.Now().Add(time.Hour).Unix()}
	val, _ := sealSession(p, secret)
	// Flip a byte in the body half. The signature won't match.
	parts := strings.SplitN(val, ".", 2)
	if len(parts) != 2 {
		t.Fatal("malformed seal output")
	}
	body := parts[0]
	// Replace the first char with 'A' if it isn't, else 'B'.
	if body[0] == 'A' {
		body = "B" + body[1:]
	} else {
		body = "A" + body[1:]
	}
	_, err := openSession(body+"."+parts[1], secret)
	if !errors.Is(err, errSessionTampered) {
		t.Fatalf("want tampered, got %v", err)
	}
}

func TestOpenSessionWrongSecret(t *testing.T) {
	good := strings.Repeat("a", 32)
	bad := strings.Repeat("b", 32)
	p := SessionPayload{UID: "u", Exp: time.Now().Add(time.Hour).Unix()}
	val, _ := sealSession(p, good)
	_, err := openSession(val, bad)
	if !errors.Is(err, errSessionTampered) {
		t.Fatalf("want tampered, got %v", err)
	}
}

func TestOpenSessionMissing(t *testing.T) {
	_, err := openSession("", "secret")
	if !errors.Is(err, errSessionMissing) {
		t.Fatalf("want missing, got %v", err)
	}
}

func TestOpenSessionMalformed(t *testing.T) {
	for _, v := range []string{
		"no-dot-here",
		"only.one.dot.extra",
		"!!!.invalidbase64",
	} {
		_, err := openSession(v, "secret")
		if err == nil {
			t.Fatalf("expected error for %q", v)
		}
	}
}

func TestSetSessionCookieOverridesExpiry(t *testing.T) {
	secret := strings.Repeat("a", 32)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)

	// Caller passes a wildly future Exp; setSessionCookie should
	// override it to ~30 days from now.
	if err := setSessionCookie(w, r, SessionPayload{UID: "u", Exp: 1<<62}, secret); err != nil {
		t.Fatal(err)
	}
	resp := w.Result()
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("want 1 cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != sessionCookieName {
		t.Fatalf("wrong cookie name: %q", c.Name)
	}
	if !c.HttpOnly {
		t.Fatal("expected HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("expected SameSite=Lax, got %v", c.SameSite)
	}
	if c.Domain != "" {
		t.Fatalf("expected no Domain attribute, got %q", c.Domain)
	}
	if c.MaxAge != int(sessionMaxAge/time.Second) {
		t.Fatalf("expected MaxAge=%d, got %d", int(sessionMaxAge/time.Second), c.MaxAge)
	}
	// Open the sealed value and verify Exp is in the next 30 days, not
	// 1<<62.
	got, err := openSession(c.Value, secret)
	if err != nil {
		t.Fatalf("open returned: %v", err)
	}
	upperBound := time.Now().Add(sessionMaxAge + time.Minute).Unix()
	if got.Exp > upperBound {
		t.Fatalf("Exp not overridden: %d > %d", got.Exp, upperBound)
	}
}

func TestSetSessionCookieSecureFromHTTPS(t *testing.T) {
	secret := strings.Repeat("a", 32)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	if err := setSessionCookie(w, r, SessionPayload{UID: "u"}, secret); err != nil {
		t.Fatal(err)
	}
	c := w.Result().Cookies()[0]
	if !c.Secure {
		t.Fatal("expected Secure=true when X-Forwarded-Proto=https")
	}
}

func TestSetSessionCookieNotSecureFromHTTP(t *testing.T) {
	secret := strings.Repeat("a", 32)
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	if err := setSessionCookie(w, r, SessionPayload{UID: "u"}, secret); err != nil {
		t.Fatal(err)
	}
	c := w.Result().Cookies()[0]
	if c.Secure {
		t.Fatal("expected Secure=false on plain HTTP request")
	}
}

func TestClearSessionCookie(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	clearSessionCookie(w, r)
	c := w.Result().Cookies()[0]
	if c.MaxAge != -1 {
		t.Fatalf("expected MaxAge=-1, got %d", c.MaxAge)
	}
}

func TestRequestIsHTTPS(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(r *http.Request)
		expect bool
	}{
		{
			name:   "x-forwarded-proto https",
			setup:  func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") },
			expect: true,
		},
		{
			name:   "x-forwarded-proto HTTP-S mixed case",
			setup:  func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "HTTPS") },
			expect: true,
		},
		{
			name:   "x-forwarded-proto http",
			setup:  func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") },
			expect: false,
		},
		{
			name:   "no scheme info",
			setup:  func(r *http.Request) {},
			expect: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			tc.setup(r)
			if got := requestIsHTTPS(r); got != tc.expect {
				t.Fatalf("want %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestOriginForRedirect(t *testing.T) {
	r := httptest.NewRequest("GET", "https://app.example.com/x", nil)
	r.Host = "app.example.com"
	r.Header.Set("X-Forwarded-Proto", "https")
	got := originForRedirect(r)
	if got != "https://app.example.com" {
		t.Fatalf("want https://app.example.com, got %q", got)
	}
}

// The signature verification is a security boundary; make sure a
// hand-rolled valid HMAC over arbitrary payload bytes also verifies.
// This catches accidental hash-mode regressions.
func TestSignatureAgreesWithStandardHMAC(t *testing.T) {
	secret := "test-secret"
	body := base64.RawURLEncoding.EncodeToString([]byte(`{"u":"x","x":1}`))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	want := mac.Sum(nil)
	got := hmacSHA256(secret, body)
	if !hmac.Equal(want, got) {
		t.Fatal("hmacSHA256 disagrees with stdlib")
	}
}

func TestPayloadShortKeys(t *testing.T) {
	// Confirm JSON keys are the short ones — the cookie ships on every
	// request, so renaming UID to "user_id" or similar is a perf
	// regression worth catching.
	raw, _ := json.Marshal(SessionPayload{UID: "u", Email: "e", Provider: "password", Exp: 1})
	want := `"u":"u"`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("JSON missing %q: %s", want, raw)
	}
	if strings.Contains(string(raw), `"uid"`) {
		t.Fatal("payload using long key 'uid' — should be 'u'")
	}
}
