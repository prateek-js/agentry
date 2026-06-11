package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentry/agentry/pkg/mcp"
)

// Tests for the operator-side auth feature: on-disk state, secret
// minting, provider validation, pickDBBinding, env-stamping
// integration with the existing PostCreateHook. Fitness checks
// against real DBs are covered in auth_fitness_test.go; here we
// only assert on the pure-logic surface that doesn't need network.

// pinAgentryCluster pins both cluster and profile so the auth
// subcommands have a context to operate on. Returns the base config
// dir so tests can poke at on-disk state directly.
func pinAgentryCluster(t *testing.T, cluster, profile string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agentry.json")
	body := `{"cluster":"` + cluster + `","profile":"` + profile + `"}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTRY_CONFIG", cfg)
	resetMigrationOnce()
	return dir
}

// ── AuthState round-trip ──────────────────────────────────────────────

func TestAuthState_Roundtrip(t *testing.T) {
	pinAgentryCluster(t, "test", "default")
	want := &AuthState{
		Enabled:   true,
		DBBinding: "postgres",
		Secret:    "deadbeef" + strings.Repeat("c0", 28),
		Providers: map[string]AuthProviderState{
			"google": {ClientID: "abc.apps.googleusercontent.com", ClientSecret: "GOCSPX-xyz"},
		},
	}
	if err := saveAuthState("test", "default", want); err != nil {
		t.Fatal(err)
	}
	got, err := loadAuthState("test", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("loadAuthState returned nil")
	}
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Errorf("roundtrip mismatch:\n got  = %s\n want = %s", gj, wj)
	}
}

func TestLoadAuthState_Missing_ReturnsNil(t *testing.T) {
	pinAgentryCluster(t, "test", "default")
	got, err := loadAuthState("test", "default")
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if got != nil {
		t.Errorf("missing file should return nil, got %+v", got)
	}
}

func TestSaveAuthState_FilePerms(t *testing.T) {
	pinAgentryCluster(t, "test", "default")
	if err := saveAuthState("test", "default", &AuthState{Enabled: true, Secret: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(authFilePath("test", "default"))
	if err != nil {
		t.Fatal(err)
	}
	// Auth state holds AUTH_SECRET and every provider's
	// client_secret. World-readable would leak both.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("auth file perms = %o; want 0600 (secrets in there)", perm)
	}
}

func TestDeleteAuthState_IdempotentOnMissing(t *testing.T) {
	pinAgentryCluster(t, "test", "default")
	if err := deleteAuthState("test", "default"); err != nil {
		t.Errorf("delete on missing file should be no-op: %v", err)
	}
}

// ── Profile isolation ────────────────────────────────────────────────

func TestAuthState_ProfileIsolation(t *testing.T) {
	pinAgentryCluster(t, "test", "dev")
	dev := &AuthState{Enabled: true, DBBinding: "postgres", Secret: strings.Repeat("d", 64)}
	prod := &AuthState{Enabled: true, DBBinding: "mysql", Secret: strings.Repeat("p", 64)}
	if err := saveAuthState("test", "dev", dev); err != nil {
		t.Fatal(err)
	}
	if err := saveAuthState("test", "prod", prod); err != nil {
		t.Fatal(err)
	}
	gotDev, _ := loadAuthState("test", "dev")
	gotProd, _ := loadAuthState("test", "prod")
	if gotDev == nil || gotProd == nil {
		t.Fatalf("missing one side: dev=%v prod=%v", gotDev, gotProd)
	}
	if gotDev.DBBinding == gotProd.DBBinding || gotDev.Secret == gotProd.Secret {
		t.Errorf("dev and prod state bled across profiles: dev=%+v prod=%+v", gotDev, gotProd)
	}
}

// ── Secret minting + hex validation ──────────────────────────────────

func TestMintAuthSecret_ShapeAndUniqueness(t *testing.T) {
	a, err := mintAuthSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 {
		t.Errorf("secret length = %d; want 64 hex chars (32 bytes)", len(a))
	}
	if !looksLikeHex(a, 32) {
		t.Errorf("secret %q doesn't look like hex of the right length", a)
	}
	b, _ := mintAuthSecret()
	if a == b {
		t.Errorf("two consecutive mints returned the same secret (entropy issue?)")
	}
}

func TestLooksLikeHex(t *testing.T) {
	cases := []struct {
		s    string
		min  int
		want bool
	}{
		{"deadbeef", 4, true},
		{"DEADBEEF", 4, true},
		{"deadbeef", 5, false}, // too short
		{"deadbeefxy", 4, false}, // non-hex chars
		{"deadbee", 3, false},  // odd length
		{"", 0, false},
	}
	for _, c := range cases {
		if got := looksLikeHex(c.s, c.min); got != c.want {
			t.Errorf("looksLikeHex(%q, %d) = %v; want %v", c.s, c.min, got, c.want)
		}
	}
}

// ── pickDBBinding: hint, auto-pick, ambiguity ────────────────────────

func TestPickDBBinding_AutoPicksWhenExactlyOneMatch(t *testing.T) {
	binds := []*StoredBind{
		{Service: "postgres", Env: map[string]string{"DATABASE_URL": "postgres://x"}},
	}
	bind, kind, err := pickDBBinding(binds, "")
	if err != nil {
		t.Fatal(err)
	}
	if kind != "postgres" || bind.Service != "postgres" {
		t.Errorf("got kind=%s service=%s; want postgres/postgres", kind, bind.Service)
	}
}

func TestPickDBBinding_HintOverridesAutoPick(t *testing.T) {
	binds := []*StoredBind{
		{Service: "postgres", Env: map[string]string{"DATABASE_URL": "postgres://x"}},
		{Service: "mysql", Env: map[string]string{"DATABASE_URL": "mysql://y"}},
	}
	_, kind, err := pickDBBinding(binds, "mysql")
	if err != nil {
		t.Fatal(err)
	}
	if kind != "mysql" {
		t.Errorf("hint=mysql got kind=%s", kind)
	}
}

func TestPickDBBinding_RefusesAmbiguousWithoutHint(t *testing.T) {
	binds := []*StoredBind{
		{Service: "postgres", Env: map[string]string{"DATABASE_URL": "postgres://x"}},
		{Service: "mysql", Env: map[string]string{"DATABASE_URL": "mysql://y"}},
	}
	_, _, err := pickDBBinding(binds, "")
	if err == nil {
		t.Error("expected ambiguity error when multiple DBs are bound without --db")
	}
	if !strings.Contains(err.Error(), "--db") {
		t.Errorf("error should point at --db flag; got %v", err)
	}
}

func TestPickDBBinding_NoBindGivesHelpfulError(t *testing.T) {
	_, _, err := pickDBBinding(nil, "")
	if err == nil {
		t.Fatal("expected error when no DBs bound")
	}
	if !strings.Contains(err.Error(), "service bind") {
		t.Errorf("error should suggest `agentry service bind …`; got %v", err)
	}
}

func TestPickDBBinding_HintForUnboundFamily(t *testing.T) {
	binds := []*StoredBind{
		{Service: "postgres", Env: map[string]string{"DATABASE_URL": "postgres://x"}},
	}
	_, _, err := pickDBBinding(binds, "mysql")
	if err == nil {
		t.Fatal("expected error when hint names an unbound family")
	}
}

// ── dbBindingURL: env-var lookup priority ────────────────────────────

func TestDBBindingURL_PrefersDATABASE_URL(t *testing.T) {
	b := &StoredBind{Env: map[string]string{
		"DATABASE_URL": "primary",
		"POSTGRES_URL": "fallback",
	}}
	if got := dbBindingURL(b); got != "primary" {
		t.Errorf("got %q; want DATABASE_URL value", got)
	}
}

func TestDBBindingURL_FallsBackToPostgresUrl(t *testing.T) {
	b := &StoredBind{Env: map[string]string{
		"POSTGRES_URL": "x",
	}}
	if got := dbBindingURL(b); got != "x" {
		t.Errorf("got %q; want POSTGRES_URL value", got)
	}
}

func TestDBBindingURL_EmptyWhenNoKnownKey(t *testing.T) {
	b := &StoredBind{Env: map[string]string{"FOO": "bar"}}
	if got := dbBindingURL(b); got != "" {
		t.Errorf("got %q; want empty when no known DB-URL key", got)
	}
}

// ── Provider name → env-var conversion ──────────────────────────────

func TestUpperEnvSafe(t *testing.T) {
	cases := map[string]string{
		"google":       "GOOGLE",
		"github":       "GITHUB",
		"generic-oidc": "GENERIC_OIDC",
		"AlReAdY-up":   "ALREADY_UP",
		"":             "",
	}
	for in, want := range cases {
		if got := upperEnvSafe(in); got != want {
			t.Errorf("upperEnvSafe(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestAuthStateProviderEnv_StampsBothFieldsPerProvider(t *testing.T) {
	s := &AuthState{
		Providers: map[string]AuthProviderState{
			"google": {ClientID: "g-id", ClientSecret: "g-secret"},
			"github": {ClientID: "h-id", ClientSecret: "h-secret"},
		},
	}
	got := authStateProviderEnv(s)
	for k, v := range map[string]string{
		"GOOGLE_CLIENT_ID":     "g-id",
		"GOOGLE_CLIENT_SECRET": "g-secret",
		"GITHUB_CLIENT_ID":     "h-id",
		"GITHUB_CLIENT_SECRET": "h-secret",
	} {
		if got[k] != v {
			t.Errorf("env[%q] = %q; want %q", k, got[k], v)
		}
	}
}

func TestAuthStateProviderEnv_HandlesNilSafely(t *testing.T) {
	got := authStateProviderEnv(nil)
	if got == nil {
		t.Error("should return empty map, not nil — caller may iterate")
	}
	if len(got) != 0 {
		t.Errorf("nil state should produce empty env, got %v", got)
	}
}

// ── Provider validation: httptest-backed OIDC discovery + GitHub ────

func TestValidateProvider_OIDCSuccess(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
			http.Error(w, "wrong path", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_endpoint": srvURL + "/authorize",
			"token_endpoint":         srvURL + "/token",
		})
	}))
	srvURL = srv.URL
	defer srv.Close()
	if err := validateProvider(context.Background(), "generic-oidc", srv.URL); err != nil {
		t.Errorf("validateProvider returned err: %v", err)
	}
}

func TestValidateProvider_OIDCMissingAuthorizationEndpoint(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": srvURL,
		})
	}))
	srvURL = srv.URL
	defer srv.Close()
	err := validateProvider(context.Background(), "generic-oidc", srv.URL)
	if err == nil {
		t.Error("expected err when discovery omits authorization_endpoint")
	}
	if !strings.Contains(err.Error(), "authorization_endpoint") {
		t.Errorf("error should name the missing field; got %v", err)
	}
}

func TestValidateProvider_OIDC500SurfacesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "oh no", 500)
	}))
	defer srv.Close()
	err := validateProvider(context.Background(), "generic-oidc", srv.URL)
	if err == nil {
		t.Error("expected err on 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code; got %v", err)
	}
}

func TestValidateProvider_RefusesUnknownProvider(t *testing.T) {
	err := validateProvider(context.Background(), "myspace", "")
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("expected unknown-provider error, got %v", err)
	}
}

func TestValidateProvider_GenericOIDCRequiresIssuer(t *testing.T) {
	err := validateProvider(context.Background(), "generic-oidc", "")
	if err == nil || !strings.Contains(err.Error(), "--issuer") {
		t.Errorf("expected error pointing at --issuer; got %v", err)
	}
}

// ── Fitness dispatch ────────────────────────────────────────────────

func TestFitnessReport_OkRequiresEveryStep(t *testing.T) {
	r := fitnessReport{CanConnect: true, CanCreate: true, CanWrite: true, CanRead: true, CanCleanup: true}
	if !r.Ok() {
		t.Error("all-true report should be Ok")
	}
	for _, mut := range []func(*fitnessReport){
		func(r *fitnessReport) { r.CanConnect = false },
		func(r *fitnessReport) { r.CanCreate = false },
		func(r *fitnessReport) { r.CanWrite = false },
		func(r *fitnessReport) { r.CanRead = false },
		func(r *fitnessReport) { r.CanCleanup = false },
	} {
		r2 := r
		mut(&r2)
		if r2.Ok() {
			t.Errorf("Ok should be false when any step failed: %+v", r2)
		}
	}
}

func TestFitnessReport_DescribePointsAtFailedStep(t *testing.T) {
	r := fitnessReport{}
	if d := r.describe(); !strings.Contains(d, "connect") {
		t.Errorf("connect=false should describe connect failure; got %q", d)
	}
	r.CanConnect = true
	if d := r.describe(); !strings.Contains(d, "create") {
		t.Errorf("create=false should describe create failure; got %q", d)
	}
}

func TestFitnessMongo_UnreachableHostFails(t *testing.T) {
	// Skipped on CI without a mongo server; the dial against a
	// non-resolvable host takes ~30s and we don't want that in unit
	// runs. The real fitness path is exercised in the binary integration
	// tests; here we just confirm an unreachable URL surfaces as an
	// error, not a silent pass.
	if testing.Short() {
		t.Skip("skipping mongo fitness dial in -short mode")
	}
	r := fitnessMongo("mongodb://nonexistent.invalid:27017/agentry?serverSelectionTimeoutMS=1500&connectTimeoutMS=1500")
	if r.Err == nil {
		t.Error("expected unreachable mongo URL to surface an error")
	}
	if r.CanWrite {
		t.Error("CanWrite should be false when the dial failed")
	}
}

func TestDatabaseFromMongoURI(t *testing.T) {
	cases := map[string]string{
		"mongodb://h:27017":              "agentry",
		"mongodb://h:27017/":             "agentry",
		"mongodb://h:27017/myapp":        "myapp",
		"mongodb://h:27017/myapp?w=1":    "myapp",
		"mongodb+srv://h/auth?retry=1":   "auth",
		"bogus":                          "agentry",
	}
	for in, want := range cases {
		if got := databaseFromMongoURI(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestFitnessPostgres_EmptyURL(t *testing.T) {
	if r := fitnessPostgres(""); r.Err == nil {
		t.Error("empty URL should error before opening a connection")
	}
}

func TestFitnessMySQL_EmptyURL(t *testing.T) {
	if r := fitnessMySQL(""); r.Err == nil {
		t.Error("empty URL should error before opening a connection")
	}
}

// ── End-to-end: env-stamping hook stamps AGENTRY_AUTH_* when enabled ─

func TestApplyClusterEnvDefaults_StampsAuthEnvWhenAuthEnabled(t *testing.T) {
	pinAgentryCluster(t, "test", "default")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(saveAuthState("test", "default", &AuthState{
		Enabled:   true,
		DBBinding: "postgres",
		Secret:    strings.Repeat("a", 64),
		Providers: map[string]AuthProviderState{
			"google": {ClientID: "g-id", ClientSecret: "g-secret"},
		},
	}))

	got := captureHookPosts(t, "test", defaultProfile)
	names := map[string]string{}
	for _, b := range got {
		n, _ := b["name"].(string)
		v, _ := b["value"].(string)
		names[n] = v
	}
	if names["AGENTRY_AUTH_ENABLED"] != "true" {
		t.Errorf("AGENTRY_AUTH_ENABLED = %q; want \"true\"", names["AGENTRY_AUTH_ENABLED"])
	}
	if names["AGENTRY_AUTH_DB"] != "postgres" {
		t.Errorf("AGENTRY_AUTH_DB = %q; want postgres", names["AGENTRY_AUTH_DB"])
	}
	if names["AGENTRY_AUTH_SECRET"] != strings.Repeat("a", 64) {
		t.Errorf("AGENTRY_AUTH_SECRET wrong value")
	}
	if names["GOOGLE_CLIENT_ID"] != "g-id" {
		t.Errorf("GOOGLE_CLIENT_ID = %q; want g-id", names["GOOGLE_CLIENT_ID"])
	}
}

func TestApplyClusterEnvDefaults_NoAuthEnv_WhenAuthDisabled(t *testing.T) {
	pinAgentryCluster(t, "test", "default")
	got := captureHookPosts(t, "test", defaultProfile)
	for _, b := range got {
		name, _ := b["name"].(string)
		if strings.HasPrefix(name, "AGENTRY_AUTH_") {
			t.Errorf("unexpected %s stamped when auth disabled", name)
		}
	}
}

// captureHookPosts spins up a fake provisioner that accepts every
// /api/sandboxes/{id}/secrets POST and returns the parsed bodies.
// Used by tests that want to assert on the full set of env vars the
// hook stamps in one shot.
func captureHookPosts(t *testing.T, cluster, profile string) []map[string]any {
	t.Helper()
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		got = append(got, b)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: &hostRewriter{base: srv.URL, wrapped: srv.Client().Transport}}
	hook := applyClusterEnvDefaults(constCtx(cluster, profile), hc)
	if err := hook(context.Background(), mcp.SandboxInfo{SandboxID: "sb_1"}); err != nil {
		t.Fatalf("hook returned err: %v", err)
	}
	return got
}

// ── authStatusTo render checks ───────────────────────────────────────

func TestAuthStatusTo_DisabledHasNextSteps(t *testing.T) {
	var buf bytes.Buffer
	authStatusTo(&buf, "test", "dev", nil)
	out := buf.String()
	for _, want := range []string{"DISABLED", "agentry service bind postgres", "agentry auth enable"} {
		if !strings.Contains(out, want) {
			t.Errorf("disabled status missing %q; got:\n%s", want, out)
		}
	}
}

func TestAuthStatusTo_EnabledShowsBindAndProviders(t *testing.T) {
	var buf bytes.Buffer
	authStatusTo(&buf, "test", "dev", &AuthState{
		Enabled:   true,
		DBBinding: "postgres",
		Secret:    strings.Repeat("a", 64),
		Providers: map[string]AuthProviderState{"google": {ClientID: "g"}},
	})
	out := buf.String()
	for _, want := range []string{"ENABLED", "db binding: postgres", "google"} {
		if !strings.Contains(out, want) {
			t.Errorf("enabled status missing %q; got:\n%s", want, out)
		}
	}
	// Secret value should NEVER appear, only its length.
	if strings.Contains(out, strings.Repeat("a", 64)) {
		t.Errorf("auth status leaked the AUTH_SECRET into output")
	}
}

func TestAuthProviderListTo_EmptyHasGuidance(t *testing.T) {
	var buf bytes.Buffer
	authProviderListTo(&buf, "test", "dev", &AuthState{Enabled: true})
	out := buf.String()
	if !strings.Contains(out, "Add one with") {
		t.Errorf("empty list should suggest `agentry auth providers add`; got:\n%s", out)
	}
}

func TestAuthProviderListTo_TableShape(t *testing.T) {
	var buf bytes.Buffer
	authProviderListTo(&buf, "test", "dev", &AuthState{
		Enabled: true,
		Providers: map[string]AuthProviderState{
			"google": {ClientID: "g-id", Scopes: []string{"openid", "email"}},
			"github": {ClientID: "h-id"},
		},
	})
	out := buf.String()
	for _, h := range []string{"PROVIDER", "CLIENT ID", "SCOPES"} {
		if !strings.Contains(out, h) {
			t.Errorf("table missing header %q; got:\n%s", h, out)
		}
	}
	// Client secrets must NEVER appear in the table.
	if strings.Contains(out, "client_secret") {
		t.Errorf("provider list leaked the literal key client_secret")
	}
}

