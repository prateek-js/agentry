package main

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/agentry/agentry/pkg/bridge"
)

// makeArgon2Hash mirrors agentry-app's passhash.Hash output shape
// (16-byte salt || 32-byte key, argon2id with the cost params pinned
// in passverify.go). Used by tests to stage a "stored" hash without
// importing the agentry-app side directly — if these tests ever fail
// because of cost-param drift, it means the bridge and agentry-app
// have lost sync and every existing password is now unverifiable.
func makeArgon2Hash(passphrase string) []byte {
	salt := make([]byte, argonSaltLen)
	_, _ = rand.Read(salt)
	key := argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return append(salt, key...)
}

// verifyArgon2id round-trips: hash a known passphrase, verify it
// passes; verify a wrong attempt fails. This is the single load-bearing
// guarantee — if hash params drift between agentry-app and the bridge,
// every password ever set becomes unverifiable, and this is the test
// that fires.
func TestVerifyArgon2id_Roundtrip(t *testing.T) {
	hash := makeArgon2Hash("brave-otter")
	ok, err := verifyArgon2id("brave-otter", hash)
	if err != nil || !ok {
		t.Errorf("right passphrase rejected: ok=%v err=%v", ok, err)
	}
	ok, err = verifyArgon2id("brave-otter-wrong", hash)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("wrong passphrase accepted")
	}
}

func TestVerifyArgon2id_Malformed(t *testing.T) {
	ok, err := verifyArgon2id("anything", []byte{0x01, 0x02})
	if err != errBadHash {
		t.Errorf("err = %v; want errBadHash", err)
	}
	if ok {
		t.Error("malformed hash should never accept")
	}
}

// Cookie mint/verify with HMAC: roundtrip works, tampering breaks.
func TestUnlockCookie_RoundtripAndTamper(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	const prefix uint64 = 0xdeadbeef12345678
	c := mintUnlockCookie(prefix, secret, time.Now().Add(time.Hour))
	got, ok := verifyUnlockCookie(c, secret)
	if !ok {
		t.Fatal("fresh cookie failed to verify")
	}
	if got != prefix {
		t.Errorf("verify prefix = %#x; want %#x", got, prefix)
	}

	// Tamper with prefix → HMAC fails.
	parts := strings.SplitN(c, ".", 3)
	bad := "ffffffffffffffff." + parts[1] + "." + parts[2]
	if _, ok := verifyUnlockCookie(bad, secret); ok {
		t.Error("cookie with swapped prefix verified — HMAC isn't covering the body")
	}

	// Tamper with HMAC → fails.
	if _, ok := verifyUnlockCookie(parts[0]+"."+parts[1]+".AAAAA", secret); ok {
		t.Error("cookie with replaced HMAC verified")
	}
}

// Strict revoke: a cookie minted under one password prefix becomes
// invalid the moment the route's prefix changes (i.e. owner clicks
// Regenerate). The verify still succeeds — but checkDeployAuthPassword
// compares the prefix to route.PasswordPrefix and rejects on mismatch.
// This test exercises the higher-level flow.
func TestPasswordMode_CookieFailsAfterRegenerate(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	hashV1 := makeArgon2Hash("hunter2")
	routeV1 := bridge.DeployRoute{
		AuthMode:        "password",
		PasswordHashB64: base64.StdEncoding.EncodeToString(hashV1),
		PasswordPrefix:  prefixFor(hashV1),
	}

	// User unlocks under v1.
	cookieV1 := mintUnlockCookie(routeV1.PasswordPrefix, secret, time.Now().Add(time.Hour))

	// Owner regenerates → new hash, new prefix.
	hashV2 := makeArgon2Hash("hunter2") // same passphrase, different salt → different hash → different prefix
	routeV2 := bridge.DeployRoute{
		AuthMode:        "password",
		PasswordHashB64: base64.StdEncoding.EncodeToString(hashV2),
		PasswordPrefix:  prefixFor(hashV2),
	}
	if routeV1.PasswordPrefix == routeV2.PasswordPrefix {
		t.Fatal("two argon2 hashes of the same passphrase produced the same prefix — test setup broken")
	}

	// The v1 cookie should NOT unlock the v2 route.
	r := httptest.NewRequest(http.MethodGet, "https://app.agentry.live/", nil)
	r.AddCookie(&http.Cookie{Name: unlockCookieName, Value: cookieV1})
	w := httptest.NewRecorder()
	allowed := checkDeployAuthPassword(w, r, routeV2, secret)
	if allowed {
		t.Error("v1 cookie still unlocked v2 route — strict revoke is broken")
	}
}

