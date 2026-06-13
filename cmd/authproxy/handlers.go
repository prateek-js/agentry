package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// handlers.go — the auth surface mounted under /auth/*.
//
//   GET  /auth/login         render the login page
//   POST /auth/login         consume credentials → set cookie → 302 home
//   GET  /auth/signup        render the signup page
//   POST /auth/signup        create user → set cookie → 302 home
//   POST /auth/logout        clear cookie → 302 /auth/login
//   GET  /auth/me            JSON view of the current session (for app code)
//
// Everything else falls through to proxy.go.
//
// The handlers DO NOT try to render arbitrary "back to" URLs after
// login — too easy to turn into an open-redirect. We always return to
// "/" and let the app route from there.

type authHandlers struct {
	cfg     *Config
	store   Store
	mailer  Mailer       // nil when no SMTP bound — reset routes stay dark
	limiter *rateLimiter // per-IP throttle on credential endpoints
}

func (h *authHandlers) routes() map[string]http.HandlerFunc {
	// /auth/signin + /auth/register + /auth/signout are aliases for the
	// canonical /auth/login + /auth/signup + /auth/logout paths. LLMs
	// trained on next-auth, lucia, and similar libraries reach for
	// /auth/signin out of muscle memory; the alias means they hit a
	// working page instead of a 404 that derails the iteration.
	routes := map[string]http.HandlerFunc{
		"GET /auth/login":     h.getLogin,
		"POST /auth/login":    h.postLogin,
		"GET /auth/signin":    h.getLogin,
		"POST /auth/signin":   h.postLogin,
		"GET /auth/signup":    h.getSignup,
		"POST /auth/signup":   h.postSignup,
		"GET /auth/register":  h.getSignup,
		"POST /auth/register": h.postSignup,
		"POST /auth/logout":   h.postLogout,
		"POST /auth/signout":  h.postLogout,
		"GET /auth/me":        h.getMe,
		"GET /auth/session":   h.getMe, // next-auth uses /api/auth/session; this is close enough
	}
	// Email-gated routes: only mounted when an SMTP service is bound.
	// Without a mailer there's no way to deliver a reset/verify link, so
	// the routes simply don't exist (a stray GET 404s) — the login page
	// also hides the "Forgot password?" link in that state.
	if h.cfg.EmailEnabled() {
		routes["GET /auth/forgot"] = h.getForgot
		routes["POST /auth/forgot"] = h.postForgot
		routes["GET /auth/reset"] = h.getReset
		routes["POST /auth/reset"] = h.postReset
		routes["GET /auth/verify"] = h.getVerify
	}
	return routes
}

// register attaches the auth handlers + a dispatcher onto a mux. Anything
// not matching /auth/* falls through to `fallthrough_`.
func (h *authHandlers) register(mux *http.ServeMux, fallthroughHandler http.Handler) {
	routes := h.routes()
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if fn, ok := routes[key]; ok {
			fn(w, r)
			return
		}
		// /auth/oauth/<name>/start + /callback live in oauth.go; they
		// register their own paths so this dispatcher only owns the
		// fixed routes above.
		http.NotFound(w, r)
	})
	// /auth/me is documented as the JSON identity probe — same as the
	// header inject, but accessible from JS so frontend-only code can
	// read it without parsing a header.
	mux.Handle("/", fallthroughHandler)
}

