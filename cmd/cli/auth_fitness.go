package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	// Driver registration. Imported for side effects only — we use
	// database/sql throughout the fitness code so the same DDL test
	// works for both postgres and mysql.
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// fitnessReport is the structured result of a DB connectivity test.
// Each bool tracks one capability the sidecar will need at runtime:
// connect, create the tables, write a row, read it back, clean up
// after itself. The first failure stops the cascade — if Connect=false
// the others are meaningless.
//
// We separate "can connect" from "can write" so the error message
// can point at the right thing: a bad URL fails connect; a read-only
// user fails create.
type fitnessReport struct {
	CanConnect bool
	CanCreate  bool
	CanWrite   bool
	CanRead    bool
	CanCleanup bool
	Err        error
}

// Ok returns true only when every step passed. The auth-enable flow
// refuses to proceed otherwise.
func (r fitnessReport) Ok() bool {
	return r.CanConnect && r.CanCreate && r.CanWrite && r.CanRead && r.CanCleanup && r.Err == nil
}

// describe returns a human-readable summary of which step failed
// (or "ok" when everything passed). Used in CLI error output so the
// operator knows whether to fix the URL, the credentials, or the
// schema permissions.
func (r fitnessReport) describe() string {
	switch {
	case !r.CanConnect:
		return "connect failed"
	case !r.CanCreate:
		return "create table failed (read-only user? missing CREATE permission?)"
	case !r.CanWrite:
		return "insert failed (write permission on user's schema?)"
	case !r.CanRead:
		return "select failed (unexpected — connection went stale?)"
	case !r.CanCleanup:
		return "cleanup failed (drop table denied — partial state left on the DB)"
	default:
		return "ok"
	}
}

// runSQLFitness drives a *sql.DB through CREATE → INSERT → SELECT →
// DROP. Shared between postgres and mysql because both speak the
// subset of SQL the test uses. Wraps each phase with a tight
// context timeout so a hung server doesn't block the operator's
// terminal for the full default driver deadline (~30s for postgres).
//
// The table name is fixed (_agentry_fitness) instead of randomly
// generated because if a prior run crashed mid-test, the next one
// should overwrite the leftover row, not pile up garbage.
//
// Caller is responsible for opening + closing the *sql.DB.
func runSQLFitness(ctx context.Context, db *sql.DB) fitnessReport {
	var r fitnessReport
	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		r.Err = fmt.Errorf("ping: %w", err)
		return r
	}
	r.CanConnect = true

	exec := func(timeout time.Duration, q string) error {
		c, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_, err := db.ExecContext(c, q)
		return err
	}

	// CREATE — uses IF NOT EXISTS so a stale leftover doesn't trip us.
	// Two columns: an INT to prove typed writes work, a bare PRIMARY
	// KEY-less shape so we don't hit autoincrement-permission errors
	// on locked-down accounts.
	if err := exec(5*time.Second, `CREATE TABLE IF NOT EXISTS _agentry_fitness (id INTEGER, marker VARCHAR(64))`); err != nil {
		r.Err = fmt.Errorf("create table: %w", err)
		return r
	}
	r.CanCreate = true

	// INSERT. Marker carries an obvious value so a curious operator
	// browsing the DB can see what touched the table.
	if err := exec(5*time.Second, `INSERT INTO _agentry_fitness (id, marker) VALUES (1, 'agentry-fitness-check')`); err != nil {
		r.Err = fmt.Errorf("insert: %w", err)
		return r
	}
	r.CanWrite = true

	// SELECT. Don't require exact-count semantics (prior crashed
	// runs might've left a row) — just confirm at least one match.
	queryCtx, queryCancel := context.WithTimeout(ctx, 5*time.Second)
	defer queryCancel()
	var marker string
	row := db.QueryRowContext(queryCtx, `SELECT marker FROM _agentry_fitness WHERE id = 1 LIMIT 1`)
	if err := row.Scan(&marker); err != nil {
		r.Err = fmt.Errorf("select: %w", err)
		return r
	}
	if !strings.HasPrefix(marker, "agentry-fitness") {
		r.Err = fmt.Errorf("select got unexpected marker %q (someone wrote into our fitness table?)", marker)
		return r
	}
	r.CanRead = true

	// Cleanup. Refuse to call the run "ok" if drop fails — that means
	// our test left state behind on the operator's DB.
	if err := exec(5*time.Second, `DROP TABLE _agentry_fitness`); err != nil {
		r.Err = fmt.Errorf("drop: %w", err)
		return r
	}
	r.CanCleanup = true
	return r
}

// fitnessPostgres opens the bound DB and runs the SQL fitness test.
// URL shape is whatever the operator put on `agentry service bind
// postgres` — we don't reshape it, just hand it straight to the
// driver. (pgx/v5 accepts both the libpq DSN and the URL shape.)
func fitnessPostgres(url string) fitnessReport {
	if url == "" {
		return fitnessReport{Err: fmt.Errorf("DATABASE_URL is empty in the postgres bind")}
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return fitnessReport{Err: fmt.Errorf("open: %w", err)}
	}
	defer db.Close()
	// Keep the pool tiny — we're using one connection for one
	// quick test.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return runSQLFitness(ctx, db)
}

