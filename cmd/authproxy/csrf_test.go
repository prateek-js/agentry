package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef"

// newFormPost builds a form POST carrying the given csrf_token.
// No cookies — the v2 design is stateless on the browser side.
func newFormPost(tok string) *http.Request {
	v := url.Values{}
	if tok != "" {
		v.Set("csrf_token", tok)
	}
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestMintValidateRoundTrip(t *testing.T) {
	tok, err := mintCSRFToken(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(tok, "."); got != 2 {
		t.Fatalf("token should have 3 dot-separated parts, got %q", tok)
	}
	r := newFormPost(tok)
	if err := validateCSRF(r, testSecret); err != nil {
		t.Fatalf("fresh token should validate, got %v", err)
	}
}

func TestValidateRejectsWrongSecret(t *testing.T) {
	tok, _ := mintCSRFToken(testSecret)
	r := newFormPost(tok)
	err := validateCSRF(r, "another-secret-another-secret-xx")
	if !errors.Is(err, errCSRFBadSig) {
		t.Fatalf("want bad-sig, got %v", err)
	}
}

func TestValidateRejectsExpired(t *testing.T) {
	// Hand-mint a token with an expiry in the past, signed correctly.
	nonce := "bm9uY2U" // base64url("nonce")
	exp := time.Now().Add(-time.Minute).Unix()
	tok := fmt.Sprintf("%s.%d.%s", nonce, exp, csrfSign(testSecret, nonce, exp))
	r := newFormPost(tok)
	err := validateCSRF(r, testSecret)
	if !errors.Is(err, errCSRFExpired) {
		t.Fatalf("want expired, got %v", err)
	}
}

func TestValidateRejectsTamperedExpiry(t *testing.T) {
	// Take a valid token and push the expiry forward without
	// re-signing — signature must catch it.
	tok, _ := mintCSRFToken(testSecret)
	parts := strings.Split(tok, ".")
	farFuture := time.Now().Add(100 * time.Hour).Unix()
	tampered := fmt.Sprintf("%s.%d.%s", parts[0], farFuture, parts[2])
	r := newFormPost(tampered)
	err := validateCSRF(r, testSecret)
	if !errors.Is(err, errCSRFBadSig) {
		t.Fatalf("want bad-sig on tampered expiry, got %v", err)
	}
}

func TestValidateRejectsMissingToken(t *testing.T) {
	r := newFormPost("")
	// empty value = field absent from the form's perspective
	err := validateCSRF(r, testSecret)
	if !errors.Is(err, errCSRFMissing) {
		t.Fatalf("want missing, got %v", err)
	}
}

func TestValidateRejectsMalformed(t *testing.T) {
	for _, tok := range []string{
		"no-dots-at-all",
		"one.dot",
		"a.notanumber.c",
		"a.b.c.d",
	} {
		r := newFormPost(tok)
		if err := validateCSRF(r, testSecret); err == nil {
			t.Errorf("token %q should be rejected", tok)
		}
	}
}

// TestTokensSurviveConcurrentRenders pins the property the v1 cookie
// design lacked: two tokens minted independently (think: signup page
// + a favicon-triggered login render) BOTH validate. No shared state
// means no clobbering.
func TestTokensSurviveConcurrentRenders(t *testing.T) {
	tok1, _ := mintCSRFToken(testSecret)
	tok2, _ := mintCSRFToken(testSecret)
	if tok1 == tok2 {
		t.Fatal("two mints produced the same token")
	}
	for i, tok := range []string{tok1, tok2} {
		if err := validateCSRF(newFormPost(tok), testSecret); err != nil {
			t.Errorf("token %d should validate independently: %v", i+1, err)
		}
	}
}
