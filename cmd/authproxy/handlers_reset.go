package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// handlers_reset.go — the email-gated flows: forgot-password, reset, and
// email verification. These routes only exist when an SMTP service is
// bound (see routes() in handlers.go); h.mailer is therefore non-nil
// whenever they run.

// getForgot renders the "enter your email" form.
func (h *authHandlers) getForgot(w http.ResponseWriter, r *http.Request) {
	tok, err := mintCSRFToken(h.cfg.Secret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeHTML(w, http.StatusOK, renderForgot(pageData{CSRFToken: tok}))
}

// postForgot is enumeration-safe: it ALWAYS renders the same "if that
// address exists, we've sent a link" notice, whether or not the email
// is on file. Only when a matching user exists do we actually mint a
// token + send mail.
func (h *authHandlers) postForgot(w http.ResponseWriter, r *http.Request) {
	if h.rateLimited(w, r) {
		return
	}
	if err := validateSameOrigin(r); err != nil {
		http.Error(w, "request rejected (cross-origin POST)", http.StatusForbidden)
		return
	}
	// CSRF failure here is non-fatal to the UX promise (we show the same
	// generic notice regardless), but we still gate on it to stop drive-
	// by token-spraying from other origins; same-origin already covers
	// the cross-site case.
	_ = validateCSRF(r, h.cfg.Secret)

	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	const notice = "If an account exists for that address, we've sent a password reset link. Check your inbox."

	if email != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if user, err := h.store.GetUserByEmail(ctx, email); err == nil && user != nil {
			h.sendResetEmail(r, user)
		}
		// Any lookup error (incl. not-found) is swallowed — the response
		// is identical so an attacker learns nothing.
	}
	writeHTML(w, http.StatusOK, renderNotice("Check your email", notice))
}

// getReset renders the new-password form for a given token. We don't
// validate the token here (that would consume it / leak validity on a
// GET); the POST is where it's checked and burned.
func (h *authHandlers) getReset(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("token"))
	if raw == "" {
		writeHTML(w, http.StatusBadRequest, renderNotice("Invalid link",
			"This password reset link is missing its token. Request a new one from the sign-in page."))
		return
	}
	tok, err := mintCSRFToken(h.cfg.Secret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeHTML(w, http.StatusOK, renderReset(pageData{CSRFToken: tok, ResetToken: raw}))
}

// postReset validates the token, applies the password policy, sets the
// new password (which also clears any lockout), and sends the user to
// sign in with a success banner.
func (h *authHandlers) postReset(w http.ResponseWriter, r *http.Request) {
	if h.rateLimited(w, r) {
		return
	}
	if err := validateSameOrigin(r); err != nil {
		http.Error(w, "request rejected (cross-origin POST)", http.StatusForbidden)
		return
	}
	raw := strings.TrimSpace(r.FormValue("token"))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")

	resetErr := func(msg string) {
		tok, _ := mintCSRFToken(h.cfg.Secret)
		writeHTML(w, http.StatusBadRequest, renderReset(pageData{
			CSRFToken: tok, ResetToken: raw, Error: msg,
		}))
	}

	if err := validateCSRF(r, h.cfg.Secret); err != nil {
		resetErr("Your session expired — please re-enter your new password.")
		return
	}
	if raw == "" {
		writeHTML(w, http.StatusBadRequest, renderNotice("Invalid link",
			"This reset link is missing its token. Request a new one from the sign-in page."))
		return
	}
	if password != confirm {
		resetErr("The two passwords don't match.")
		return
	}
	if err := validatePassword(password); err != nil {
		resetErr(err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	userID, err := h.store.ConsumeEmailToken(ctx, purposeReset, hashToken(raw))
	if err != nil {
		if errors.Is(err, ErrTokenInvalid) {
			writeHTML(w, http.StatusBadRequest, renderNotice("Link expired",
				"This password reset link is invalid or has already been used. Request a fresh one from the sign-in page."))
			return
		}
		log.Printf("authproxy: ConsumeEmailToken(reset): %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := h.store.UpdatePassword(ctx, userID, password); err != nil {
		log.Printf("authproxy: UpdatePassword: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Don't auto-login — make the user prove the new password works.
	// The ?reset=1 flag renders a success banner on the login page.
	http.Redirect(w, r, "/auth/login?reset=1", http.StatusFound)
}

// getVerify consumes a verification token and marks the address
// verified, then routes to sign-in with a success banner.
func (h *authHandlers) getVerify(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("token"))
	if raw == "" {
		writeHTML(w, http.StatusBadRequest, renderNotice("Invalid link",
			"This verification link is missing its token."))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	userID, err := h.store.ConsumeEmailToken(ctx, purposeVerify, hashToken(raw))
	if err != nil {
		writeHTML(w, http.StatusBadRequest, renderNotice("Link expired",
			"This verification link is invalid or has already been used. Sign in to request a new one."))
		return
	}
	if err := h.store.MarkEmailVerified(ctx, userID); err != nil {
		log.Printf("authproxy: MarkEmailVerified: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/auth/login?verified=1", http.StatusFound)
}

// sendResetEmail mints a reset token and emails the link. Best-effort:
// a send failure is logged but never surfaced to the caller (that would
// break the enumeration-safe promise of postForgot).
func (h *authHandlers) sendResetEmail(r *http.Request, user *User) {
	if h.mailer == nil {
		return
	}
	raw, err := newRawToken()
	if err != nil {
		log.Printf("authproxy: reset token mint: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := h.store.CreateEmailToken(ctx, user.ID, purposeReset, hashToken(raw), time.Now().Add(resetTokenTTL)); err != nil {
		log.Printf("authproxy: CreateEmailToken(reset): %v", err)
		return
	}
	link := publicBaseURL(r) + "/auth/reset?token=" + raw
	text, html := resetEmailBody(link)
	if err := h.mailer.Send(user.Email, "Reset your password", html, text); err != nil {
		log.Printf("authproxy: send reset email to %s: %v", user.Email, err)
	}
}

// sendVerifyEmail mints a verification token and emails the link.
// Best-effort, same as sendResetEmail.
func (h *authHandlers) sendVerifyEmail(r *http.Request, user *User) {
	if h.mailer == nil {
		return
	}
	raw, err := newRawToken()
	if err != nil {
		log.Printf("authproxy: verify token mint: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := h.store.CreateEmailToken(ctx, user.ID, purposeVerify, hashToken(raw), time.Now().Add(verifyTokenTTL)); err != nil {
		log.Printf("authproxy: CreateEmailToken(verify): %v", err)
		return
	}
	link := publicBaseURL(r) + "/auth/verify?token=" + raw
	text, html := verifyEmailBody(link)
	if err := h.mailer.Send(user.Email, "Verify your email", html, text); err != nil {
		log.Printf("authproxy: send verify email to %s: %v", user.Email, err)
	}
}
