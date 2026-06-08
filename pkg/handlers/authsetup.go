package handlers

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

// Package-level embed for the auth scaffold templates. Baked in at
// build time so the runtime image carries one canonical version of
// every file the auth setup writes. agentry_auth_setup MCP tool ←
// runtime /v1/auth/setup ← these.
//
//go:embed authsetup_templates
var authSetupTemplates embed.FS

// Test-overridable roots. Production values are the canonical
// in-sandbox paths; tests rebind them to t.TempDir() via the helpers
// in authsetup_test.go.
//
// workspaceRoot is the parent of "projects/" — handler resolves a
// project as filepath.Join(workspaceRoot, "projects", name).
// bindingsRoot is where service_bind drops <service>/ directories;
// the probe uses presence-of-directory to flag binding availability.
// sqliteDataDir is the dir scaffold ensures exists before SQLite mode
// boots (the adapter creates the .db file but needs the parent dir).
var (
	workspaceRoot = "/workspace"
	bindingsRoot  = "/var/run/agentry"
	sqliteDataDir = "/workspace/.agentry"
)

// authSetupRequest is the body of POST /v1/auth/setup.
type authSetupRequest struct {
	// Project name under /workspace/projects/. Default: "app" (matches
	// the canonical layout app.md prescribes).
	Project string `json:"project,omitempty"`
	// Mode picks the scaffold path:
	//   "" (empty)         → phase 1: probe + return questions envelope
	//   "none"             → no auth wired; idempotent no-op
	//   "sqlite"           → write Better-Auth files with sqlite adapter
	//   "binding:postgres" → use process.env.DATABASE_URL
	//   "binding:mongodb"  → use process.env.MONGODB_URL
	Mode string `json:"mode,omitempty"`
}

// authSetupResponse covers both phases. Probe responses set
// Phase="probe"; scaffold responses set Phase="scaffold". The shape
// stays flat so the MCP tool returns it verbatim to the LLM.
type authSetupResponse struct {
	Phase     string `json:"phase"`
	Project   string `json:"project"`
	ProjectAt string `json:"project_path"`
	Framework string `json:"framework"`
	// AlreadyConfigured fires when Better-Auth is already in
	// package.json. The handler stops short of overwriting files in
	// that case; the LLM can tell the user "auth's already wired".
	AlreadyConfigured bool   `json:"already_configured"`
	CurrentMode       string `json:"current_mode,omitempty"`

	// Probe-phase output. Populated when Mode was empty in the request.
	AvailableModes []authModeOption `json:"available_modes,omitempty"`
	QuestionPrompt string           `json:"question_prompt,omitempty"`

	// Scaffold-phase output. Populated when Mode was set.
	Mode          string   `json:"mode,omitempty"`
	FilesWritten  []string `json:"files_written,omitempty"`
	PackageAdded  []string `json:"package_added,omitempty"`
	CommandsToRun []string `json:"commands_to_run,omitempty"`
	NextSteps     []string `json:"next_steps,omitempty"`
}

// authModeOption is one entry in the probe-phase menu.
type authModeOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Available   bool   `json:"available"`
}

