package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// recordingMailer captures sends so tests can assert on them without an
// SMTP server.
type recordingMailer struct{ sent []sentMail }
type sentMail struct{ to, subject, html, text string }

func (m *recordingMailer) Send(to, subject, html, text string) error {
	m.sent = append(m.sent, sentMail{to, subject, html, text})
	return nil
}

// emailAuthHandlers builds handlers with the email capability lit + a
// recording mailer + a generous rate limiter.
func emailAuthHandlers(t *testing.T, store *fakeStore) (*authHandlers, *recordingMailer) {
	t.Helper()
	m := &recordingMailer{}
	cfg := &Config{
		Mode:      "agentry",
		Secret:    strings.Repeat("a", 32),
		Providers: map[string]ProviderConfig{},
		Email:     &EmailConfig{Host: "smtp.x", Port: "587", From: "no-reply@x"},
	}
	return &authHandlers{cfg: cfg, store: store, mailer: m, limiter: newRateLimiter(100, time.Minute)}, m
}

// sameOriginForm builds a POST with a valid CSRF token + same-origin
// Origin header so the request clears both gates.
func sameOriginForm(secret, path string, kv map[string]string) *http.Request {
	form := url.Values{}
	tok, _ := mintCSRFToken(secret)
	form.Set("csrf_token", tok)
	for k, v := range kv {
		form.Set(k, v)
	}
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+r.Host)
	return r
}

func TestRoutes_EmailGated(t *testing.T) {
	// No email → forgot/reset routes absent.
	noEmail := newAuthHandlers(t, newFakeStore())
	if _, ok := noEmail.routes()["POST /auth/forgot"]; ok {
		t.Error("forgot route should NOT exist without SMTP bound")
	}
	// Email on → routes present.
	withEmail, _ := emailAuthHandlers(t, newFakeStore())
	for _, key := range []string{"GET /auth/forgot", "POST /auth/forgot", "GET /auth/reset", "POST /auth/reset", "GET /auth/verify"} {
		if _, ok := withEmail.routes()[key]; !ok {
			t.Errorf("route %q should exist with SMTP bound", key)
		}
	}
}

func TestLoginPage_ForgotLinkOnlyWhenEmailOn(t *testing.T) {
	noEmail := newAuthHandlers(t, newFakeStore())
	w := httptest.NewRecorder()
	noEmail.getLogin(w, httptest.NewRequest("GET", "/auth/login", nil))
	if strings.Contains(w.Body.String(), "/auth/forgot") {
		t.Error("login page should NOT show Forgot link without email")
	}
	withEmail, _ := emailAuthHandlers(t, newFakeStore())
	w2 := httptest.NewRecorder()
	withEmail.getLogin(w2, httptest.NewRequest("GET", "/auth/login", nil))
	if !strings.Contains(w2.Body.String(), "/auth/forgot") {
		t.Error("login page SHOULD show Forgot link with email on")
	}
}

func TestForgot_EnumerationSafe(t *testing.T) {
	store := newFakeStore()
	_, _ = store.CreateUserPassword(context.Background(), "real@x.com", "longenough", "")
	h, mailer := emailAuthHandlers(t, store)

	// Existing user → same notice AND a mail is sent.
	w := httptest.NewRecorder()
	h.postForgot(w, sameOriginForm(h.cfg.Secret, "/auth/forgot", map[string]string{"email": "real@x.com"}))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "sent a password reset link") {
		t.Fatalf("existing-user forgot: code=%d body=%s", w.Code, body)
	}
	if len(mailer.sent) != 1 || mailer.sent[0].to != "real@x.com" {
		t.Fatalf("expected one reset mail to real@x.com; got %+v", mailer.sent)
	}

	// Non-existent user → IDENTICAL notice, but NO mail.
	w2 := httptest.NewRecorder()
	h.postForgot(w2, sameOriginForm(h.cfg.Secret, "/auth/forgot", map[string]string{"email": "ghost@x.com"}))
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "sent a password reset link") {
		t.Errorf("ghost forgot should look identical; code=%d", w2.Code)
	}
	if len(mailer.sent) != 1 {
		t.Errorf("no mail should be sent for an unknown address; total sent=%d", len(mailer.sent))
	}
}

