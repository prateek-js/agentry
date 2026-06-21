package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oauth.go — minimal OAuth 2.0 / OIDC client per provider.
//
// We do NOT depend on golang.org/x/oauth2 or coreos/go-oidc — both are
// fine libraries but bring in transitive deps and config knobs we
// don't need. The provider set is small (~5), the flows are
// well-specified, and the runtime budget for a sidecar matters.
//
// Per provider we know:
//   - Authorize URL  (where we send the user)
//   - Token URL      (where we POST code → access token)
//   - Userinfo URL   (where we GET email + name)
//   - Scopes         (default + operator override)
//   - id_field, email_field, name_field for the userinfo response.
//
// PKCE is enforced on every provider (S256). State is HMAC-bound to
// the cookie so a malicious redirect can't be replayed against a
// different browser session.
//
// Each callback ends in `UpsertUserFromOAuth` + setSessionCookie + 302
// to "/".

type oauthHandlers struct {
	cfg   *Config
	store Store
}

// oauthState is the temporary blob we sign and stuff into the `state`
// query param sent to the provider. It carries everything we need on
// the callback so we don't need a server-side state table.
type oauthState struct {
	Provider     string `json:"p"`
	PKCEVerifier string `json:"v"`
	Nonce        string `json:"n"` // CSRF-ish — verified against cookie
	Exp          int64  `json:"x"`
}

const oauthStateCookieName = "agentry_oauth_state"
const oauthStateMaxAge = 10 * time.Minute

// register adds /auth/oauth/<provider>/{start,callback} routes for
// every configured provider.
func (h *oauthHandlers) register(mux *http.ServeMux) {
	for name := range h.cfg.Providers {
		name := name // capture
		mux.HandleFunc("/auth/oauth/"+name+"/start", func(w http.ResponseWriter, r *http.Request) {
			h.start(w, r, name)
		})
		mux.HandleFunc("/auth/oauth/"+name+"/callback", func(w http.ResponseWriter, r *http.Request) {
			h.callback(w, r, name)
		})
	}
}

// start kicks off the flow: mint PKCE pair, sign state, redirect to
// provider's authorize endpoint.
func (h *oauthHandlers) start(w http.ResponseWriter, r *http.Request, name string) {
	pcfg, ok := h.cfg.Providers[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	spec, err := providerSpec(name, pcfg)
	if err != nil {
		log.Printf("authproxy: provider spec %q: %v", name, err)
		http.Error(w, "provider misconfigured", http.StatusInternalServerError)
		return
	}
	verifier, challenge, err := mintPKCE()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nonce, err := randHex(16)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	st := oauthState{
		Provider:     name,
		PKCEVerifier: verifier,
		Nonce:        nonce,
		Exp:          time.Now().Add(oauthStateMaxAge).Unix(),
	}
	stateVal, err := sealOAuthState(st, h.cfg.Secret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Cookie holds the nonce — provider sends back the signed state,
	// we verify the nonce in the state against the nonce in this
	// cookie. If they differ, the callback was replayed against the
	// wrong session.
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    nonce,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   int(oauthStateMaxAge / time.Second),
	})

	scopes := spec.defaultScopes
	if len(pcfg.Scopes) > 0 {
		scopes = pcfg.Scopes
	}
	q := url.Values{}
	q.Set("client_id", pcfg.ClientID)
	q.Set("redirect_uri", originForRedirect(r)+"/auth/oauth/"+name+"/callback")
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", stateVal)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if spec.extraAuthorizeParams != nil {
		for k, v := range spec.extraAuthorizeParams {
			q.Set(k, v)
		}
	}
	http.Redirect(w, r, spec.authorizeURL+"?"+q.Encode(), http.StatusFound)
}

