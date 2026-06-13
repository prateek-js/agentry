package main

import (
	"strings"
	"testing"
)

// setenv saves + restores env for the duration of a subtest. Required
// because loadConfig reads from the process env, and running tests in
// parallel against shared env would cross-contaminate.
func setenv(t *testing.T, kvs map[string]string) {
	t.Helper()
	for k, v := range kvs {
		t.Setenv(k, v)
	}
}

func TestLoadConfigPassthroughByDefault(t *testing.T) {
	// Make sure nothing else is set.
	t.Setenv("AGENTRY_AUTH_ENABLED", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("expected no error in passthrough, got %v", err)
	}
	if cfg.Mode != "passthrough" {
		t.Fatalf("expected passthrough, got %q", cfg.Mode)
	}
}

func TestLoadConfigAgentryRequiresDB(t *testing.T) {
	setenv(t, map[string]string{
		"AGENTRY_AUTH_ENABLED": "true",
		"AGENTRY_AUTH_SECRET":  strings.Repeat("a", 32),
	})
	t.Setenv("AGENTRY_AUTH_DB", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected error when AGENTRY_AUTH_DB is empty")
	}
}

func TestLoadConfigAcceptsMongo(t *testing.T) {
	setenv(t, map[string]string{
		"AGENTRY_AUTH_ENABLED": "true",
		"AGENTRY_AUTH_DB":      "mongo",
		"AGENTRY_AUTH_SECRET":  strings.Repeat("a", 32),
		"DATABASE_URL":         "mongodb://localhost/x",
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("expected mongo to load, got %v", err)
	}
	if cfg.DBKind != "mongo" {
		t.Fatalf("expected canonical DBKind=mongo, got %q", cfg.DBKind)
	}
}

func TestLoadConfigAcceptsMongoDBAlias(t *testing.T) {
	// AGENTRY_AUTH_DB=mongodb should normalize to "mongo" so openStore
	// and friends see one canonical name.
	setenv(t, map[string]string{
		"AGENTRY_AUTH_ENABLED": "true",
		"AGENTRY_AUTH_DB":      "mongodb",
		"AGENTRY_AUTH_SECRET":  strings.Repeat("a", 32),
		"DATABASE_URL":         "mongodb://localhost/x",
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("expected mongodb alias to load, got %v", err)
	}
	if cfg.DBKind != "mongo" {
		t.Fatalf("expected normalization to mongo, got %q", cfg.DBKind)
	}
}

func TestLoadConfigRequiresSecret(t *testing.T) {
	setenv(t, map[string]string{
		"AGENTRY_AUTH_ENABLED": "true",
		"AGENTRY_AUTH_DB":      "postgres",
		"DATABASE_URL":         "postgres://x",
	})
	t.Setenv("AGENTRY_AUTH_SECRET", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected secret-required error")
	}
}

func TestLoadConfigRejectsShortSecret(t *testing.T) {
	setenv(t, map[string]string{
		"AGENTRY_AUTH_ENABLED": "true",
		"AGENTRY_AUTH_DB":      "postgres",
		"DATABASE_URL":         "postgres://x",
		"AGENTRY_AUTH_SECRET":  "tooshort",
	})
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected short-secret error")
	}
}

func TestLoadConfigRequiresDBURL(t *testing.T) {
	setenv(t, map[string]string{
		"AGENTRY_AUTH_ENABLED": "true",
		"AGENTRY_AUTH_DB":      "postgres",
		"AGENTRY_AUTH_SECRET":  strings.Repeat("a", 32),
	})
	t.Setenv("DATABASE_URL", "")
	t.Setenv("POSTGRES_URL", "")
	t.Setenv("MYSQL_URL", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected DB URL error")
	}
}

func TestLoadConfigReadsProvider(t *testing.T) {
	setenv(t, map[string]string{
		"AGENTRY_AUTH_ENABLED": "true",
		"AGENTRY_AUTH_DB":      "postgres",
		"AGENTRY_AUTH_SECRET":  strings.Repeat("a", 32),
		"DATABASE_URL":         "postgres://x",
		"GOOGLE_CLIENT_ID":     "gid",
		"GOOGLE_CLIENT_SECRET": "gsec",
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Providers["google"]
	if !ok {
		t.Fatalf("google provider not loaded: %v", cfg.Providers)
	}
	if p.ClientID != "gid" || p.ClientSecret != "gsec" {
		t.Fatalf("provider creds wrong: %+v", p)
	}
}

func TestLoadConfigProviderHalfMissing(t *testing.T) {
	setenv(t, map[string]string{
		"AGENTRY_AUTH_ENABLED": "true",
		"AGENTRY_AUTH_DB":      "postgres",
		"AGENTRY_AUTH_SECRET":  strings.Repeat("a", 32),
		"DATABASE_URL":         "postgres://x",
		"GOOGLE_CLIENT_ID":     "gid",
		// no GOOGLE_CLIENT_SECRET — provider should NOT load.
	})
	t.Setenv("GOOGLE_CLIENT_SECRET", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Providers["google"]; ok {
		t.Fatal("provider loaded with only half its creds")
	}
}

func TestLoadConfigCustomPort(t *testing.T) {
	setenv(t, map[string]string{
		"AGENTRY_AUTH_ENABLED": "true",
		"AGENTRY_AUTH_DB":      "mysql",
		"AGENTRY_AUTH_SECRET":  strings.Repeat("a", 32),
		"MYSQL_URL":            "root:@/test",
		"PORT":                 "9090",
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "9090" {
		t.Fatalf("expected port 9090, got %q", cfg.Port)
	}
}

func TestLoadConfigBadPortDefaults(t *testing.T) {
	setenv(t, map[string]string{
		"AGENTRY_AUTH_ENABLED": "true",
		"AGENTRY_AUTH_DB":      "mysql",
		"AGENTRY_AUTH_SECRET":  strings.Repeat("a", 32),
		"MYSQL_URL":            "root:@/test",
		"PORT":                 "not-a-port",
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "3000" {
		t.Fatalf("expected default 3000, got %q", cfg.Port)
	}
}

func TestUpperEnvSafe(t *testing.T) {
	cases := map[string]string{
		"google":       "GOOGLE",
		"generic-oidc": "GENERIC_OIDC",
		"micro soft":   "MICRO_SOFT",
		"GitHub":       "GITHUB",
	}
	for in, want := range cases {
		if got := upperEnvSafe(in); got != want {
			t.Fatalf("upperEnvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDBURLFromEnvWalksFallback(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("POSTGRES_URL", "postgres://from-postgres-url")
	t.Setenv("MYSQL_URL", "")
	if got := dbURLFromEnv(); got != "postgres://from-postgres-url" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("DATABASE_URL", "postgres://from-database-url")
	if got := dbURLFromEnv(); got != "postgres://from-database-url" {
		t.Fatalf("DATABASE_URL didn't win: %q", got)
	}
}

func TestLoadConfigEmailCapabilityFromSMTP(t *testing.T) {
	base := map[string]string{
		"AGENTRY_AUTH_ENABLED": "true",
		"AGENTRY_AUTH_DB":      "postgres",
		"AGENTRY_AUTH_SECRET":  strings.Repeat("a", 32),
		"DATABASE_URL":         "postgres://localhost/x",
	}

	// No SMTP → email capability off.
	setenv(t, base)
	t.Setenv("SMTP_HOST", "")
	t.Setenv("SMTP_FROM", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmailEnabled() {
		t.Error("email should be OFF without SMTP_HOST/FROM")
	}

	// SMTP host+from → email capability on.
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_FROM", "no-reply@example.com")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.EmailEnabled() {
		t.Error("email should be ON with SMTP bound")
	}
}

func TestLoadConfigVerificationNeedsEmail(t *testing.T) {
	base := map[string]string{
		"AGENTRY_AUTH_ENABLED":              "true",
		"AGENTRY_AUTH_DB":                   "postgres",
		"AGENTRY_AUTH_SECRET":               strings.Repeat("a", 32),
		"DATABASE_URL":                      "postgres://localhost/x",
		"AGENTRY_AUTH_REQUIRE_VERIFICATION": "true",
	}
	// Verification asked for, but no SMTP → must stay OFF (we can't verify
	// without a way to send mail; refusing-to-gate is the safe default).
	setenv(t, base)
	t.Setenv("SMTP_HOST", "")
	t.Setenv("SMTP_FROM", "")
	cfg, _ := loadConfig()
	if cfg.RequireVerification {
		t.Error("verification must be disabled when no SMTP is bound")
	}
	// With SMTP, the flag takes effect.
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_FROM", "no-reply@example.com")
	cfg, _ = loadConfig()
	if !cfg.RequireVerification {
		t.Error("verification should be enabled with SMTP + the flag")
	}
}
