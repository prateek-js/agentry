package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for profile.go — the operator-side, cluster-scoped config
// slice. Storage roundtrips, the legacy → default migration, the
// rendering helpers (listProfilesTo / showProfileTo), and the
// resolution-fallback chain (override → config → default).

// pinProfileConfig stages a fake config root and pins the active
// cluster. Returns the base dir so the test can poke at on-disk
// state directly. Different from pinAgentryConfig only in that it
// also resets the migrationOnce gate so each test runs migration
// fresh.
func pinProfileConfig(t *testing.T, cluster, profile string) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agentry.json")
	body := `{"cluster":"` + cluster + `","profile":"` + profile + `"}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTRY_CONFIG", cfgPath)
	resetMigrationOnce()
	return dir
}

// ── resolveProfile: the fallback chain ──────────────────────────────────

func TestResolveProfile_FallbackChain(t *testing.T) {
	cases := []struct {
		name, profile, override, want string
	}{
		{"override wins over config", "dev", "prod", "prod"},
		{"config used when no override", "dev", "", "dev"},
		{"default when both empty", "", "", defaultProfile},
		{"nil config falls to default", "<nil>", "", defaultProfile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg *Config
			if tc.profile != "<nil>" {
				cfg = &Config{Profile: tc.profile}
			}
			if got := resolveProfile(cfg, tc.override); got != tc.want {
				t.Errorf("resolveProfile(%+v, %q) = %q; want %q", cfg, tc.override, got, tc.want)
			}
		})
	}
}

// ── validateProfileName: refuse path-component attacks ─────────────────

func TestValidateProfileName(t *testing.T) {
	good := []string{"dev", "prod", "staging", "ash-prod", "release_1"}
	bad := []string{"", ".", "..", "a/b", `a\b`, ".hidden"}
	for _, n := range good {
		if err := validateProfileName(n); err != nil {
			t.Errorf("good name %q rejected: %v", n, err)
		}
	}
	for _, n := range bad {
		if err := validateProfileName(n); err == nil {
			t.Errorf("bad name %q accepted", n)
		}
	}
}

// ── Legacy migration ────────────────────────────────────────────────────

// TestMigrateLegacyLayout_MovesJSONsIntoDefault verifies the one-time
// migration: pre-profile clusters had <cluster>/<service>.json directly;
// after migration those files live under <cluster>/default/. Files
// that were already inside a profile dir don't move.
func TestMigrateLegacyLayout_MovesJSONsIntoDefault(t *testing.T) {
	base := pinProfileConfig(t, "test", "")
	// Legacy file: services/test/postgres.json (no profile dir).
	legacyDir := filepath.Join(base, "services", "test")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "postgres.json"), []byte(`{"service":"postgres"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Already-profile file: services/test/dev/redis.json should be
	// left where it is (dev profile already has it).
	devDir := filepath.Join(legacyDir, "dev")
	if err := os.MkdirAll(devDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "redis.json"), []byte(`{"service":"redis"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Run migration by touching the profile-aware path constructor.
	_ = bindsDir("test", defaultProfile)

	// Legacy file should now live under default/.
	if _, err := os.Stat(filepath.Join(legacyDir, defaultProfile, "postgres.json")); err != nil {
		t.Errorf("legacy postgres.json should have moved to default/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, "postgres.json")); !os.IsNotExist(err) {
		t.Errorf("legacy postgres.json still at old location: %v", err)
	}
	// dev/redis.json should be untouched.
	if _, err := os.Stat(filepath.Join(devDir, "redis.json")); err != nil {
		t.Errorf("dev/redis.json should be unchanged: %v", err)
	}
}

// TestMigrateLegacyLayout_RefusesToClobber: if both a legacy file
// AND a same-named file in default/ exist, the migration must NOT
// overwrite the profile file. The legacy one is left behind for the
// operator to deal with manually.
func TestMigrateLegacyLayout_RefusesToClobber(t *testing.T) {
	base := pinProfileConfig(t, "test", "")
	clusterDir := filepath.Join(base, "services", "test")
	if err := os.MkdirAll(clusterDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clusterDir, "postgres.json"), []byte(`legacy`), 0o600); err != nil {
		t.Fatal(err)
	}
	defDir := filepath.Join(clusterDir, defaultProfile)
	if err := os.MkdirAll(defDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defDir, "postgres.json"), []byte(`new`), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = bindsDir("test", defaultProfile)

	got, _ := os.ReadFile(filepath.Join(defDir, "postgres.json"))
	if string(got) != "new" {
		t.Errorf("default/postgres.json content = %q; want %q (must not clobber existing profile file)", string(got), "new")
	}
}

// ── End-to-end roundtrip via the renamed signatures ───────────────────

func TestSaveEnv_ProfileIsolation(t *testing.T) {
	pinProfileConfig(t, "test", "default")
	if err := saveEnv("test", "dev", &StoredEnv{Name: "DATABASE_URL", Value: "dev-url"}); err != nil {
		t.Fatal(err)
	}
	if err := saveEnv("test", "prod", &StoredEnv{Name: "DATABASE_URL", Value: "prod-url"}); err != nil {
		t.Fatal(err)
	}
	dev, err := loadEnv("test", "dev", "DATABASE_URL")
	if err != nil || dev == nil {
		t.Fatalf("dev load: %v", err)
	}
	prod, err := loadEnv("test", "prod", "DATABASE_URL")
	if err != nil || prod == nil {
		t.Fatalf("prod load: %v", err)
	}
	if dev.Value == prod.Value {
		t.Errorf("dev and prod values bled across profiles: both=%q", dev.Value)
	}
	if dev.Value != "dev-url" || prod.Value != "prod-url" {
		t.Errorf("dev=%q prod=%q; profile keying didn't roundtrip", dev.Value, prod.Value)
	}
}

func TestSaveBind_ProfileIsolation(t *testing.T) {
	pinProfileConfig(t, "test", "default")
	if err := saveBind("test", "dev", &StoredBind{Service: "postgres", Env: map[string]string{"DATABASE_URL": "dev"}}); err != nil {
		t.Fatal(err)
	}
	if err := saveBind("test", "prod", &StoredBind{Service: "postgres", Env: map[string]string{"DATABASE_URL": "prod"}}); err != nil {
		t.Fatal(err)
	}
	dev, _ := loadBind("test", "dev", "postgres")
	prod, _ := loadBind("test", "prod", "postgres")
	if dev == nil || prod == nil {
		t.Fatalf("missing one side: dev=%v prod=%v", dev, prod)
	}
	if dev.Env["DATABASE_URL"] == prod.Env["DATABASE_URL"] {
		t.Errorf("bind values bled across profiles")
	}
}

// ── Render helpers (listProfilesTo, showProfileTo) ────────────────────

// TestListProfilesTo_EmptyState explains itself when there's nothing
// staged. The "no profiles" message must include `agentry profile
// create` so a new operator knows what to do next.
func TestListProfilesTo_EmptyState(t *testing.T) {
	pinProfileConfig(t, "test", "default")
	var buf bytes.Buffer
	if code := listProfilesTo(&buf); code != 0 {
		t.Errorf("exit code = %d; want 0 on empty state", code)
	}
	out := buf.String()
	for _, want := range []string{"no profiles staged", "agentry profile create"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty-state output missing %q; got:\n%s", want, out)
		}
	}
}

// TestListProfilesTo_TableShape ensures the table renders the
// canonical columns and marks the active row with "*". Counts come
// from real on-disk content.
func TestListProfilesTo_TableShape(t *testing.T) {
	pinProfileConfig(t, "test", "dev")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(saveEnv("test", "dev", &StoredEnv{Name: "A", Value: "x"}))
	must(saveEnv("test", "dev", &StoredEnv{Name: "B", Value: "y"}))
	must(saveBind("test", "dev", &StoredBind{Service: "postgres", Env: map[string]string{"X": "y"}}))
	must(saveEnv("test", "prod", &StoredEnv{Name: "C", Value: "z"}))

	var buf bytes.Buffer
	if code := listProfilesTo(&buf); code != 0 {
		t.Fatalf("listProfilesTo exit = %d", code)
	}
	out := buf.String()
	for _, header := range []string{"ACTIVE", "PROFILE", "CLUSTER", "ENVS", "BINDS"} {
		if !strings.Contains(out, header) {
			t.Errorf("header %q missing from listing:\n%s", header, out)
		}
	}
	// dev should be active (marked *), prod should not.
	lines := strings.Split(out, "\n")
	var devLine, prodLine string
	for _, l := range lines {
		if strings.Contains(l, " dev ") {
			devLine = l
		}
		if strings.Contains(l, " prod ") {
			prodLine = l
		}
	}
	if !strings.HasPrefix(devLine, "*") {
		t.Errorf("dev line should start with '*' (active marker); got %q", devLine)
	}
	if strings.HasPrefix(prodLine, "*") {
		t.Errorf("prod line should NOT be marked active; got %q", prodLine)
	}
	// dev has 2 envs + 1 bind; prod has 1 env + 0 binds.
	if !strings.Contains(devLine, "2") || !strings.Contains(devLine, "1") {
		t.Errorf("dev counts wrong in: %q", devLine)
	}
}

func TestShowProfileTo_EmptyProfile(t *testing.T) {
	pinProfileConfig(t, "test", "dev")
	var buf bytes.Buffer
	if code := showProfileTo(&buf, "test", "dev"); code != 0 {
		t.Errorf("exit code = %d", code)
	}
	out := buf.String()
	for _, want := range []string{
		"Profile:  dev",
		"Cluster:  test",
		"Envs   (0)",
		"Binds  (0)",
		"agentry env set",
		"agentry service bind",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q; got:\n%s", want, out)
		}
	}
}

