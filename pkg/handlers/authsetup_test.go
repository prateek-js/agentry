package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the agentry_auth_setup endpoint. The auth scaffold is
// the one piece of generated code we MUST keep deterministic — if
// these regress, every newly-scaffolded app inherits the bug, and
// auth bugs ship undetected for weeks.
//
// What's asserted:
//
//  1. Probe (mode="") reports framework + lists every backend + sets
//     Available correctly based on whether the binding directory is
//     mounted in the test FS.
//  2. Scaffold (mode="sqlite") writes ALL six files and adds
//     better-auth + better-sqlite3 to package.json.
//  3. Scaffold for binding:postgres adds pg (not better-sqlite3),
//     and the auth.ts template references DATABASE_URL.
//  4. Scaffold for binding:mongodb adds mongodb and references
//     MONGODB_URL via the mongodbAdapter.
//  5. Idempotency: re-running with mode set after a previous scaffold
//     does NOT re-scaffold — it reports AlreadyConfigured + the
//     current mode.
//  6. mode="none" returns immediately and writes no files.
//  7. Bad inputs surface a 4xx (unknown mode, missing project,
//     wrong-framework package.json).
//  8. .env.local additions are idempotent — re-running won't duplicate
//     the BETTER_AUTH_SECRET line.

// withFakeWorkspace stages an isolated /workspace-shaped tree under
// t.TempDir() and rewrites the handler's hardcoded /workspace/projects
// paths via /workspace symlink trickery. Since handlers.AuthSetupHandler
// hardcodes "/workspace/projects/..." we can't trivially redirect it
// per-test — instead we change CWD into the temp dir and override
// the path using a build-tag-free hook in the test only.
//
// We get the same effect by writing into the SAME "/workspace" the
// handler reads from, but under t.TempDir() acting AS "/workspace".
// To avoid touching the real filesystem we rewrite the handler's path
// reads through a symlink chain — but the cleanest path is just to
// make /workspace test-injectable. Since this is the FIRST test that
// needs it, we accept the small refactor: hoist the workspace root
// behind a package var the test can set.
//
// The hoisting lives in authsetup.go (workspaceRoot var). Tests
// override it via the helper below.

// stageProject writes a minimal Next.js package.json under <root>/projects/<name>/.
func stageProject(t *testing.T, root, name string, pkg map[string]any) string {
	t.Helper()
	dir := filepath.Join(root, "projects", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(pkg, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func nextPkg() map[string]any {
	return map[string]any{
		"name":    "app",
		"version": "0.0.1",
		"dependencies": map[string]any{
			"next":      "^15.0.0",
			"react":     "^19.0.0",
			"react-dom": "^19.0.0",
		},
	}
}

// callAuth performs an HTTP call against AuthSetupHandler with the
// given body and returns the parsed response.
func callAuth(t *testing.T, body authSetupRequest) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/setup", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	AuthSetupHandler(w, req)
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("response not JSON (status=%d body=%s)", w.Code, w.Body.String())
		}
	}
	return w, out
}

// pinWorkspaceRoot points the package's workspace lookup at a test
// temp dir for the duration of the test. Returns the dir's
// /workspace-equivalent root (the parent of /projects).
func pinWorkspaceRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// Symlink t.TempDir()/projects → t.TempDir()/projects (no-op);
	// we instead point the test-controlled hook below.
	prev := workspaceRoot
	workspaceRoot = root
	t.Cleanup(func() { workspaceRoot = prev })
	return root
}