func TestReset_HappyPath_SingleUse(t *testing.T) {
	store := newFakeStore()
	u, _ := store.CreateUserPassword(context.Background(), "u@x.com", "oldpassword", "")
	h := &authHandlers{cfg: &Config{Mode: "agentry", Secret: strings.Repeat("a", 32),
		Email: &EmailConfig{Host: "x", From: "f"}}, store: store, limiter: newRateLimiter(100, time.Minute)}

	raw, _ := newRawToken()
	_ = store.CreateEmailToken(context.Background(), u.ID, purposeReset, hashToken(raw), time.Now().Add(time.Hour))

	r := sameOriginForm(h.cfg.Secret, "/auth/reset", map[string]string{
		"token": raw, "password": "brand-new-pw", "confirm": "brand-new-pw",
	})
	w := httptest.NewRecorder()
	h.postReset(w, r)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/auth/login?reset=1" {
		t.Fatalf("reset should 302 to login?reset=1; code=%d loc=%s", w.Code, w.Header().Get("Location"))
	}
	// Password actually changed.
	got, _ := store.GetUserByEmail(context.Background(), "u@x.com")
	if got.PasswordHash != "h:brand-new-pw" {
		t.Errorf("password not updated; hash=%q", got.PasswordHash)
	}
	// Token is single-use: replaying the same link fails.
	w2 := httptest.NewRecorder()
	h.postReset(w2, sameOriginForm(h.cfg.Secret, "/auth/reset", map[string]string{
		"token": raw, "password": "another-new-pw", "confirm": "another-new-pw",
	}))
	if !strings.Contains(w2.Body.String(), "invalid or has already been used") {
		t.Errorf("replayed token should be rejected; body=%s", w2.Body.String())
	}
}

func TestReset_Rejections(t *testing.T) {
	store := newFakeStore()
	u, _ := store.CreateUserPassword(context.Background(), "u@x.com", "oldpassword", "")
	h := &authHandlers{cfg: &Config{Mode: "agentry", Secret: strings.Repeat("a", 32),
		Email: &EmailConfig{Host: "x", From: "f"}}, store: store, limiter: newRateLimiter(100, time.Minute)}
	raw, _ := newRawToken()
	_ = store.CreateEmailToken(context.Background(), u.ID, purposeReset, hashToken(raw), time.Now().Add(time.Hour))

	// Mismatched confirmation.
	w := httptest.NewRecorder()
	h.postReset(w, sameOriginForm(h.cfg.Secret, "/auth/reset", map[string]string{
		"token": raw, "password": "longenough1", "confirm": "different22"}))
	if !strings.Contains(w.Body.String(), "two passwords") {
		t.Errorf("mismatch should be flagged; body=%s", w.Body.String())
	}
	// Weak password.
	w2 := httptest.NewRecorder()
	h.postReset(w2, sameOriginForm(h.cfg.Secret, "/auth/reset", map[string]string{
		"token": raw, "password": "password", "confirm": "password"}))
	if !strings.Contains(w2.Body.String(), "too common") {
		t.Errorf("weak password should be rejected; body=%s", w2.Body.String())
	}
	// Bogus token.
	w3 := httptest.NewRecorder()
	h.postReset(w3, sameOriginForm(h.cfg.Secret, "/auth/reset", map[string]string{
		"token": "deadbeef", "password": "longenough1", "confirm": "longenough1"}))
	if !strings.Contains(w3.Body.String(), "invalid or has already been used") {
		t.Errorf("bogus token should be rejected; body=%s", w3.Body.String())
	}
}

