package main

import (
	"errors"
	"fmt"
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
//     + password against the bound DB, plus whichever OAuth providers
//     have env vars set.
//
// Other knobs:
//
//   AGENTRY_AUTH_DB        "postgres" | "mysql" — which family driver
//                          to dial. Mongo is refused with the same
//                          v2-coming message the CLI returns.
//   AGENTRY_AUTH_SECRET    32-byte hex, HMAC key for cookies + the
//                          bridge-pre-signed identity headers.
//   DATABASE_URL           the bound DB's URL (any of the standard
//                          fallback names — see dbURLFromEnv).
//   PORT                   listen port (default 3000, matching the
//                          AgentryDeployPort convention from m1).
//   AGENTRY_AUTHPROXY_UPSTREAM
//                          where to proxy authenticated requests
//                          (default 127.0.0.1:3001). The user's app
//                          binds here behind us.
//   <PROVIDER>_CLIENT_ID +
//   <PROVIDER>_CLIENT_SECRET
//                          enable that provider on the login page.
//                          Known: GOOGLE, GITHUB, MICROSOFT, APPLE,
//                          GENERIC_OIDC (the last requires
//                          GENERIC_OIDC_ISSUER).
//   AGENTRY_AUTH_DEBUG     "1" to dump config (with secrets redacted)
//                          on startup. Default off.
type Config struct {
	Mode      string // "passthrough" or "agentry"
	DBKind    string // "postgres" | "mysql"
	DBURL     string
	Secret    string
	Port      string
	Upstream  string
	Providers map[string]ProviderConfig
	Debug     bool
}

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

	cfg.Secret = os.Getenv("AGENTRY_AUTH_SECRET")
	if cfg.Secret == "" {
		return nil, errors.New("AGENTRY_AUTH_SECRET is empty (set by `agentry auth enable`); refusing to start in auth mode without an HMAC key")
	}
	if len(cfg.Secret) < 32 {
		return nil, fmt.Errorf("AGENTRY_AUTH_SECRET is too short (%d hex chars; want at least 32 = 16 bytes)", len(cfg.Secret))
	}

	for _, name := range []string{"google", "github", "microsoft", "apple", "generic-oidc"} {
		if p, ok := readProviderEnv(name); ok {
			cfg.Providers[name] = p
		}
	}

	return cfg, nil
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