// AuthSetupHandler implements GET/POST /v1/auth/setup. Two-phase
// flow so the LLM can ask the user first, then act. Idempotent —
// running it twice on the same project doesn't double-scaffold.
func AuthSetupHandler(w http.ResponseWriter, r *http.Request) {
	var req authSetupRequest
	if r.Method == http.MethodPost && r.ContentLength > 0 {
		if err := DecodeJSON(r, &req); err != nil {
			Error(w, http.StatusBadRequest, "bad body: "+err.Error())
			return
		}
	}
	if req.Project == "" {
		req.Project = "app"
	}
	projectAt := filepath.Join(workspaceRoot, "projects", req.Project)
	if _, err := os.Stat(projectAt); err != nil {
		Error(w, http.StatusNotFound,
			fmt.Sprintf("project %q does not exist at %s — run sandbox_create + scaffold first",
				req.Project, projectAt))
		return
	}

	framework, _ := detectFramework(projectAt)
	if framework != "next" {
		Error(w, http.StatusBadRequest,
			fmt.Sprintf("auth_setup currently supports Next.js App Router only (detected: %q)",
				framework))
		return
	}

	configured, currentMode := detectExistingAuth(projectAt)

	switch req.Mode {
	case "":
		// Phase 1 — probe.
		JSON(w, http.StatusOK, authSetupResponse{
			Phase:             "probe",
			Project:           req.Project,
			ProjectAt:         projectAt,
			Framework:         framework,
			AlreadyConfigured: configured,
			CurrentMode:       currentMode,
			AvailableModes:    listAvailableModes(),
			QuestionPrompt:    buildQuestionPrompt(configured, currentMode),
		})
		return
	case "none":
		// Idempotent no-op. We don't write files for "none"; the LLM
		// reports back to the user that auth was skipped.
		JSON(w, http.StatusOK, authSetupResponse{
			Phase:             "scaffold",
			Project:           req.Project,
			ProjectAt:         projectAt,
			Framework:         framework,
			Mode:              "none",
			AlreadyConfigured: configured,
			CurrentMode:       currentMode,
			NextSteps:         []string{"No auth wired. Re-run agentry_auth_setup later to enable."},
		})
		return
	case "sqlite", "binding:postgres", "binding:mongodb":
		// fall through to scaffold below
	default:
		Error(w, http.StatusBadRequest,
			fmt.Sprintf("mode must be one of: none, sqlite, binding:postgres, binding:mongodb (got %q)",
				req.Mode))
		return
	}

	if configured {
		// Idempotent re-call. Report the existing wiring; do NOT
		// re-scaffold (would clobber user edits to sign-in.tsx etc).
		JSON(w, http.StatusOK, authSetupResponse{
			Phase:             "scaffold",
			Project:           req.Project,
			ProjectAt:         projectAt,
			Framework:         framework,
			Mode:              currentMode,
			AlreadyConfigured: true,
			CurrentMode:       currentMode,
			NextSteps: []string{
				fmt.Sprintf("Auth is already wired in %s mode. To rotate the secret or change storage, edit src/lib/auth.ts and .env.local manually, or delete those files and re-run agentry_auth_setup.", currentMode),
			},
		})
		return
	}

	// Phase 2 — scaffold.
	result, err := scaffoldNextAuth(projectAt, req.Mode)
	if err != nil {
		Error(w, http.StatusInternalServerError, "scaffold: "+err.Error())
		return
	}
	result.Phase = "scaffold"
	result.Project = req.Project
	result.ProjectAt = projectAt
	result.Framework = framework
	result.Mode = req.Mode
	JSON(w, http.StatusOK, result)
}

// detectFramework inspects package.json for "next" in deps. Returns
// "next" / "unknown". Stays narrow on purpose — V1 only ships the
// Next.js scaffold; other frameworks come in a follow-up.
func detectFramework(projectAt string) (string, error) {
	pkg, err := readPackageJSON(projectAt)
	if err != nil {
		return "unknown", err
	}
	deps := mergeDeps(pkg)
	if _, ok := deps["next"]; ok {
		return "next", nil
	}
	return "unknown", nil
}

// detectExistingAuth checks for a previous agentry_auth_setup run.
// Signals: better-auth in package.json deps, AND src/lib/auth.ts on
// disk. Returns the mode encoded in the auth.ts header comment if
// present.
func detectExistingAuth(projectAt string) (bool, string) {
	pkg, err := readPackageJSON(projectAt)
	if err != nil {
		return false, ""
	}
	deps := mergeDeps(pkg)
	if _, ok := deps["better-auth"]; !ok {
		return false, ""
	}
	// Read the whole auth.ts and look for one of the storage-specific
	// import markers. Reading-all is cheap (the file is < 4 KB), and
	// the previous "first 600 bytes" cap blew past the file's longer
	// doc-header on more recent templates, sending every detection
	// to "unknown".
	authTS := filepath.Join(projectAt, "src", "lib", "auth.ts")
	body, err := os.ReadFile(authTS)
	if err != nil {
		// better-auth in deps but no auth.ts — half-installed; treat as
		// not configured so the scaffold can complete.
		return false, ""
	}
	s := string(body)
	switch {
	case strings.Contains(s, "better-sqlite3"):
		return true, "sqlite"
	case strings.Contains(s, `from "pg"`):
		return true, "binding:postgres"
	case strings.Contains(s, "mongodbAdapter") ||
		strings.Contains(s, "@better-auth/mongodb-adapter"):
		return true, "binding:mongodb"
	}
	return true, "unknown"
}