func TestVerify_MarksVerified(t *testing.T) {
	store := newFakeStore()
	u, _ := store.CreateUserPassword(context.Background(), "u@x.com", "oldpassword", "")
	h, _ := emailAuthHandlers(t, store)
	raw, _ := newRawToken()
	_ = store.CreateEmailToken(context.Background(), u.ID, purposeVerify, hashToken(raw), time.Now().Add(time.Hour))

	r := httptest.NewRequest("GET", "/auth/verify?token="+raw, nil)
	w := httptest.NewRecorder()
	h.getVerify(w, r)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/auth/login?verified=1" {
		t.Fatalf("verify should 302 to login?verified=1; code=%d", w.Code)
	}
	got, _ := store.GetUserByEmail(context.Background(), "u@x.com")
	if !got.EmailVerified {
		t.Error("user should be marked verified")
	}
	// Invalid token → notice, not a redirect.
	w2 := httptest.NewRecorder()
	h.getVerify(w2, httptest.NewRequest("GET", "/auth/verify?token=nope", nil))
	if w2.Code == http.StatusFound {
		t.Error("invalid verify token should not redirect to success")
	}
}

func TestLogin_LockoutAfterRepeatedFailures(t *testing.T) {
	store := newFakeStore()
	userWithRealBcryptHash(t, store, "u@x.com", "correct-password")
	h := newAuthHandlers(t, store)
	h.limiter = newRateLimiter(0, time.Minute) // disable rate limit; isolate lockout

	bad := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.postLogin(w, sameOriginForm(h.cfg.Secret, "/auth/login", map[string]string{
			"email": "u@x.com", "password": "wrong-password"}))
		return w
	}
	for i := 0; i < lockoutThreshold; i++ {
		bad()
	}
	// Now even the CORRECT password is refused while locked.
	w := httptest.NewRecorder()
	h.postLogin(w, sameOriginForm(h.cfg.Secret, "/auth/login", map[string]string{
		"email": "u@x.com", "password": "correct-password"}))
	if !strings.Contains(w.Body.String(), "Too many failed attempts") {
		t.Errorf("account should be locked after %d failures; body=%s", lockoutThreshold, w.Body.String())
	}
}

func TestLogin_RateLimited(t *testing.T) {
	h := newAuthHandlers(t, newFakeStore())
	h.limiter = newRateLimiter(2, time.Minute)
	hit := func() int {
		w := httptest.NewRecorder()
		h.postLogin(w, sameOriginForm(h.cfg.Secret, "/auth/login", map[string]string{
			"email": "u@x.com", "password": "whatever12"}))
		return w.Code
	}
	hit()
	hit()
	if code := hit(); code != http.StatusTooManyRequests {
		t.Errorf("3rd attempt should be 429; got %d", code)
	}
}

// Belt: a successful login clears the failure counter so a later typo
// doesn't compound across sessions.
func TestLogin_SuccessResetsFailures(t *testing.T) {
	store := newFakeStore()
	userWithRealBcryptHash(t, store, "u@x.com", "correct-password")
	h := newAuthHandlers(t, store)
	h.limiter = newRateLimiter(0, time.Minute)

	// Two failures, then a success.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.postLogin(w, sameOriginForm(h.cfg.Secret, "/auth/login", map[string]string{
			"email": "u@x.com", "password": "wrong"}))
	}
	w := httptest.NewRecorder()
	h.postLogin(w, sameOriginForm(h.cfg.Secret, "/auth/login", map[string]string{
		"email": "u@x.com", "password": "correct-password"}))
	if w.Code != http.StatusFound {
		t.Fatalf("correct password should log in; code=%d", w.Code)
	}
	got, _ := store.GetUserByEmail(context.Background(), "u@x.com")
	if got.FailedAttempts != 0 {
		t.Errorf("successful login should reset failed_attempts; got %d", got.FailedAttempts)
	}
	_ = io.Discard
}