// callback consumes the redirect from the provider, exchanges the
// code, fetches userinfo, upserts the user, sets session.
func (h *oauthHandlers) callback(w http.ResponseWriter, r *http.Request, name string) {
	pcfg, ok := h.cfg.Providers[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	spec, err := providerSpec(name, pcfg)
	if err != nil {
		http.Error(w, "provider misconfigured", http.StatusInternalServerError)
		return
	}

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		log.Printf("authproxy: provider %q returned error: %s", name, errParam)
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}
	code := r.URL.Query().Get("code")
	stateRaw := r.URL.Query().Get("state")
	if code == "" || stateRaw == "" {
		http.Error(w, "missing code/state", http.StatusBadRequest)
		return
	}
	st, err := openOAuthState(stateRaw, h.cfg.Secret)
	if err != nil {
		log.Printf("authproxy: oauth state invalid: %v", err)
		http.Error(w, "state invalid", http.StatusBadRequest)
		return
	}
	if st.Provider != name {
		http.Error(w, "state/provider mismatch", http.StatusBadRequest)
		return
	}
	// Nonce vs cookie.
	c, err := r.Cookie(oauthStateCookieName)
	if err != nil || c.Value != st.Nonce {
		http.Error(w, "state nonce mismatch", http.StatusBadRequest)
		return
	}
	// Expired?
	if time.Now().Unix() > st.Exp {
		http.Error(w, "state expired", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	redirectURI := originForRedirect(r) + "/auth/oauth/" + name + "/callback"
	tok, err := exchangeCode(ctx, spec, pcfg, code, st.PKCEVerifier, redirectURI)
	if err != nil {
		log.Printf("authproxy: token exchange %q: %v", name, err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	info, err := fetchUserInfo(ctx, spec, tok.AccessToken)
	if err != nil {
		log.Printf("authproxy: userinfo %q: %v", name, err)
		http.Error(w, "userinfo failed", http.StatusBadGateway)
		return
	}
	if info.Email == "" {
		http.Error(w, "provider did not return an email; cannot create account", http.StatusBadGateway)
		return
	}
	user, err := h.store.UpsertUserFromOAuth(ctx, name, info.ID, info.Email, info.Name)
	if err != nil {
		log.Printf("authproxy: UpsertUserFromOAuth: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := setSessionCookie(w, r, SessionPayload{
		UID:      user.ID,
		Email:    user.Email,
		Name:     user.Name,
		Provider: user.Provider,
	}, h.cfg.Secret); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Clear the OAuth state cookie — single-use.
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// providerSpecRecord is the per-provider URL set + userinfo schema.
type providerSpecRecord struct {
	authorizeURL  string
	tokenURL      string
	userinfoURL   string
	defaultScopes []string
	// Field names in the userinfo JSON. Defaults work for OIDC-shaped
	// responses; GitHub's userinfo is a special shape we adapt for.
	idField    string
	emailField string
	nameField  string
	// Provider-specific tweaks to the authorize URL.
	extraAuthorizeParams map[string]string
}

func providerSpec(name string, pcfg ProviderConfig) (providerSpecRecord, error) {
	switch name {
	case "google":
		return providerSpecRecord{
			authorizeURL:  "https://accounts.google.com/o/oauth2/v2/auth",
			tokenURL:      "https://oauth2.googleapis.com/token",
			userinfoURL:   "https://openidconnect.googleapis.com/v1/userinfo",
			defaultScopes: []string{"openid", "email", "profile"},
			idField:       "sub",
			emailField:    "email",
			nameField:     "name",
			extraAuthorizeParams: map[string]string{
				"access_type": "online",
				"prompt":      "select_account",
			},
		}, nil
	case "github":
		return providerSpecRecord{
			authorizeURL:  "https://github.com/login/oauth/authorize",
			tokenURL:      "https://github.com/login/oauth/access_token",
			userinfoURL:   "https://api.github.com/user",
			defaultScopes: []string{"read:user", "user:email"},
			idField:       "id",
			emailField:    "email",
			nameField:     "name",
		}, nil
	case "microsoft":
		// We default to the common tenant — operator override via
		// MICROSOFT_ISSUER if they need a single-tenant install.
		issuer := pcfg.Issuer
		if issuer == "" {
			issuer = "https://login.microsoftonline.com/common/v2.0"
		}
		return providerSpecRecord{
			authorizeURL:  strings.TrimRight(issuer, "/") + "/oauth2/v2.0/authorize",
			tokenURL:      strings.TrimRight(issuer, "/") + "/oauth2/v2.0/token",
			userinfoURL:   "https://graph.microsoft.com/oidc/userinfo",
			defaultScopes: []string{"openid", "email", "profile"},
			idField:       "sub",
			emailField:    "email",
			nameField:     "name",
		}, nil
	case "apple":
		return providerSpecRecord{
			authorizeURL: "https://appleid.apple.com/auth/authorize",
			tokenURL:     "https://appleid.apple.com/auth/token",
			userinfoURL:  "", // Apple doesn't expose a userinfo endpoint; we
			//                  decode the id_token client-side. For v1 we
			//                  refuse Apple SSO if we'd need userinfo.
			defaultScopes: []string{"name", "email"},
			idField:       "sub",
			emailField:    "email",
			nameField:     "name",
			extraAuthorizeParams: map[string]string{
				"response_mode": "form_post",
			},
		}, nil
	case "generic-oidc":
		if pcfg.Issuer == "" {
			return providerSpecRecord{}, errors.New("GENERIC_OIDC_ISSUER must be set")
		}
		issuer := strings.TrimRight(pcfg.Issuer, "/")
		return providerSpecRecord{
			authorizeURL:  issuer + "/protocol/openid-connect/auth",
			tokenURL:      issuer + "/protocol/openid-connect/token",
			userinfoURL:   issuer + "/protocol/openid-connect/userinfo",
			defaultScopes: []string{"openid", "email", "profile"},
			idField:       "sub",
			emailField:    "email",
			nameField:     "name",
		}, nil
	}
	return providerSpecRecord{}, fmt.Errorf("unknown provider %q", name)
}

type oauthToken struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
}

func exchangeCode(ctx context.Context, spec providerSpecRecord, pcfg ProviderConfig, code, verifier, redirectURI string) (*oauthToken, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", pcfg.ClientID)
	form.Set("client_secret", pcfg.ClientSecret)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, "POST", spec.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json") // github will otherwise return form-encoded
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("token endpoint %d: %s", resp.StatusCode, snip(body))
	}
	var t oauthToken
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("decode token: %w (body=%s)", err, snip(body))
	}
	if t.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in response: %s", snip(body))
	}
	return &t, nil
}

type userInfo struct {
	ID    string
	Email string
	Name  string
}

func fetchUserInfo(ctx context.Context, spec providerSpecRecord, accessToken string) (*userInfo, error) {
	if spec.userinfoURL == "" {
		return nil, errors.New("provider has no userinfo endpoint (id_token decoding not implemented in v1)")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", spec.userinfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("userinfo %d: %s", resp.StatusCode, snip(body))
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode userinfo: %w", err)
	}
	out := &userInfo{
		ID:    coerceString(raw[spec.idField]),
		Email: coerceString(raw[spec.emailField]),
		Name:  coerceString(raw[spec.nameField]),
	}
	// GitHub: if the user has only private emails, `email` comes back
	// null. We don't recover that here in v1 — operator can warn users
	// to make their primary email public. Documented gotcha.
	return out, nil
}

func coerceString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%d", int64(t))
	case int:
		return fmt.Sprintf("%d", t)
	case int64:
		return fmt.Sprintf("%d", t)
	}
	return ""
}

