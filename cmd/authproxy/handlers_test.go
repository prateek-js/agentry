package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// userWithRealBcryptHash is the one place we use a real bcrypt — the
// fakeStore stubs hashes as "h:<password>", which is fine for tests
// that don't exercise the verify path. postLogin uses
// bcrypt.CompareHashAndPassword so the password-login test needs a
// real hash.
func userWithRealBcryptHash(t *testing.T, store *fakeStore, email, password string) *User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.CreateUserPassword(context.Background(), email, "ignoreplaceholder", "")
	if err != nil {
		t.Fatal(err)
	}
	u.PasswordHash = string(hash)
	return u
}

func TestRoutesAliasesPresent(t *testing.T) {
	// Pin the alias contract: LLMs reach for /auth/signin (next-auth
	// pattern); they shouldn't 404 on it. Same for /auth/register and
	// /auth/signout. The implementations are the same handlers as the
	// canonical /auth/login + /auth/signup + /auth/logout — alias
	// means "renders the same page" so future-me doesn't accidentally
	// split them and let one drift.
	h := newAuthHandlers(t, newFakeStore())
	r := h.routes()
	wants := map[string]http.HandlerFunc{
		"GET /auth/signin":    h.getLogin,
		"POST /auth/signin":   h.postLogin,
		"GET /auth/register":  h.getSignup,
		"POST /auth/register": h.postSignup,
		"POST /auth/signout":  h.postLogout,
		"GET /auth/session":   h.getMe,
	}
	for key := range wants {
		if _, ok := r[key]; !ok {
			t.Errorf("missing route %q in alias set", key)
		}
	}
}

func newAuthHandlers(t *testing.T, store *fakeStore) *authHandlers {
	t.Helper()
	cfg := &Config{
		Mode:      "agentry",
		Secret:    strings.Repeat("a", 32),
		Providers: map[string]ProviderConfig{},
	}
	return &authHandlers{cfg: cfg, store: store}
}

func TestGetLoginRendersForm(t *testing.T) {
	h := newAuthHandlers(t, newFakeStore())
	r := httptest.NewRequest("GET", "/auth/login", nil)
	w := httptest.NewRecorder()
	h.getLogin(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), `name="csrf_token"`) {
		t.Fatal("missing csrf_token field")
	}
	if !strings.Contains(string(body), "Sign in") {
		t.Fatal("missing title")
	}
}

func TestPostLoginCrossOrigin403(t *testing.T) {
	// A POST from a different origin (no Origin matching Host) must be
	// 403'd. This is the REAL CSRF defense — a malicious attacker.com
	// page can't spoof our Host into Origin, so it can't pass this gate.
	h := newAuthHandlers(t, newFakeStore())
	form := url.Values{
		"email":    {"x@y.com"},
		"password": {"longenough"},
	}
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://attacker.com")
	w := httptest.NewRecorder()
	h.postLogin(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST should 403, got %d", w.Code)
	}
}

func TestPostLoginCSRFMismatchRendersForm(t *testing.T) {
	// CSRF token mismatch (same-origin POST, but cookie/token desync
	// from tabs / back button / cookie expiry) must NOT 403 — it must
	// re-render the login form with a fresh token + a one-line note.
	// This is the m2 fix for the "request rejected" UX dead-end.
	h := newAuthHandlers(t, newFakeStore())
	form := url.Values{
		"csrf_token": {"form-token"},
		"email":      {"x@y.com"},
		"password":   {"longenough"},
	}
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+r.Host)
	w := httptest.NewRecorder()
	h.postLogin(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("CSRF mismatch should re-render with 401, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "session expired") {
		t.Errorf("expected 'session expired' note in re-rendered form; body=%s", body)
	}
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Error("re-rendered form should include a fresh csrf_token field")
	}
}

func TestPostLoginMissingCookieRendersForm(t *testing.T) {
	// Same as above but the cookie is entirely absent (the original
	// "CSRF cookie missing" 403 dead-end). Now: same-origin gate
	// passes, CSRF advisory fires, form re-renders.
	h := newAuthHandlers(t, newFakeStore())
	form := url.Values{
		"csrf_token": {"x"},
		"email":      {"x@y.com"},
		"password":   {"longenough"},
	}
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+r.Host)
	// no cookie attached.
	w := httptest.NewRecorder()
	h.postLogin(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing cookie should re-render, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "session expired") {
		t.Error("expected 'session expired' note")
	}
}

func TestPostLoginUnknownEmail(t *testing.T) {
	h := newAuthHandlers(t, newFakeStore())
	r, _ := postLoginRequest(t, "absent@x.com", "longenough")
	w := httptest.NewRecorder()
	h.postLogin(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "Invalid email or password") {
		t.Fatal("missing standard error string")
	}
}

