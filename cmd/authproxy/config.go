package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// Config is the boot-time configuration the sidecar reads from env.
// Everything is sourced from the env-stamping the CLI's
// applyClusterEnvDefaults hook does — see m2 phase 3 for the producer
// side. No file-based config, no command-line flags beyond debug
// ergonomics.
//
// Three startup modes driven by AGENTRY_AUTH_MODE:
//
//   - "" or "off" or "passthrough" — the sidecar is just a proxy.
//     Useful for projects that don't want our auth surface even
//     though they're running our base image.
//   - "agentry" (or anything else, defaulting here) — full auth: email
//   - password against the bound DB, plus whichever OAuth providers
//     have env vars set.
//
// Other knobs:
//
//	AGENTRY_AUTH_DB        "postgres" | "mysql" — which family driver
//	                       to dial. Mongo is refused with the same
//	                       v2-coming message the CLI returns.
//	AGENTRY_AUTH_SECRET    32-byte hex, HMAC key for cookies + the
//	                       bridge-pre-signed identity headers.
//	DATABASE_URL           the bound DB's URL (any of the standard
//	                       fallback names — see dbURLFromEnv).
//	PORT                   listen port (default 3000, matching the
//	                       AgentryDeployPort convention from m1).
//	AGENTRY_AUTHPROXY_UPSTREAM
//	                       where to proxy authenticated requests
//	                       (default 127.0.0.1:3001). The user's app
//	                       binds here behind us.
//	<PROVIDER>_CLIENT_ID +
//	<PROVIDER>_CLIENT_SECRET
//	                       enable that provider on the login page.
//	                       Known: GOOGLE, GITHUB, MICROSOFT, APPLE,
//	                       GENERIC_OIDC (the last requires
//	                       GENERIC_OIDC_ISSUER).
//	AGENTRY_AUTH_DEBUG     "1" to dump config (with secrets redacted)
//	                       on startup. Default off.
type Config struct {
	Mode   string // "passthrough" or "agentry"
	DBKind string // "postgres" | "mysql"
	DBURL  string
	// Secret is the PER-APP signing key — derived from the shared
	// AGENTRY_AUTH_SECRET mixed with AppID. Used for session cookies,
	// CSRF tokens, and OAuth state so one app can't accept another
	// app's cookie even though they share the root secret.
	Secret string
	// IdentitySecret is the shared root AGENTRY_AUTH_SECRET, used ONLY
	// to sign the X-Forwarded-Sig identity header — apps verify that
	// against AGENTRY_AUTH_SECRET, which they have in env. (Not a
	// cross-app vector: inbound identity headers are always stripped.)
	IdentitySecret string
	// AppID uniquely + stably identifies this app (deployment id or
	// sandbox id). AppSuffix is its DB-safe table-name suffix. Together
	// they isolate every app's users into its own table.
	AppID     string
	AppSuffix string
	Port      string
	Upstream  string
	Providers map[string]ProviderConfig
	Debug     bool

	// Email is non-nil when an SMTP service is bound (SMTP_HOST +
	// SMTP_FROM present). Its presence is the "email capability" — it
	// lights up the password-reset route + the "Forgot password?" link.
	Email *EmailConfig

	// RequireVerification gates login on a verified email address. Off
	// by default; only meaningful when Email is configured (no SMTP →
	// no way to verify → we never block). Set via
	// AGENTRY_AUTH_REQUIRE_VERIFICATION.
	RequireVerification bool
}

// EmailEnabled reports whether the email capability is lit.
func (c *Config) EmailEnabled() bool { return c.Email != nil }

// ProviderConfig is what we keep per OAuth provider after parsing env
// vars. Issuer is filled in for generic-oidc (or as an override on
// the known providers when the operator chose a custom issuer in
// `agentry auth providers add`).
type ProviderConfig struct {
	ClientID     string
	ClientSecret string
	Issuer       string
	Scopes       []string
}