func snip(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "…"
	}
	return string(b)
}

// PKCE — base64url(sha256(verifier)) is the challenge; the verifier is
// any 43-128 character string we keep secret in the signed state.
func mintPKCE() (verifier, challenge string, err error) {
	v, err := randHex(48) // 96 hex chars → in spec range
	if err != nil {
		return "", "", err
	}
	return v, pkceChallenge(v), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256Sum([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum)
}

func sha256Sum(b []byte) []byte {
	h := sha256.New()
	h.Write(b)
	return h.Sum(nil)
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, x := range b {
		out[i*2] = hex[x>>4]
		out[i*2+1] = hex[x&0x0f]
	}
	return string(out), nil
}

// State sealing — same shape as the session cookie but with its own
// json struct so future renames don't cross-contaminate.
func sealOAuthState(st oauthState, secret string) (string, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmacSHA256(secret, body)
	sig := base64.RawURLEncoding.EncodeToString(mac)
	return body + "." + sig, nil
}

func openOAuthState(s, secret string) (*oauthState, error) {
	parts := strings.SplitN(s, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed")
	}
	body, sig := parts[0], parts[1]
	gotSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return nil, errors.New("malformed sig")
	}
	wantSig := hmacSHA256(secret, body)
	if !hmac.Equal(gotSig, wantSig) {
		return nil, errors.New("signature mismatch")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, errors.New("malformed body")
	}
	var st oauthState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, errors.New("malformed json")
	}
	return &st, nil
}