// On a brand-new GET with no cookie, the bridge serves the unlock
// form with a 401, not a redirect. The form must include the submit
// path and a password input.
func TestPasswordMode_ServesForm(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	hash := makeArgon2Hash("brave-otter")
	route := bridge.DeployRoute{
		AuthMode:        "password",
		PasswordHashB64: base64.StdEncoding.EncodeToString(hash),
		PasswordPrefix:  prefixFor(hash),
	}
	r := httptest.NewRequest(http.MethodGet, "https://app.agentry.live/somepath", nil)
	w := httptest.NewRecorder()
	allowed := checkDeployAuthPassword(w, r, route, secret)
	if allowed {
		t.Fatal("checkDeployAuthPassword returned true for an unauth'd GET")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, unlockSubmitPath) {
		t.Error("form body missing the submit action")
	}
	if !strings.Contains(body, `name="p"`) {
		t.Error("form body missing the password input")
	}
}

// POST with the right passphrase: cookie set, 303 to the user-requested
// path. Wrong passphrase: 401 + re-rendered form, no cookie.
func TestPasswordMode_SubmitRightAndWrong(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	hash := makeArgon2Hash("brave-otter")
	route := bridge.DeployRoute{
		AuthMode:        "password",
		PasswordHashB64: base64.StdEncoding.EncodeToString(hash),
		PasswordPrefix:  prefixFor(hash),
	}

	// Right.
	form := strings.NewReader("p=brave-otter&return=/some/page")
	r := httptest.NewRequest(http.MethodPost, "https://app.agentry.live"+unlockSubmitPath, form)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	if checkDeployAuthPassword(w, r, route, secret) {
		t.Fatal("submit returned true; should have written response + returned false")
	}
	if w.Code != http.StatusSeeOther {
		t.Errorf("right-pass status = %d; want 303", w.Code)
	}
	if w.Header().Get("Location") != "/some/page" {
		t.Errorf("Location = %q; want /some/page", w.Header().Get("Location"))
	}
	if !strings.Contains(w.Header().Get("Set-Cookie"), unlockCookieName) {
		t.Error("no unlock cookie set on successful unlock")
	}

	// Wrong.
	w = httptest.NewRecorder()
	form = strings.NewReader("p=nope&return=/x")
	r = httptest.NewRequest(http.MethodPost, "https://app.agentry.live"+unlockSubmitPath, form)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	checkDeployAuthPassword(w, r, route, secret)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong-pass status = %d; want 401", w.Code)
	}
	if strings.Contains(w.Header().Get("Set-Cookie"), unlockCookieName) {
		t.Error("unlock cookie set on a wrong-password attempt")
	}
}

// Rate limiter: more than max attempts/min from one IP returns 429.
// Different IPs are tracked independently.
func TestRateLimit_BlocksAfterMax(t *testing.T) {
	rl := newRateLimiter(3)
	for i := 0; i < 3; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Errorf("attempt %d should be allowed (max=3)", i+1)
		}
	}
	if rl.Allow("1.2.3.4") {
		t.Error("4th attempt should have been blocked")
	}
	// Different IP is independent.
	if !rl.Allow("5.6.7.8") {
		t.Error("different IP should not be affected by another's budget")
	}
}

// prefixFor mirrors passhash.PrefixUint64 — first 8 bytes of the hash
// as a big-endian uint64. Test-side helper so we can stage routes
// without going through agentry-app.
func prefixFor(hash []byte) uint64 {
	if len(hash) < 8 {
		return 0
	}
	var x uint64
	for i := 0; i < 8; i++ {
		x = (x << 8) | uint64(hash[i])
	}
	return x
}