// getLogin renders the page with a fresh CSRF token.
func (h *authHandlers) getLogin(w http.ResponseWriter, r *http.Request) {
	if h.alreadyLoggedIn(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	tok, err := mintCSRFToken(h.cfg.Secret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Success banners from the reset/verify round-trips. These are the
	// only query flags we honor — no free-form text reaches the page.
	var success string
	switch {
	case r.URL.Query().Get("reset") == "1":
		success = "Your password was reset. Sign in with your new password."
	case r.URL.Query().Get("verified") == "1":
		success = "Your email is verified. You can sign in now."
	}
	body, err := renderLogin(pageData{
		CSRFToken:  tok,
		Providers:  providerButtons(h.cfg.Providers),
		ShowForgot: h.cfg.EmailEnabled(),
		Success:    success,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeHTML(w, http.StatusOK, body)
}

// postLogin validates CSRF, looks up the user, checks bcrypt, mints
// session.
//
// CSRF model (changed in m2): same-origin is the REAL defense. A
// cross-origin POST is rejected with 403 — a malicious page on
// attacker.com cannot stamp our Host into the Origin header. The
// double-submit token is belt-and-braces: when its cookie and form
// field disagree (which happens to honest users across tabs / back
// button / cookie expiry), we re-render the form with a fresh token
// + a one-line note. The user types their password once more and the
// POST succeeds. No more 403 dead-ends.
func (h *authHandlers) postLogin(w http.ResponseWriter, r *http.Request) {
	if h.rateLimited(w, r) {
		return
	}
	if err := validateSameOrigin(r); err != nil {
		log.Printf("authproxy: same-origin rejected on /auth/login: %v", err)
		http.Error(w, "request rejected (cross-origin POST)", http.StatusForbidden)
		return
	}
	if err := validateCSRF(r, h.cfg.Secret); err != nil {
		log.Printf("authproxy: CSRF advisory on /auth/login: %v (re-rendering form)", err)
		h.loginError(w, r,
			"Your sign-in session expired — please try again.",
			strings.ToLower(strings.TrimSpace(r.FormValue("email"))))
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	password := r.FormValue("password")
	if email == "" || password == "" {
		h.loginError(w, r, "Email and password are required", email)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	user, err := h.store.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.loginError(w, r, "Invalid email or password", email)
			return
		}
		log.Printf("authproxy: GetUserByEmail: %v", err)
		h.loginError(w, r, "Sign-in is temporarily unavailable", email)
		return
	}
	// Account lockout: too many recent failures parks the account for a
	// backoff window. We surface this distinctly (not "invalid
	// password") so a legit user knows to wait — the per-IP rate limit
	// above is what stops an attacker from probing lock state.
	if isLocked(user.LockedUntil, time.Now()) {
		h.loginError(w, r, "Too many failed attempts. Try again in a few minutes.", email)
		return
	}
	if user.PasswordHash == "" {
		// Account was created via OAuth and never set a password. Tell
		// the user which provider so they can use the right button.
		h.loginError(w, r, "This account uses "+providerDisplayName(user.Provider)+" sign-in. Use the button above.", email)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		h.recordLoginFailure(user)
		h.loginError(w, r, "Invalid email or password", email)
		return
	}
	// Optional verification gate — only reachable when SMTP is bound AND
	// the operator opted in. Unverified users get a resend path, not a
	// dead end.
	if h.cfg.RequireVerification && !user.EmailVerified {
		h.sendVerifyEmail(r, user) // best-effort resend
		h.loginError(w, r, "Please verify your email — we just sent you a fresh link.", email)
		return
	}
	// Success: clear the failure counter and mint the session.
	_ = h.store.ResetLoginAttempts(ctx, user.ID)
	if err := setSessionCookie(w, r, SessionPayload{
		UID:      user.ID,
		Email:    user.Email,
		Name:     user.Name,
		Provider: user.Provider,
	}, h.cfg.Secret); err != nil {
		log.Printf("authproxy: setSessionCookie: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// rateLimited enforces the per-IP throttle on a credential endpoint.
// Returns true (and writes a 429) when the caller is over the limit.
func (h *authHandlers) rateLimited(w http.ResponseWriter, r *http.Request) bool {
	if h.limiter == nil || h.limiter.allow(remoteIP(r)) {
		return false
	}
	log.Printf("authproxy: rate-limited %s from ip=%s", r.URL.Path, remoteIP(r))
	w.Header().Set("Retry-After", "60")
	http.Error(w, "Too many requests. Please wait a minute and try again.", http.StatusTooManyRequests)
	return true
}

// recordLoginFailure bumps the account's failure counter and applies the
// shared lockout policy. Best-effort: a DB hiccup here must not block the
// login response (the user already failed; we just couldn't persist it).
func (h *authHandlers) recordLoginFailure(user *User) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attempts := user.FailedAttempts + 1
	until := lockoutUntil(time.Now(), attempts)
	if err := h.store.RecordLoginFailure(ctx, user.ID, attempts, until); err != nil {
		log.Printf("authproxy: RecordLoginFailure: %v", err)
	}
}

func (h *authHandlers) loginError(w http.ResponseWriter, r *http.Request, msg, email string) {
	tok, _ := mintCSRFToken(h.cfg.Secret)
	body, _ := renderLogin(pageData{
		CSRFToken: tok,
		Providers: providerButtons(h.cfg.Providers),
		Error:     msg,
		Email:     email,
	})
	writeHTML(w, http.StatusUnauthorized, body)
}

// getSignup renders the signup page.
func (h *authHandlers) getSignup(w http.ResponseWriter, r *http.Request) {
	if h.alreadyLoggedIn(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	tok, err := mintCSRFToken(h.cfg.Secret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body, err := renderSignup(pageData{
		CSRFToken: tok,
		Providers: providerButtons(h.cfg.Providers),
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeHTML(w, http.StatusOK, body)
}

// postSignup validates CSRF, creates the user, mints session.
// Same CSRF model as postLogin — see the doc there.
func (h *authHandlers) postSignup(w http.ResponseWriter, r *http.Request) {
	if h.rateLimited(w, r) {
		return
	}
	if err := validateSameOrigin(r); err != nil {
		log.Printf("authproxy: same-origin rejected on /auth/signup: %v", err)
		http.Error(w, "request rejected (cross-origin POST)", http.StatusForbidden)
		return
	}
	if err := validateCSRF(r, h.cfg.Secret); err != nil {
		log.Printf("authproxy: CSRF advisory on /auth/signup: %v (re-rendering form)", err)
		h.signupError(w, r,
			"Your sign-up session expired — please try again.",
			strings.ToLower(strings.TrimSpace(r.FormValue("email"))))
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	password := r.FormValue("password")
	name := strings.TrimSpace(r.FormValue("name"))

	if email == "" || password == "" {
		h.signupError(w, r, "Email and password are required", email)
		return
	}
	if len(password) < 8 {
		h.signupError(w, r, "Password must be at least 8 characters", email)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	user, err := h.store.CreateUserPassword(ctx, email, password, name)
	if err != nil {
		if errors.Is(err, ErrEmailTaken) {
			h.signupError(w, r, "An account with that email already exists. Try signing in instead.", email)
			return
		}
		log.Printf("authproxy: CreateUserPassword: %v", err)
		h.signupError(w, r, "Sign-up is temporarily unavailable", email)
		return
	}
	// Verification-required mode: don't auto-login. Send a verify link
	// and show a "check your email" interstitial. The account exists but
	// can't sign in until the link is clicked.
	if h.cfg.RequireVerification {
		h.sendVerifyEmail(r, user)
		writeHTML(w, http.StatusOK, renderNotice(
			"Check your email",
			"We sent a verification link to "+user.Email+". Click it to finish setting up your account, then sign in.",
		))
		return
	}
	if err := setSessionCookie(w, r, SessionPayload{
		UID:      user.ID,
		Email:    user.Email,
		Name:     user.Name,
		Provider: user.Provider,
	}, h.cfg.Secret); err != nil {
		log.Printf("authproxy: setSessionCookie: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *authHandlers) signupError(w http.ResponseWriter, r *http.Request, msg, email string) {
	tok, _ := mintCSRFToken(h.cfg.Secret)
	body, _ := renderSignup(pageData{
		CSRFToken: tok,
		Providers: providerButtons(h.cfg.Providers),
		Error:     msg,
		Email:     email,
	})
	writeHTML(w, http.StatusBadRequest, body)
}

// postLogout clears the session cookie and redirects to login.
// Same-origin is the real check (an attacker.com page can't spoof our
// Origin header). The double-submit token is logged but not enforced
// here — a stale logout link should still log you out, not strand you
// with a 403 you can't recover from.
func (h *authHandlers) postLogout(w http.ResponseWriter, r *http.Request) {
	if err := validateSameOrigin(r); err != nil {
		log.Printf("authproxy: same-origin rejected on /auth/logout: %v", err)
		http.Error(w, "request rejected (cross-origin POST)", http.StatusForbidden)
		return
	}
	if err := validateCSRF(r, h.cfg.Secret); err != nil {
		log.Printf("authproxy: CSRF advisory on /auth/logout (clearing anyway): %v", err)
	}
	clearSessionCookie(w, r)
	http.Redirect(w, r, "/auth/login", http.StatusFound)
}

// getMe returns the session as JSON. The user's frontend can hit
// /auth/me to render the signed-in chrome without needing to parse
// the injected x-forwarded-* headers (which the browser never sees
// anyway). Returns 401 with empty body when there's no session.
func (h *authHandlers) getMe(w http.ResponseWriter, r *http.Request) {
	p := h.readSession(r)
	if p == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"uid":      p.UID,
		"email":    p.Email,
		"name":     p.Name,
		"provider": p.Provider,
	})
}

// alreadyLoggedIn answers "should we skip the login page for this
// request?" — same payload validation as the proxy's session check.
func (h *authHandlers) alreadyLoggedIn(r *http.Request) bool {
	return h.readSession(r) != nil
}

func (h *authHandlers) readSession(r *http.Request) *SessionPayload {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil
	}
	p, err := openSession(c.Value, h.cfg.Secret)
	if err != nil {
		return nil
	}
	return p
}

func writeHTML(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