func TestPostLoginWrongPassword(t *testing.T) {
	store := newFakeStore()
	userWithRealBcryptHash(t, store, "real@x.com", "correct-password")
	h := newAuthHandlers(t, store)

	r, _ := postLoginRequest(t, "real@x.com", "wrong-password")
	w := httptest.NewRecorder()
	h.postLogin(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestPostLoginHappyPath(t *testing.T) {
	store := newFakeStore()
	u := userWithRealBcryptHash(t, store, "real@x.com", "correct-password")
	h := newAuthHandlers(t, store)

	r, _ := postLoginRequest(t, "real@x.com", "correct-password")
	w := httptest.NewRecorder()
	h.postLogin(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("expected redirect to /, got %q", loc)
	}
	// Verify the session cookie is opened-back-correctly.
	var sessVal string
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			sessVal = c.Value
		}
	}
	if sessVal == "" {
		t.Fatal("no session cookie set")
	}
	p, err := openSession(sessVal, h.cfg.Secret)
	if err != nil {
		t.Fatalf("session open: %v", err)
	}
	if p.UID != u.ID || p.Email != u.Email {
		t.Fatalf("session payload doesn't match user: %+v vs %+v", p, u)
	}
}

func TestPostSignupHappyPath(t *testing.T) {
	store := newFakeStore()
	h := newAuthHandlers(t, store)
	r, _ := postSignupRequest(t, "new@x.com", "longenough123", "New Person")
	w := httptest.NewRecorder()
	h.postSignup(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	// Verify user landed in store.
	u, err := store.GetUserByEmail(context.Background(), "new@x.com")
	if err != nil {
		t.Fatalf("user not created: %v", err)
	}
	if u.Name != "New Person" {
		t.Fatalf("name wrong: %q", u.Name)
	}
}

func TestPostSignupShortPassword(t *testing.T) {
	h := newAuthHandlers(t, newFakeStore())
	r, _ := postSignupRequest(t, "new@x.com", "short", "")
	w := httptest.NewRecorder()
	h.postSignup(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestPostSignupDuplicateEmail(t *testing.T) {
	store := newFakeStore()
	_, _ = store.CreateUserPassword(context.Background(), "taken@x.com", "longenough", "")
	h := newAuthHandlers(t, store)
	r, _ := postSignupRequest(t, "taken@x.com", "longenough", "")
	w := httptest.NewRecorder()
	h.postSignup(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "already exists") {
		t.Fatal("missing duplicate-email error string")
	}
}

func TestPostLogoutClearsCookie(t *testing.T) {
	h := newAuthHandlers(t, newFakeStore())
	tok, _ := mintCSRFToken(strings.Repeat("a", 32))
	v := url.Values{"csrf_token": {tok}}
	r := httptest.NewRequest("POST", "/auth/logout", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+r.Host)
	w := httptest.NewRecorder()
	h.postLogout(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	var sessClear bool
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge == -1 {
			sessClear = true
		}
	}
	if !sessClear {
		t.Fatal("session cookie not cleared")
	}
}

func TestGetMeReturnsJSONForActiveSession(t *testing.T) {
	store := newFakeStore()
	h := newAuthHandlers(t, store)
	p := SessionPayload{UID: "u1", Email: "x@y.com", Name: "X", Provider: "password"}
	val, _ := sealSession(p, h.cfg.Secret)

	r := httptest.NewRequest("GET", "/auth/me", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: val})
	w := httptest.NewRecorder()
	h.getMe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: %q", ct)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), `"x@y.com"`) {
		t.Fatalf("missing email in body: %s", body)
	}
}

func TestGetMeReturns401WithoutSession(t *testing.T) {
	h := newAuthHandlers(t, newFakeStore())
	r := httptest.NewRequest("GET", "/auth/me", nil)
	w := httptest.NewRecorder()
	h.getMe(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestGetLoginRedirectsWhenAlreadyAuthed(t *testing.T) {
	h := newAuthHandlers(t, newFakeStore())
	p := SessionPayload{UID: "u", Email: "x@y.com", Provider: "password"}
	val, _ := sealSession(p, h.cfg.Secret)
	r := httptest.NewRequest("GET", "/auth/login", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: val})
	w := httptest.NewRecorder()
	h.getLogin(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
}

func postLoginRequest(t *testing.T, email, password string) (*http.Request, string) {
	t.Helper()
	tok, _ := mintCSRFToken(strings.Repeat("a", 32))
	v := url.Values{
		"csrf_token": {tok},
		"email":      {email},
		"password":   {password},
	}
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Mimic a real browser: stamp Origin to match Host. Without this
	// the same-origin check (m2 Bug C durable fix) 403s the request
	// before the CSRF / business logic runs.
	r.Header.Set("Origin", "http://"+r.Host)
	return r, tok
}

func postSignupRequest(t *testing.T, email, password, name string) (*http.Request, string) {
	t.Helper()
	tok, _ := mintCSRFToken(strings.Repeat("a", 32))
	v := url.Values{
		"csrf_token": {tok},
		"email":      {email},
		"password":   {password},
		"name":       {name},
	}
	r := httptest.NewRequest("POST", "/auth/signup", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+r.Host)
	return r, tok
}