// listAvailableModes returns the menu the probe response surfaces.
// Modes whose underlying binding is NOT mounted at runtime are still
// listed but with Available=false so the LLM can explain to the user
// why they're greyed out.
func listAvailableModes() []authModeOption {
	out := []authModeOption{
		{
			ID:          "none",
			Label:       "No auth",
			Description: "Don't wire auth for this app. Re-run agentry_auth_setup later to enable.",
			Available:   true,
		},
		{
			ID:          "sqlite",
			Label:       "SQLite (zero setup)",
			Description: "Users + sessions live in a local SQLite file at /workspace/.agentry/auth.db. Survives sandbox restart. Easy to swap to Postgres/Mongo later.",
			Available:   true,
		},
		{
			ID:          "binding:postgres",
			Label:       "Bound Postgres",
			Description: "Use DATABASE_URL from your bound Postgres service. The bridge already wires the env var; auth tables auto-migrate on first request.",
			Available:   bindingMounted("postgres"),
		},
		{
			ID:          "binding:mongodb",
			Label:       "Bound MongoDB",
			Description: "Use MONGODB_URL from your bound MongoDB service. Schemaless; no migration required.",
			Available:   bindingMounted("mongodb"),
		},
	}
	return out
}

// bindingMounted reports whether service_bind has ever wired the
// given service into this sandbox. The convention from the
// bind-credential machinery: each service drops a directory under
// bindingsRoot/<service>/ (default /var/run/agentry).
func bindingMounted(service string) bool {
	_, err := os.Stat(filepath.Join(bindingsRoot, service))
	return err == nil
}

// buildQuestionPrompt is the verbatim text the LLM should relay to
// the user. Keep it tight; the LLM may rephrase but having a default
// makes phase 1 → phase 2 hand-off deterministic.
func buildQuestionPrompt(configured bool, currentMode string) string {
	if configured {
		return fmt.Sprintf(
			"Auth is already wired in %q mode. Re-running won't overwrite. Want me to skip, or are you trying to switch storage backends?",
			currentMode,
		)
	}
	return "How should I wire user auth?\n" +
		"  1. No auth — keep the app open.\n" +
		"  2. SQLite — zero setup, users live in /workspace/.agentry/auth.db.\n" +
		"  3. Bound Postgres / MongoDB — only if you've already used service_bind.\n" +
		"Pick one."
}