// loadConfig reads every relevant env var, defaults the obvious ones,
// and refuses to boot under a misconfiguration that would silently
// downgrade security (auth-enabled with no AUTH_SECRET, etc.).
//
// Returns Mode=passthrough with no error when AGENTRY_AUTH_ENABLED is
// not "true" — that's the documented "just proxy" path and shouldn't
// require the rest of the env to be set.
func loadConfig() (*Config, error) {
	cfg := &Config{
		Port:      defaultPort(),
		Upstream:  envOr("AGENTRY_AUTHPROXY_UPSTREAM", "127.0.0.1:3001"),
		Providers: map[string]ProviderConfig{},
		Debug:     os.Getenv("AGENTRY_AUTH_DEBUG") == "1",
	}

	enabled := strings.ToLower(os.Getenv("AGENTRY_AUTH_ENABLED"))
	switch enabled {
	case "true", "1", "yes":
		cfg.Mode = "agentry"
	default:
		cfg.Mode = "passthrough"
		return cfg, nil
	}

	cfg.DBKind = strings.ToLower(os.Getenv("AGENTRY_AUTH_DB"))
	switch cfg.DBKind {
	case "postgres", "mysql":
		// known + supported
	case "mongo", "mongodb":
		// Normalize the family alias so openStore + the rest of the
		// code path see a single canonical name.
		cfg.DBKind = "mongo"
	case "":
		return nil, errors.New("AGENTRY_AUTH_DB is empty (set by `agentry auth enable`); cannot start without a DB family")
	default:
		return nil, fmt.Errorf("unknown AGENTRY_AUTH_DB=%q (expected postgres, mysql, or mongo)", cfg.DBKind)
	}

	cfg.DBURL = dbURLFromEnv()
	if cfg.DBURL == "" {
		return nil, errors.New("no DB URL found in env (checked DATABASE_URL, POSTGRES_URL, MYSQL_URL, MONGODB_URI, MONGO_URL); is the service binding intact?")
	}

	root := os.Getenv("AGENTRY_AUTH_SECRET")
	if root == "" {
		return nil, errors.New("AGENTRY_AUTH_SECRET is empty (set by `agentry auth enable`); refusing to start in auth mode without an HMAC key")
	}
	if len(root) < 32 {
		return nil, fmt.Errorf("AGENTRY_AUTH_SECRET is too short (%d hex chars; want at least 32 = 16 bytes)", len(root))
	}
	cfg.IdentitySecret = root

	// Per-app isolation. Every app (one deployment, or one sandbox) gets
	// its own users table and its own session-signing key, both derived
	// from a stable, unique app id. Without this, every auth app sharing
	// a DB binding would write to one `agentry_users` table AND — because
	// sessions are stateless HMACs — accept each other's session cookies.
	cfg.AppID = appIDFromEnv()
	if cfg.AppID == "" {
		log.Printf("authproxy: WARNING — no AGENTRY_APP_ID / AGENTRY_APP_NAME / SANDBOX_ID in env; " +
			"auth state will NOT be isolated per app. Set AGENTRY_APP_ID to a unique value.")
		cfg.AppID = "default"
	}
	cfg.AppSuffix = appSuffix(cfg.AppID)
	cfg.Secret = deriveAppSecret(root, cfg.AppID)

	for _, name := range []string{"google", "github", "microsoft", "apple", "generic-oidc"} {
		if p, ok := readProviderEnv(name); ok {
			cfg.Providers[name] = p
		}
	}

	// Email capability: lit purely by the bound SMTP_* env. No flag —
	// bind the smtp service and reset/verify become available.
	if email, ok := loadEmailConfig(); ok {
		cfg.Email = email
	}
	// Verification gate only has teeth when we can actually send mail.
	switch strings.ToLower(os.Getenv("AGENTRY_AUTH_REQUIRE_VERIFICATION")) {
	case "true", "1", "yes":
		cfg.RequireVerification = cfg.Email != nil
	}

	return cfg, nil
}

// appIDFromEnv finds the stable, unique identifier for this app. The
// control plane stamps AGENTRY_APP_ID = deployment id on deployments;
// the provisioner stamps it (and SANDBOX_ID) on sandboxes. AGENTRY_APP_NAME
// is a last-resort fallback only — it's a human slug that can collide
// between apps, so it's used solely to avoid a hard failure.
func appIDFromEnv() string {
	for _, k := range []string{"AGENTRY_APP_ID", "AGENTRY_APP_NAME", "SANDBOX_ID"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// appSuffix turns an app id into a short, DB-safe, collision-resistant
// table-name suffix. Hashing keeps it bounded and guarantees the result
// is `[a-f0-9]+` (safe to interpolate into a table name — it can never
// be SQL).
func appSuffix(appID string) string {
	sum := sha256.Sum256([]byte(appID))
	return hex.EncodeToString(sum[:])[:16]
}

// deriveAppSecret mixes the shared root secret with the app id so each
// app signs sessions/CSRF/OAuth-state with a key no other app can
// reproduce — even though they all share AGENTRY_AUTH_SECRET.
func deriveAppSecret(root, appID string) string {
	mac := hmac.New(sha256.New, []byte(root))
	mac.Write([]byte("agentry-app-secret:"))
	mac.Write([]byte(appID))
	return hex.EncodeToString(mac.Sum(nil))
}

func defaultPort() string {
	if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return v
		}
	}
	return "3000"
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// dbURLFromEnv finds the bound DB URL. Walks the canonical names a
// service-bind would stamp; first non-empty wins. Stays in sync with
// the CLI's dbBindingURL helper so the producer + consumer can't
// drift.
func dbURLFromEnv() string {
	for _, k := range []string{
		"DATABASE_URL",
		"POSTGRES_URL",
		"MYSQL_URL",
		"MONGODB_URI",
		"MONGO_URL",
	} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// readProviderEnv returns (cfg, true) if BOTH _CLIENT_ID and
// _CLIENT_SECRET are set for the provider. Either-only is a misconfig
// that we treat as "provider not enabled" instead of erroring — the
// operator probably forgot to set one half and the login page just
// won't show the button until they fix it.
func readProviderEnv(name string) (ProviderConfig, bool) {
	upper := upperEnvSafe(name)
	id := os.Getenv(upper + "_CLIENT_ID")
	sec := os.Getenv(upper + "_CLIENT_SECRET")
	if id == "" || sec == "" {
		return ProviderConfig{}, false
	}
	scopes := strings.Fields(os.Getenv(upper + "_SCOPES"))
	issuer := os.Getenv(upper + "_ISSUER")
	return ProviderConfig{
		ClientID:     id,
		ClientSecret: sec,
		Issuer:       issuer,
		Scopes:       scopes,
	}, true
}

// upperEnvSafe matches the CLI-side helper of the same name so the
// producer + consumer agree on the casing. (Duplicated rather than
// imported because cmd/cli is a binary, not a library, and the
// sidecar is a separate binary that must build standalone.)
func upperEnvSafe(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' || c == ' ' {
			out = append(out, '_')
			continue
		}
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