func TestShowProfileTo_PopulatedProfile(t *testing.T) {
	pinProfileConfig(t, "test", "dev")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(saveEnv("test", "dev", &StoredEnv{Name: "DATABASE_URL", Value: "x"}))
	must(saveEnv("test", "dev", &StoredEnv{Name: "AUTH_SECRET", Value: "y"}))
	must(saveBind("test", "dev", &StoredBind{Service: "postgres", Env: map[string]string{"DATABASE_URL": "x"}}))

	var buf bytes.Buffer
	showProfileTo(&buf, "test", "dev")
	out := buf.String()
	// Lists names but never values.
	for _, want := range []string{"DATABASE_URL", "AUTH_SECRET", "postgres", "Envs   (2)", "Binds  (1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in show output:\n%s", want, out)
		}
	}
	// Critical: values should NOT leak into the display.
	for _, leak := range []string{`"value"`, `"x"`, `"y"`} {
		if strings.Contains(out, leak) {
			t.Errorf("show leaked %q to output:\n%s", leak, out)
		}
	}
}

// ── clusterAndProfile bridges per-request cluster reads + profile config ──

func TestClusterAndProfile_ReadsFreshConfigEachCall(t *testing.T) {
	pinProfileConfig(t, "test", "dev")
	bridge := clusterAndProfile(func() string { return "test" })
	c, p := bridge()
	if c != "test" || p != "dev" {
		t.Errorf("first call: got (%q,%q); want (test,dev)", c, p)
	}

	// Rewrite the config to switch to prod. The bridge should pick
	// it up on the very next call — that's what lets `agentry
	// profile use prod` take effect mid-stdio without restart.
	cfgPath := os.Getenv("AGENTRY_CONFIG")
	if err := os.WriteFile(cfgPath, []byte(`{"cluster":"test","profile":"prod"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, p = bridge()
	if c != "test" || p != "prod" {
		t.Errorf("after profile switch: got (%q,%q); want (test,prod)", c, p)
	}
}

func TestClusterAndProfile_ConfigMissingFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	// Point AGENTRY_CONFIG at a file that doesn't exist.
	t.Setenv("AGENTRY_CONFIG", filepath.Join(dir, "agentry.json"))
	resetMigrationOnce()
	bridge := clusterAndProfile(func() string { return "cl" })
	c, p := bridge()
	if c != "cl" {
		t.Errorf("cluster = %q; want cl (should still come through despite missing config)", c)
	}
	if p != defaultProfile {
		t.Errorf("profile = %q; want %s (fallback)", p, defaultProfile)
	}
}

// TestClusterAndProfile_NilGetterReturnsEmptyCluster: defensive — the
// stdio loop ever calling with a nil getter shouldn't panic; the hook
// gracefully skips when cluster is empty.
func TestClusterAndProfile_NilGetterReturnsEmptyCluster(t *testing.T) {
	pinProfileConfig(t, "ignored", "dev")
	c, p := clusterAndProfile(nil)()
	if c != "" {
		t.Errorf("cluster = %q; want empty", c)
	}
	if p != "dev" {
		t.Errorf("profile = %q; want dev", p)
	}
}