// scaffoldNextAuth materializes every Better-Auth file under
// projectAt. Caller has already validated mode and verified the
// project isn't already configured.
func scaffoldNextAuth(projectAt, mode string) (authSetupResponse, error) {
	secret, err := generateAuthSecret()
	if err != nil {
		return authSetupResponse{}, fmt.Errorf("generate secret: %w", err)
	}
	data := map[string]string{
		"Storage":    storageKindFor(mode),
		"EnvVarName": envVarFor(mode),
		"SQLitePath": filepath.Join(sqliteDataDir, "auth.db"),
		"Secret":     secret,
	}

	files := []struct {
		Tmpl, Dest string
	}{
		{"authsetup_templates/nextapp/auth.ts.tmpl", "src/lib/auth.ts"},
		{"authsetup_templates/nextapp/auth-client.ts.tmpl", "src/lib/auth-client.ts"},
		{"authsetup_templates/nextapp/api-route.ts.tmpl", "src/app/api/auth/[...all]/route.ts"},
		{"authsetup_templates/nextapp/sign-in.tsx.tmpl", "src/app/sign-in/page.tsx"},
		{"authsetup_templates/nextapp/sign-up.tsx.tmpl", "src/app/sign-up/page.tsx"},
	}
	// Mongo is schemaless; no auth-schema.ts needed (auth.ts doesn't
	// import it under mongo mode either).
	if mode == "sqlite" || mode == "binding:postgres" {
		files = append(files, struct{ Tmpl, Dest string }{
			"authsetup_templates/nextapp/auth-schema.ts.tmpl", "src/lib/auth-schema.ts",
		})
	}
	var written []string
	for _, f := range files {
		dest := filepath.Join(projectAt, f.Dest)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return authSetupResponse{}, fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), err)
		}
		if err := renderTemplate(f.Tmpl, dest, data); err != nil {
			return authSetupResponse{}, fmt.Errorf("render %s: %w", f.Dest, err)
		}
		written = append(written, f.Dest)
	}

	// .env.local — append, don't overwrite (the user may have other
	// env vars there already).
	envPath := filepath.Join(projectAt, ".env.local")
	envSnippet, err := renderToString("authsetup_templates/nextapp/env.local.tmpl", data)
	if err != nil {
		return authSetupResponse{}, fmt.Errorf("render env.local: %w", err)
	}
	if err := appendIfMissing(envPath, "BETTER_AUTH_SECRET", envSnippet); err != nil {
		return authSetupResponse{}, fmt.Errorf("write .env.local: %w", err)
	}
	written = append(written, ".env.local")

	// .npmrc — flips npm's strict peer-dep check off so React 19
	// prerelease tags don't blow up `npm install better-auth`. Append
	// the marker (legacy-peer-deps=) only if missing so a user-set
	// .npmrc with the same line in a different format isn't doubled.
	npmrcPath := filepath.Join(projectAt, ".npmrc")
	npmrcSnippet, err := renderToString("authsetup_templates/nextapp/npmrc.tmpl", data)
	if err != nil {
		return authSetupResponse{}, fmt.Errorf("render .npmrc: %w", err)
	}
	if err := appendIfMissing(npmrcPath, "legacy-peer-deps", npmrcSnippet); err != nil {
		return authSetupResponse{}, fmt.Errorf("write .npmrc: %w", err)
	}
	written = append(written, ".npmrc")

	// SQLite dir — only for sqlite mode. The adapter will create the
	// db file on first use, but the parent directory must exist or
	// better-sqlite3 throws ENOENT at boot.
	if mode == "sqlite" {
		if err := os.MkdirAll(sqliteDataDir, 0o755); err != nil {
			return authSetupResponse{}, fmt.Errorf("mkdir %s: %w", sqliteDataDir, err)
		}
	}

	// package.json — add deps + a postinstall hint. Sort the result
	// so re-runs produce identical bytes.
	deps := depsFor(mode)
	added, err := addDepsToPackageJSON(projectAt, deps)
	if err != nil {
		return authSetupResponse{}, fmt.Errorf("update package.json: %w", err)
	}
	written = append(written, "package.json")
	sort.Strings(written)

	pm := detectPackageManager(projectAt)
	commands := []string{
		fmt.Sprintf("%s install", pm),
	}
	nextSteps := []string{
		"Restart the dev server (project_start restart=true) so Next.js picks up the new routes.",
		"Add a 'Sign in' link to your nav: <a href=\"/sign-in\">Sign in</a> (use auth-client's useSession() to conditionally show 'Sign out' once logged in).",
		"Gate any protected pages by calling auth.api.getSession({ headers: await headers() }) from a Server Component and redirecting unauthenticated users to /sign-in.",
	}
	if mode == "binding:postgres" {
		nextSteps = append(nextSteps,
			"On first request, Better-Auth runs CREATE TABLE statements against your bound Postgres. Make sure the service-bound role has CREATE privileges.")
	}

	return authSetupResponse{
		FilesWritten:  written,
		PackageAdded:  added,
		CommandsToRun: commands,
		NextSteps:     nextSteps,
	}, nil
}

// generateAuthSecret returns a 32-byte URL-safe random string. Used
// once at scaffold time; the LLM never sees it.
func generateAuthSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func renderTemplate(name, dest string, data any) error {
	out, err := renderToString(name, data)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, []byte(out), 0o644)
}

func renderToString(name string, data any) (string, error) {
	raw, err := authSetupTemplates.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("embed read %s: %w", name, err)
	}
	tmpl, err := template.New(name).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("execute %s: %w", name, err)
	}
	return b.String(), nil
}

// appendIfMissing keeps .env.local additions idempotent — if marker
// already shows up in the file, we don't append again.
func appendIfMissing(path, marker, snippet string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(existing), marker) {
		return nil
	}
	combined := string(existing)
	if combined != "" && !strings.HasSuffix(combined, "\n") {
		combined += "\n"
	}
	combined += snippet
	return os.WriteFile(path, []byte(combined), 0o600)
}

