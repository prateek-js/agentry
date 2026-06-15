package main

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// isolation_test.go — covers the per-app isolation guarantees: every
// app (deployment or sandbox) gets its own users table AND its own
// session-signing secret, derived from a unique app id. Two apps that
// share a DB binding must never share a table or accept each other's
// sessions.

var hex16 = regexp.MustCompile(`^[a-f0-9]{16}$`)

func TestAppSuffix_DeterministicAndSafe(t *testing.T) {
	a := appSuffix("dep_abc123")
	if a != appSuffix("dep_abc123") {
		t.Fatalf("appSuffix not deterministic")
	}
	if !hex16.MatchString(a) {
		t.Fatalf("appSuffix %q is not 16 lowercase hex chars (must be SQL-safe)", a)
	}
	if appSuffix("dep_abc123") == appSuffix("dep_xyz789") {
		t.Fatalf("different app ids produced the same suffix — apps would co-mingle")
	}
}

func TestUsersTable_PerApp(t *testing.T) {
	s1 := usersTable(appSuffix("dep_a"))
	s2 := usersTable(appSuffix("dep_b"))
	if s1 == s2 {
		t.Fatalf("two apps got the same users table: %q", s1)
	}
	if !strings.HasPrefix(s1, "agentry_users_") {
		t.Fatalf("unexpected table name %q", s1)
	}
	// Empty suffix falls back to the legacy unsuffixed name.
	if got := usersTable(""); got != "agentry_users" {
		t.Fatalf("empty suffix should yield legacy name, got %q", got)
	}
	if got := tokensTable(""); got != "agentry_email_tokens" {
		t.Fatalf("empty suffix should yield legacy token table, got %q", got)
	}
}

func TestSqlStore_TblRewrite(t *testing.T) {
	s := &sqlStore{kind: "postgres", users: usersTable("aaaa"), tokens: tokensTable("aaaa")}
	got := s.tbl(`SELECT * FROM agentry_users WHERE id = ?`)
	want := "SELECT * FROM " + s.users + " WHERE id = ?"
	if got != want {
		t.Fatalf("tbl rewrite:\n got=%q\nwant=%q", got, want)
	}
	// Email-token table rewrites independently.
	if got := s.tbl(`INSERT INTO agentry_email_tokens (token_hash) VALUES (?)`); !strings.Contains(got, s.tokens) || strings.Contains(got, "agentry_email_tokens ") {
		t.Fatalf("token table not rewritten: %q", got)
	}
	// The provider index name is derived from the table name, so it's
	// per-app too — two apps can't collide on CREATE INDEX in one DB.
	idx := s.tbl(`CREATE INDEX IF NOT EXISTS agentry_users_provider_idx ON agentry_users (provider, provider_id)`)
	if !strings.Contains(idx, s.users+"_provider_idx") || !strings.Contains(idx, "ON "+s.users+" ") {
		t.Fatalf("index DDL not fully rewritten per-app: %q", idx)
	}
}

func TestDeriveAppSecret_PerApp(t *testing.T) {
	root := "00112233445566778899aabbccddeeff" // 32 hex
	a := deriveAppSecret(root, "dep_a")
	b := deriveAppSecret(root, "dep_b")
	if a == b {
		t.Fatalf("two apps derived the same secret — sessions would be cross-valid")
	}
	if a == root || b == root {
		t.Fatalf("derived secret equals the root secret")
	}
	if a != deriveAppSecret(root, "dep_a") {
		t.Fatalf("deriveAppSecret not deterministic")
	}
}

// TestCrossAppSession_Rejected is the core guarantee: a session minted
// for app A must NOT open under app B's secret, even though both apps
// share the same root AGENTRY_AUTH_SECRET. Without per-app secrets this
// fails — that was the bug.
func TestCrossAppSession_Rejected(t *testing.T) {
	root := "00112233445566778899aabbccddeeff"
	secretA := deriveAppSecret(root, "dep_a")
	secretB := deriveAppSecret(root, "dep_b")

	p := SessionPayload{UID: "u1", Email: "user@example.com", Provider: "password", Exp: time.Now().Add(time.Hour).Unix()}
	cookie, err := sealSession(p, secretA)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Same app: opens fine.
	if _, err := openSession(cookie, secretA); err != nil {
		t.Fatalf("session should open under its own app secret: %v", err)
	}
	// Different app: must be rejected as tampered.
	if _, err := openSession(cookie, secretB); err == nil {
		t.Fatalf("SECURITY: app B accepted app A's session cookie")
	}
}

func TestAppIDFromEnv_FallbackOrder(t *testing.T) {
	t.Setenv("AGENTRY_APP_ID", "")
	t.Setenv("AGENTRY_APP_NAME", "")
	t.Setenv("SANDBOX_ID", "sbx_1")
	if got := appIDFromEnv(); got != "sbx_1" {
		t.Fatalf("fallback to SANDBOX_ID failed, got %q", got)
	}
	t.Setenv("AGENTRY_APP_NAME", "myapp")
	if got := appIDFromEnv(); got != "myapp" {
		t.Fatalf("AGENTRY_APP_NAME should beat SANDBOX_ID, got %q", got)
	}
	t.Setenv("AGENTRY_APP_ID", "dep_primary")
	if got := appIDFromEnv(); got != "dep_primary" {
		t.Fatalf("AGENTRY_APP_ID should win, got %q", got)
	}
}