// fitnessMySQL same shape, mysql driver.
func fitnessMySQL(url string) fitnessReport {
	if url == "" {
		return fitnessReport{Err: fmt.Errorf("DATABASE_URL is empty in the mysql bind")}
	}
	db, err := sql.Open("mysql", url)
	if err != nil {
		return fitnessReport{Err: fmt.Errorf("open: %w", err)}
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return runSQLFitness(ctx, db)
}

// fitnessMongo is currently a refusal — the sidecar's mongo adapter
// isn't ready in v1 of the auth feature. We accept the catalog
// binding for forward-compat so operators can `service bind
// mongodb` without errors, but we won't sign off on the auth posture
// until the mongo path is fully validated.
//
// The refusal is intentional and explicit: silently "passing" with a
// stub check would land operators with an auth feature that fails
// later inside the sidecar — exactly the failure mode the fitness
// check is meant to prevent.
func fitnessMongo(url string) fitnessReport {
	return fitnessReport{
		Err: fmt.Errorf("mongo support for `agentry auth enable` is not yet validated in v1; bind postgres or mysql instead, or wait for the next release"),
	}
}

// ── OIDC provider discovery ────────────────────────────────────────────

// knownProviders is what `agentry auth providers add` accepts. The
// well-known config URL lets us fetch the issuer's claim shape +
// supported scopes upfront, so a typo'd client_id catches before any
// sandbox tries to use it.
//
// Generic OIDC lets the operator bring their own (Authentik, Okta,
// Keycloak, …): they pass --issuer URL with the .well-known prefix.
var knownProviders = map[string]string{
	"google":       "https://accounts.google.com/.well-known/openid-configuration",
	"microsoft":    "https://login.microsoftonline.com/common/v2.0/.well-known/openid-configuration",
	"apple":        "https://appleid.apple.com/.well-known/openid-configuration",
	"generic-oidc": "",
	// GitHub is OAuth 2.0, not OIDC — no well-known endpoint. We
	// special-case it to a different validation flow below.
	"github": "",
}

// providerKnown returns true for any provider we ship a config block
// for (including github + generic-oidc, which bypass OIDC discovery).
func providerKnown(name string) bool {
	_, ok := knownProviders[name]
	return ok
}

// validateProvider does the cheapest possible "is this real?" check
// for an upstream OAuth/OIDC provider. The shape:
//
//   - google / microsoft / apple: GET the well-known config; assert
//     a non-empty `authorization_endpoint` came back. Catches DNS
//     typos, expired DNS, captive portals, and "the provider doesn't
//     actually accept OIDC discovery".
//   - github: GET https://api.github.com/. Assert a 200 (their
//     unauthenticated discovery endpoint is the root JSON). Doesn't
//     validate the client_id — there's no public API for that until
//     the actual auth dance happens.
//   - generic-oidc: caller supplies the issuer URL; we treat it as
//     google-shape.
//
// Returns nil when the provider answers; an error when it doesn't.
// Doesn't validate the client_id beyond "the provider exists" — the
// real proof of life happens during the first user login, and we
// catch that via the sidecar's healthgate (Phase 4).
func validateProvider(ctx context.Context, name, issuerOverride string) error {
	if !providerKnown(name) {
		return fmt.Errorf("unknown provider %q (known: google, github, microsoft, apple, generic-oidc)", name)
	}
	if name == "github" {
		return validateGitHubProvider(ctx)
	}
	configURL := knownProviders[name]
	if issuerOverride != "" {
		// Trim a trailing slash if the operator copy-pasted one;
		// concatenating .well-known after a slash gives "//.well-known".
		configURL = strings.TrimRight(issuerOverride, "/") + "/.well-known/openid-configuration"
	}
	if configURL == "" {
		return fmt.Errorf("provider %q requires --issuer URL", name)
	}
	return validateOIDCDiscovery(ctx, configURL)
}

func validateOIDCDiscovery(ctx context.Context, configURL string) error {
	if _, err := url.Parse(configURL); err != nil {
		return fmt.Errorf("invalid issuer URL %q: %w", configURL, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", configURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", configURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("%s returned %d: %s", configURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("parse %s: %w", configURL, err)
	}
	if doc.AuthorizationEndpoint == "" {
		return fmt.Errorf("%s did not declare authorization_endpoint", configURL)
	}
	return nil
}

func validateGitHubProvider(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", "https://api.github.com/", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch github root: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("github root status %d (upstream issue)", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("github root status %d (unexpected; network/auth issue?)", resp.StatusCode)
	}
	return nil
}

// providerEnvVarsForBind tells the auth-enable flow which env vars on
// a service binding to look at for the DB URL. Postgres + mysql both
// stamp DATABASE_URL; we just trust the binding's env map.
func dbBindingURL(b *StoredBind) string {
	if b == nil {
		return ""
	}
	for _, candidate := range []string{"DATABASE_URL", "MONGODB_URI", "MONGO_URL", "POSTGRES_URL", "MYSQL_URL"} {
		if v, ok := b.Env[candidate]; ok && v != "" {
			return v
		}
	}
	return ""
}