// storageKindFor maps the wire-level mode to the template's Storage
// variable (one of "sqlite" / "postgres" / "mongodb").
func storageKindFor(mode string) string {
	switch mode {
	case "sqlite":
		return "sqlite"
	case "binding:postgres":
		return "postgres"
	case "binding:mongodb":
		return "mongodb"
	}
	return ""
}

// envVarFor returns the env var name the auth.ts template reads at
// runtime. Empty for sqlite (the SQLite file path is hardcoded).
func envVarFor(mode string) string {
	switch mode {
	case "binding:postgres":
		return "DATABASE_URL"
	case "binding:mongodb":
		return "MONGODB_URL"
	}
	return ""
}

// depsFor returns the npm package list each mode requires.
//
// EVERY version below is PINNED EXACTLY (no ^ or ~). The scaffold has
// to be deterministic in time — calling agentry_auth_setup today and
// next week MUST produce identical, working installs. The user hit
// four separate dependency-drift bugs in V1 (Kysely 0.29 broke the
// adapter, better-sqlite3 11 < peer dep 12, the adapter package
// moving to @better-auth/kysely-adapter, React 19 RC peer-rejection
// — that last one is handled by the generated .npmrc). Caret ranges
// would re-introduce all of those one by one as transitive bumps
// land in npm.
//
// To upgrade Better-Auth, bump these here, regenerate src/lib/auth-
// schema.ts with `npx @better-auth/cli generate --print`, and ship
// the scaffold version. Do not let users drift via `npm update`.
func depsFor(mode string) map[string]string {
	deps := map[string]string{
		"better-auth":                  "1.6.14",
		"@better-auth/kysely-adapter":  "1.6.14",
		"kysely":                       "0.28.5",
	}
	switch mode {
	case "sqlite":
		deps["better-sqlite3"] = "12.0.0"
	case "binding:postgres":
		deps["pg"] = "8.13.1"
		deps["@types/pg"] = "8.11.10"
	case "binding:mongodb":
		// Mongo uses Better-Auth's own mongo adapter — no Kysely
		// needed. We leave the Kysely entries above to avoid a
		// version-skew matrix; they're tiny and removing them
		// per-mode complicates the upgrade story.
		deps["@better-auth/mongodb-adapter"] = "1.6.14"
		deps["mongodb"] = "6.10.0"
	}
	return deps
}

// readPackageJSON unmarshals projectAt/package.json into a generic
// map so we can poke at it without dragging in the full npm schema.
func readPackageJSON(projectAt string) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join(projectAt, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("read package.json: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}
	return out, nil
}

// mergeDeps flattens dependencies + devDependencies into a single map
// so callers can ask "is X anywhere in this package.json?" cheaply.
func mergeDeps(pkg map[string]any) map[string]string {
	out := map[string]string{}
	for _, key := range []string{"dependencies", "devDependencies"} {
		raw, ok := pkg[key].(map[string]any)
		if !ok {
			continue
		}
		for name, ver := range raw {
			if s, ok := ver.(string); ok {
				out[name] = s
			}
		}
	}
	return out
}

// detectPackageManager reads which lockfile is present. Falls back to
// npm. Decides which install command to recommend in commandsToRun.
func detectPackageManager(projectAt string) string {
	for _, p := range []struct {
		File, PM string
	}{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"bun.lockb", "bun"},
		{"package-lock.json", "npm"},
	} {
		if _, err := os.Stat(filepath.Join(projectAt, p.File)); err == nil {
			return p.PM
		}
	}
	return "npm"
}

// addDepsToPackageJSON merges deps into package.json's dependencies
// map and rewrites the file. Returns the list of package names that
// were newly added (existing deps with the same name are skipped to
// avoid downgrading user-pinned versions).
func addDepsToPackageJSON(projectAt string, deps map[string]string) ([]string, error) {
	pkg, err := readPackageJSON(projectAt)
	if err != nil {
		return nil, err
	}
	cur, _ := pkg["dependencies"].(map[string]any)
	if cur == nil {
		cur = map[string]any{}
	}
	var added []string
	for name, ver := range deps {
		if _, exists := cur[name]; exists {
			continue
		}
		cur[name] = ver
		added = append(added, name)
	}
	pkg["dependencies"] = cur
	out, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')
	if err := os.WriteFile(filepath.Join(projectAt, "package.json"), out, 0o644); err != nil {
		return nil, err
	}
	sort.Strings(added)
	return added, nil
}