// pinBindingsRoot lets the test fake /var/run/agentry/<service>/ so
// availability flags on the probe response can be asserted without
// touching the host FS.
func pinBindingsRoot(t *testing.T, mounted ...string) {
	t.Helper()
	root := t.TempDir()
	for _, svc := range mounted {
		if err := os.MkdirAll(filepath.Join(root, svc), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev := bindingsRoot
	bindingsRoot = root
	t.Cleanup(func() { bindingsRoot = prev })
}

// pinSQLiteDataDir relocates /workspace/.agentry to a test temp dir
// so scaffolding sqlite mode doesn't try to mkdir on the real FS.
func pinSQLiteDataDir(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	prev := sqliteDataDir
	sqliteDataDir = root
	t.Cleanup(func() { sqliteDataDir = prev })
}

func TestAuthSetup_Probe_NextProject(t *testing.T) {
	root := pinWorkspaceRoot(t)
	pinBindingsRoot(t, "postgres") // postgres mounted, mongo NOT
	stageProject(t, root, "app", nextPkg())

	w, out := callAuth(t, authSetupRequest{Project: "app"})
	if w.Code != 200 {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if out["phase"] != "probe" {
		t.Errorf("phase = %v; want probe", out["phase"])
	}
	if out["framework"] != "next" {
		t.Errorf("framework = %v; want next", out["framework"])
	}
	if out["already_configured"] != false {
		t.Errorf("already_configured = %v; want false", out["already_configured"])
	}
	// available_modes is []authModeOption; the JSON round-trip turns
	// it into []any of map[string]any.
	modes, ok := out["available_modes"].([]any)
	if !ok || len(modes) != 4 {
		t.Fatalf("available_modes shape = %T len=%d", out["available_modes"], len(modes))
	}
	wantMap := map[string]bool{
		"none":             true,
		"sqlite":           true,
		"binding:postgres": true,
		"binding:mongodb":  false,
	}
	for _, m := range modes {
		mm := m.(map[string]any)
		want := wantMap[mm["id"].(string)]
		if mm["available"].(bool) != want {
			t.Errorf("mode %q available = %v; want %v", mm["id"], mm["available"], want)
		}
	}
	if q, _ := out["question_prompt"].(string); !strings.Contains(q, "auth") {
		t.Errorf("question_prompt missing 'auth': %q", q)
	}
}

func TestAuthSetup_Scaffold_SQLite(t *testing.T) {
	root := pinWorkspaceRoot(t)
	pinSQLiteDataDir(t)
	pinBindingsRoot(t)
	dir := stageProject(t, root, "app", nextPkg())

	w, out := callAuth(t, authSetupRequest{Project: "app", Mode: "sqlite"})
	if w.Code != 200 {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if out["phase"] != "scaffold" {
		t.Errorf("phase = %v; want scaffold", out["phase"])
	}
	if out["mode"] != "sqlite" {
		t.Errorf("mode = %v; want sqlite", out["mode"])
	}
	// Every template file should land on disk.
	for _, rel := range []string{
		"src/lib/auth.ts",
		"src/lib/auth-schema.ts",
		"src/lib/auth-client.ts",
		"src/app/api/auth/[...all]/route.ts",
		"src/app/sign-in/page.tsx",
		"src/app/sign-up/page.tsx",
		".env.local",
		".npmrc",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("missing scaffolded file %s: %v", rel, err)
		}
	}
	// .npmrc must enable legacy-peer-deps so React 19 RC peer-rejects
	// don't blow up `npm install better-auth`.
	if rc, _ := os.ReadFile(filepath.Join(dir, ".npmrc")); !strings.Contains(string(rc), "legacy-peer-deps=true") {
		t.Errorf(".npmrc missing legacy-peer-deps=true:\n%s", rc)
	}
	// auth.ts must import the kysely adapter from the NEW location
	// (the better-auth/adapters/kysely path is gone since 1.6).
	authBody, _ := os.ReadFile(filepath.Join(dir, "src/lib/auth.ts"))
	if !strings.Contains(string(authBody), "@better-auth/kysely-adapter") {
		t.Errorf("auth.ts must import @better-auth/kysely-adapter (not better-auth/adapters/kysely):\n%s", authBody)
	}
	// auth.ts must NOT hardcode a localhost URL — the wrapper reads
	// X-Forwarded-Host at request time instead.
	if strings.Contains(string(authBody), `"http://localhost:3000"`) {
		t.Errorf("auth.ts hardcodes http://localhost:3000 (should derive from X-Forwarded-Host):\n%s", authBody)
	}
	// .env.local must NOT set BETTER_AUTH_URL=http://localhost:3000
	// — that's the bug that caused Issue 9 (sign-in loops back).
	envBody, _ := os.ReadFile(filepath.Join(dir, ".env.local"))
	if strings.Contains(string(envBody), "BETTER_AUTH_URL=http://localhost") {
		t.Errorf(".env.local hardcodes BETTER_AUTH_URL=http://localhost (should be unset):\n%s", envBody)
	}
	// auth.ts must reference better-sqlite3 (template selected sqlite branch).
	b, err := os.ReadFile(filepath.Join(dir, "src/lib/auth.ts"))
	if err != nil {
		t.Fatalf("read auth.ts: %v", err)
	}
	body := string(b)
	if !strings.Contains(body, "better-sqlite3") {
		t.Errorf("auth.ts missing better-sqlite3 import:\n%s", body)
	}
	if strings.Contains(body, `from "pg"`) || strings.Contains(body, "mongodbAdapter") {
		t.Errorf("auth.ts contains wrong-storage references:\n%s", body)
	}
	// package.json must have better-auth + better-sqlite3, no pg/mongo.
	pkg, _ := readPackageJSON(dir)
	deps := mergeDeps(pkg)
	if _, ok := deps["better-auth"]; !ok {
		t.Errorf("better-auth missing from package.json deps")
	}
	if _, ok := deps["better-sqlite3"]; !ok {
		t.Errorf("better-sqlite3 missing from package.json deps")
	}
	if _, ok := deps["pg"]; ok {
		t.Errorf("pg should NOT be in package.json deps for sqlite mode")
	}
	// .env.local has a secret line.
	envB, _ := os.ReadFile(filepath.Join(dir, ".env.local"))
	if !strings.Contains(string(envB), "BETTER_AUTH_SECRET=") {
		t.Errorf(".env.local missing BETTER_AUTH_SECRET")
	}
	// commands_to_run + next_steps are non-empty.
	if cmds, _ := out["commands_to_run"].([]any); len(cmds) == 0 {
		t.Errorf("commands_to_run empty")
	}
	if steps, _ := out["next_steps"].([]any); len(steps) == 0 {
		t.Errorf("next_steps empty")
	}
}

func TestAuthSetup_Scaffold_BindingPostgres(t *testing.T) {
	root := pinWorkspaceRoot(t)
	pinBindingsRoot(t, "postgres")
	dir := stageProject(t, root, "app", nextPkg())

	w, _ := callAuth(t, authSetupRequest{Project: "app", Mode: "binding:postgres"})
	if w.Code != 200 {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "src/lib/auth.ts"))
	if !strings.Contains(string(b), `from "pg"`) {
		t.Errorf("auth.ts missing pg import:\n%s", b)
	}
	if !strings.Contains(string(b), "process.env.DATABASE_URL") {
		t.Errorf("auth.ts missing DATABASE_URL ref:\n%s", b)
	}
	pkg, _ := readPackageJSON(dir)
	deps := mergeDeps(pkg)
	if _, ok := deps["pg"]; !ok {
		t.Errorf("pg missing from package.json deps")
	}
	if _, ok := deps["better-sqlite3"]; ok {
		t.Errorf("better-sqlite3 leaked into binding:postgres scaffold")
	}
}

func TestAuthSetup_Scaffold_BindingMongo(t *testing.T) {
	root := pinWorkspaceRoot(t)
	pinBindingsRoot(t, "mongodb")
	dir := stageProject(t, root, "app", nextPkg())

	w, _ := callAuth(t, authSetupRequest{Project: "app", Mode: "binding:mongodb"})
	if w.Code != 200 {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "src/lib/auth.ts"))
	if !strings.Contains(string(b), "mongodbAdapter") {
		t.Errorf("auth.ts missing mongodbAdapter:\n%s", b)
	}
	if !strings.Contains(string(b), "process.env.MONGODB_URL") {
		t.Errorf("auth.ts missing MONGODB_URL ref:\n%s", b)
	}
}

func TestAuthSetup_Idempotent_AlreadyConfigured(t *testing.T) {
	root := pinWorkspaceRoot(t)
	pinSQLiteDataDir(t)
	pinBindingsRoot(t)
	dir := stageProject(t, root, "app", nextPkg())

	// First call wires sqlite.
	w1, _ := callAuth(t, authSetupRequest{Project: "app", Mode: "sqlite"})
	if w1.Code != 200 {
		t.Fatalf("first call status = %d; body=%s", w1.Code, w1.Body.String())
	}
	// Snapshot auth.ts so we can confirm it's unchanged after re-run.
	authTSPath := filepath.Join(dir, "src/lib/auth.ts")
	before, _ := os.ReadFile(authTSPath)

	// Second call (still sqlite). Should NOT re-scaffold.
	w2, out2 := callAuth(t, authSetupRequest{Project: "app", Mode: "sqlite"})
	if w2.Code != 200 {
		t.Fatalf("second call status = %d; body=%s", w2.Code, w2.Body.String())
	}
	if out2["already_configured"] != true {
		t.Errorf("already_configured = %v; want true", out2["already_configured"])
	}
	if out2["current_mode"] != "sqlite" {
		t.Errorf("current_mode = %v; want sqlite", out2["current_mode"])
	}
	if files, _ := out2["files_written"].([]any); len(files) != 0 {
		t.Errorf("files_written should be empty on idempotent re-run; got %v", files)
	}
	after, _ := os.ReadFile(authTSPath)
	if string(before) != string(after) {
		t.Errorf("auth.ts changed on re-run — idempotency broken")
	}
}

func TestAuthSetup_ModeNone_NoOp(t *testing.T) {
	root := pinWorkspaceRoot(t)
	pinBindingsRoot(t)
	dir := stageProject(t, root, "app", nextPkg())

	w, out := callAuth(t, authSetupRequest{Project: "app", Mode: "none"})
	if w.Code != 200 {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if out["mode"] != "none" {
		t.Errorf("mode = %v; want none", out["mode"])
	}
	// No auth files should land on disk.
	if _, err := os.Stat(filepath.Join(dir, "src/lib/auth.ts")); !os.IsNotExist(err) {
		t.Errorf("auth.ts created for mode=none: %v", err)
	}
	pkg, _ := readPackageJSON(dir)
	if _, ok := mergeDeps(pkg)["better-auth"]; ok {
		t.Errorf("better-auth added to package.json for mode=none")
	}
}

func TestAuthSetup_BadMode(t *testing.T) {
	root := pinWorkspaceRoot(t)
	pinBindingsRoot(t)
	stageProject(t, root, "app", nextPkg())

	w, _ := callAuth(t, authSetupRequest{Project: "app", Mode: "magic-link"})
	if w.Code != 400 {
		t.Errorf("bad mode status = %d; want 400", w.Code)
	}
}

func TestAuthSetup_MissingProject(t *testing.T) {
	pinWorkspaceRoot(t)
	pinBindingsRoot(t)

	w, _ := callAuth(t, authSetupRequest{Project: "nope"})
	if w.Code != 404 {
		t.Errorf("missing project status = %d; want 404", w.Code)
	}
}

func TestAuthSetup_NonNextFramework_Rejected(t *testing.T) {
	root := pinWorkspaceRoot(t)
	pinBindingsRoot(t)
	stageProject(t, root, "app", map[string]any{
		"name":    "vibe-cli",
		"version": "0.0.1",
		"dependencies": map[string]any{
			"commander": "^12.0.0",
		},
	})

	w, _ := callAuth(t, authSetupRequest{Project: "app", Mode: "sqlite"})
	if w.Code != 400 {
		t.Errorf("non-next framework status = %d; want 400 (V1 is Next-only)", w.Code)
	}
}

func TestAuthSetup_EnvLocal_Idempotent(t *testing.T) {
	root := pinWorkspaceRoot(t)
	pinSQLiteDataDir(t)
	pinBindingsRoot(t)
	dir := stageProject(t, root, "app", nextPkg())
	// Pre-existing env with unrelated vars + a pre-existing
	// BETTER_AUTH_SECRET (user already set one). Scaffold MUST NOT
	// overwrite it.
	envPath := filepath.Join(dir, ".env.local")
	preExisting := "FOO=bar\nBETTER_AUTH_SECRET=user-set-secret\n"
	if err := os.WriteFile(envPath, []byte(preExisting), 0o600); err != nil {
		t.Fatal(err)
	}

	w, _ := callAuth(t, authSetupRequest{Project: "app", Mode: "sqlite"})
	if w.Code != 200 {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	after, _ := os.ReadFile(envPath)
	if !strings.Contains(string(after), "FOO=bar") {
		t.Errorf("pre-existing FOO=bar was clobbered:\n%s", after)
	}
	if !strings.Contains(string(after), "BETTER_AUTH_SECRET=user-set-secret") {
		t.Errorf("user's BETTER_AUTH_SECRET was overwritten:\n%s", after)
	}
	// Count BETTER_AUTH_SECRET lines — must be exactly one.
	if c := strings.Count(string(after), "BETTER_AUTH_SECRET="); c != 1 {
		t.Errorf("BETTER_AUTH_SECRET appears %d times; want 1", c)
	}
}
