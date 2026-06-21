package main

import (
	"strings"
	"testing"
	"time"
)

func TestPKCEChallengeShape(t *testing.T) {
	v, c, err := mintPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 96 {
		t.Fatalf("verifier length: got %d", len(v))
	}
	if len(c) != 43 {
		// 32 bytes -> 43 chars base64url no-pad.
		t.Fatalf("challenge length: got %d", len(c))
	}
	// Recomputing the challenge from the verifier must match.
	if pkceChallenge(v) != c {
		t.Fatal("challenge is not deterministic from verifier")
	}
}

func TestPKCEChallengeIsStableAcrossCalls(t *testing.T) {
	v := strings.Repeat("a", 96)
	if pkceChallenge(v) != pkceChallenge(v) {
		t.Fatal("pkceChallenge changed across calls for same verifier")
	}
}

func TestSealOpenOAuthState(t *testing.T) {
	secret := strings.Repeat("k", 32)
	st := oauthState{
		Provider:     "google",
		PKCEVerifier: "verify-me",
		Nonce:        "n1",
		Exp:          time.Now().Add(time.Minute).Unix(),
	}
	val, err := sealOAuthState(st, secret)
	if err != nil {
		t.Fatal(err)
	}
	got, err := openOAuthState(val, secret)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != st.Provider || got.PKCEVerifier != st.PKCEVerifier || got.Nonce != st.Nonce {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, st)
	}
}

func TestOAuthStateWrongSecret(t *testing.T) {
	good := strings.Repeat("a", 32)
	bad := strings.Repeat("b", 32)
	st := oauthState{Provider: "google", Nonce: "x", Exp: time.Now().Unix() + 60}
	val, _ := sealOAuthState(st, good)
	if _, err := openOAuthState(val, bad); err == nil {
		t.Fatal("expected signature mismatch")
	}
}

func TestOAuthStateMalformed(t *testing.T) {
	for _, v := range []string{"", "no-dot", "!!!.!!!"} {
		if _, err := openOAuthState(v, "k"); err == nil {
			t.Fatalf("expected error for %q", v)
		}
	}
}

func TestProviderSpecKnown(t *testing.T) {
	cases := []string{"google", "github", "microsoft", "apple"}
	for _, n := range cases {
		s, err := providerSpec(n, ProviderConfig{ClientID: "x", ClientSecret: "y"})
		if err != nil {
			t.Fatalf("%s: unexpected err %v", n, err)
		}
		if s.authorizeURL == "" || s.tokenURL == "" {
			t.Fatalf("%s: incomplete spec %+v", n, s)
		}
	}
}

func TestProviderSpecGenericOIDCNeedsIssuer(t *testing.T) {
	_, err := providerSpec("generic-oidc", ProviderConfig{ClientID: "x", ClientSecret: "y"})
	if err == nil {
		t.Fatal("expected error when GENERIC_OIDC_ISSUER is empty")
	}
	s, err := providerSpec("generic-oidc", ProviderConfig{ClientID: "x", ClientSecret: "y", Issuer: "https://idp.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.authorizeURL, "https://idp.example.com") {
		t.Fatalf("authorize URL not derived from issuer: %q", s.authorizeURL)
	}
}

func TestProviderSpecUnknown(t *testing.T) {
	if _, err := providerSpec("not-a-real-one", ProviderConfig{}); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestProviderDisplayName(t *testing.T) {
	cases := map[string]string{
		"google":       "Google",
		"github":       "GitHub",
		"microsoft":    "Microsoft",
		"apple":        "Apple",
		"generic-oidc": "SSO",
		"custom":       "Custom",
	}
	for k, v := range cases {
		if got := providerDisplayName(k); got != v {
			t.Fatalf("%q -> %q, want %q", k, got, v)
		}
	}
}

func TestCoerceString(t *testing.T) {
	cases := map[any]string{
		"hello":     "hello",
		float64(42): "42",
		int(42):     "42",
		int64(42):   "42",
		true:        "", // unsupported types coerce to ""
		nil:         "",
	}
	for in, want := range cases {
		if got := coerceString(in); got != want {
			t.Fatalf("coerceString(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestRandHex(t *testing.T) {
	a, _ := randHex(16)
	b, _ := randHex(16)
	if len(a) != 32 {
		t.Fatalf("len: %d", len(a))
	}
	if a == b {
		t.Fatal("two randHex(16) calls produced the same string")
	}
}
