package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

// `agentry auth` — operator-side, cluster + profile-scoped auth
// posture. Validates a DB binding with a real fitness check, mints
// AUTH_SECRET, and tracks the OAuth providers the sidecar should
// surface on the login page.
//
// Sandbox-side, the PostCreateHook reads the (cluster, profile) auth
// state and stamps the right env vars on every new sandbox. App code
// the LLM writes does nothing about auth beyond reading
// x-forwarded-user headers (see docs_read("skills/auth")).
//
// Auth state lives at:
//
//	~/.agentry/auth/<cluster>/<profile>.json
//
// One file per (cluster, profile) pair, file mode 0600.

const authUsage = `Usage:
  agentry auth                          show the active profile's auth posture
  agentry auth enable [--db NAME]       run a DB fitness check + mint AUTH_SECRET
  agentry auth disable                  remove auth state for this profile
  agentry auth status                   detailed view (alias: just ` + "`agentry auth`" + `)
  agentry auth providers add NAME       register an OAuth provider (google, github, …)
  agentry auth providers remove NAME    remove a provider
  agentry auth providers list           table of every provider on this profile
  agentry auth sync                     re-stamp auth env on every running sandbox

Auth state is per (cluster, profile). Switching profile via
` + "`agentry profile use`" + ` switches which auth posture sandboxes inherit.

Prereq: ` + "`agentry auth enable`" + ` requires a postgres, mysql, or mongo
binding already in the profile. Run ` + "`agentry service bind postgres`" + `
(or mysql / mongodb) first if you haven't.

Examples:
  agentry service bind postgres                  # supply DATABASE_URL
  agentry auth enable                            # fitness-checks the DB, mints AUTH_SECRET
  agentry auth providers add google \
    --client-id 12345.apps.googleusercontent.com \
    --client-secret GOCSPX-…                     # validates against Google's OIDC discovery
`

func cmdAuth(args []string) int {
	if len(args) == 0 {
		return authStatus(nil)
	}
	if isHelpFlag(args[0]) {
		fmt.Fprint(os.Stdout, authUsage)
		return 0
	}
	switch args[0] {
	case "enable", "on":
		return authEnable(args[1:])
	case "disable", "off":
		return authDisable(args[1:])
	case "status", "show":
		return authStatus(args[1:])
	case "providers", "provider":
		return cmdAuthProviders(args[1:])
	case "sync":
		return authSync(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "agentry auth: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(os.Stderr, authUsage)
		return 2
	}
}

// ── enable ──────────────────────────────────────────────────────────────

func authEnable(args []string) int {
	fs := flag.NewFlagSet("agentry auth enable", flag.ContinueOnError)
	dbHint := fs.String("db", "", "name of the service bind to use (postgres|mysql); auto-detect when only one is bound")
	secretOverride := fs.String("secret", "", "use this hex secret instead of minting a fresh one (handy for prod-stable secrets)")
	skipCheck := fs.Bool("skip-fitness", false, "skip the DB fitness check (dangerous; use only when iterating on the flow itself)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no cluster pinned — run `agentry cluster use NAME` first")
	}
	profile := resolveProfile(cfg, "")

	// Find a usable DB binding on the active profile.
	binds, err := listBinds(cfg.Cluster, profile)
	if err != nil {
		return die("list binds: %v", err)
	}
	bind, kind, err := pickDBBinding(binds, *dbHint)
	if err != nil {
		return die("%v", err)
	}
	url := dbBindingURL(bind)
	if url == "" {
		return die("the %q bind doesn't expose a known DB URL env var (looked for DATABASE_URL, POSTGRES_URL, MYSQL_URL, MONGODB_URI, MONGO_URL)", kind)
	}

	// Run the fitness check unless the operator opted out.
	if !*skipCheck {
		fmt.Printf("Fitness check: %s → ", kind)
		report := runFitness(kind, url)
		fmt.Println(report.describe())
		if !report.Ok() {
			fmt.Fprintf(os.Stderr, "\nDetails: %v\n", report.Err)
			return die("auth enable refused: DB binding %q is not usable for the sidecar's auth tables", kind)
		}
	}

	// Mint or reuse the AUTH_SECRET.
	secret := *secretOverride
	if secret == "" {
		s, err := mintAuthSecret()
		if err != nil {
			return die("mint secret: %v", err)
		}
		secret = s
	} else if !looksLikeHex(secret, 16) {
		return die("--secret should be hex-encoded and at least 16 bytes (32 hex chars)")
	}

	// Preserve existing providers if the operator is re-running
	// `enable` to swap DBs or rotate the secret.
	existing, _ := loadAuthState(cfg.Cluster, profile)
	state := &AuthState{
		Enabled:   true,
		DBBinding: kind,
		Secret:    secret,
		Providers: nil,
	}
	if existing != nil {
		state.Providers = existing.Providers
	}
	if err := saveAuthState(cfg.Cluster, profile, state); err != nil {
		return die("save auth state: %v", err)
	}

	fmt.Printf("\nauth enabled on cluster %q (profile %q):\n", cfg.Cluster, profile)
	fmt.Printf("  db binding: %s\n", kind)
	fmt.Printf("  AUTH_SECRET: <set, %d hex chars>\n", len(secret))
	if state.Providers == nil {
		fmt.Println("  providers: (none — `agentry auth providers add NAME` to enable social login)")
	} else {
		fmt.Printf("  providers: %s (carried over)\n", strings.Join(sortedProviderKeys(state.Providers), ", "))
	}
	fmt.Fprintln(os.Stderr, "\nEvery new sandbox created with profile", profile, "active will inherit these vars:")
	fmt.Fprintln(os.Stderr, "  AGENTRY_AUTH_ENABLED=true")
	fmt.Fprintln(os.Stderr, "  AGENTRY_AUTH_DB=<bind URL>")
	fmt.Fprintln(os.Stderr, "  AGENTRY_AUTH_SECRET=<the secret minted above>")

	// Also push to ALREADY-running sandboxes so the operator doesn't
	// have to recreate them. Best-effort: logs but doesn't fail.
	runAuthSyncForActiveProfile("enable")
	return 0
}

// pickDBBinding finds the postgres/mysql/mongo bind to use. Hint
// wins; otherwise auto-pick if exactly one DB-family bind exists.
// Refuses ambiguous setups so the operator's intent is explicit.
func pickDBBinding(binds []*StoredBind, hint string) (*StoredBind, string, error) {
	family := map[string]string{
		"postgres":   "postgres",
		"postgresql": "postgres",
		"pg":         "postgres",
		"mysql":      "mysql",
		"mariadb":    "mysql",
		"mongodb":    "mongo",
		"mongo":      "mongo",
	}
	type candidate struct {
		bind *StoredBind
		kind string
	}
	var matches []candidate
	for _, b := range binds {
		if k, ok := family[strings.ToLower(b.Service)]; ok {
			matches = append(matches, candidate{b, k})
		}
	}
	if hint != "" {
		want, ok := family[strings.ToLower(hint)]
		if !ok {
			return nil, "", fmt.Errorf("--db %q isn't a known DB family (postgres, mysql, mongo)", hint)
		}
		for _, m := range matches {
			if m.kind == want {
				return m.bind, m.kind, nil
			}
		}
		return nil, "", fmt.Errorf("--db %q requested but no %s bind on this profile — run `agentry service bind %s` first", hint, want, want)
	}
	switch len(matches) {
	case 0:
		return nil, "", fmt.Errorf("no postgres/mysql/mongo bind on this profile — run `agentry service bind postgres` (or mysql) first")
	case 1:
		return matches[0].bind, matches[0].kind, nil
	default:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.kind
		}
		sort.Strings(names)
		return nil, "", fmt.Errorf("multiple DB binds found (%s); pass --db NAME to pick", strings.Join(names, ", "))
	}
}

// runFitness dispatches to the right family check. Lives here (not
// in auth_fitness.go) because kind names ("postgres" / "mysql" /
// "mongo") are the CLI's identifiers — the fitness functions don't
// need to know about them.
func runFitness(kind, url string) fitnessReport {
	switch kind {
	case "postgres":
		return fitnessPostgres(url)
	case "mysql":
		return fitnessMySQL(url)
	case "mongo":
		return fitnessMongo(url)
	}
	return fitnessReport{Err: fmt.Errorf("unknown db kind %q", kind)}
}

// ── disable ─────────────────────────────────────────────────────────────

func authDisable(args []string) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		fmt.Fprintln(os.Stdout, "Usage: agentry auth disable")
		fmt.Fprintln(os.Stdout, "  Removes auth state for the active (cluster, profile) pair.")
		fmt.Fprintln(os.Stdout, "  New sandboxes won't have AGENTRY_AUTH_* stamped; the sidecar")
		fmt.Fprintln(os.Stdout, "  falls back to passthrough mode. Existing sandboxes keep their")
		fmt.Fprintln(os.Stdout, "  current state until recreated.")
		return 0
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no cluster pinned")
	}
	profile := resolveProfile(cfg, "")
	state, _ := loadAuthState(cfg.Cluster, profile)
	if state == nil || !state.Enabled {
		fmt.Printf("auth already disabled on cluster %q (profile %q).\n", cfg.Cluster, profile)
		return 0
	}
	if err := deleteAuthState(cfg.Cluster, profile); err != nil {
		return die("delete auth state: %v", err)
	}
	fmt.Printf("auth disabled on cluster %q (profile %q).\n", cfg.Cluster, profile)
	// Wipe the core auth vars from already-running sandboxes so the
	// sidecar drops back to passthrough mode without a sandbox
	// recreate. Provider creds stay (see authEnvForState's note).
	runAuthSyncForActiveProfile("disable")
	return 0
}

// ── status ──────────────────────────────────────────────────────────────

func authStatus(args []string) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		fmt.Fprintln(os.Stdout, "Usage: agentry auth [status]")
		fmt.Fprintln(os.Stdout, "  Shows the auth posture of the active (cluster, profile) pair.")
		fmt.Fprintln(os.Stdout, "  Bare `agentry auth` is an alias for `agentry auth status`.")
		return 0
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no cluster pinned")
	}
	profile := resolveProfile(cfg, "")
	state, err := loadAuthState(cfg.Cluster, profile)
	if err != nil {
		return die("load auth state: %v", err)
	}
	return authStatusTo(os.Stdout, cfg.Cluster, profile, state)
}

// authStatusTo renders the status block to w so tests can capture
// the output without juggling globals.
func authStatusTo(w io.Writer, cluster, profile string, state *AuthState) int {
	if state == nil || !state.Enabled {
		fmt.Fprintf(w, "auth: DISABLED on cluster %q (profile %q)\n", cluster, profile)
		fmt.Fprintf(w, "\nEnable with:\n")
		fmt.Fprintf(w, "  agentry service bind postgres   # (or mysql)\n")
		fmt.Fprintf(w, "  agentry auth enable\n")
		return 0
	}
	fmt.Fprintf(w, "auth: ENABLED on cluster %q (profile %q)\n", cluster, profile)
	fmt.Fprintf(w, "  db binding: %s\n", state.DBBinding)
	fmt.Fprintf(w, "  AUTH_SECRET: <set, %d hex chars>\n", len(state.Secret))
	if len(state.Providers) == 0 {
		fmt.Fprintln(w, "  providers: (none — email+password only)")
	} else {
		names := sortedProviderKeys(state.Providers)
		fmt.Fprintf(w, "  providers: %s\n", strings.Join(names, ", "))
	}
	return 0
}

// ── providers ───────────────────────────────────────────────────────────

const authProvidersUsage = `Usage:
  agentry auth providers add NAME --client-id ID --client-secret SECRET [--issuer URL] [--scopes "openid email"]
  agentry auth providers remove NAME
  agentry auth providers list

Supported NAME:
  google         OIDC discovery via accounts.google.com
  github         OAuth 2.0 via api.github.com
  microsoft      OIDC discovery via login.microsoftonline.com (multi-tenant)
  apple          OIDC discovery via appleid.apple.com
  generic-oidc   pass --issuer with your provider's base URL

Add and remove validate the upstream before writing — a typo'd issuer
or unreachable provider fails at enable time, not when an end user
tries to log in.
`

func cmdAuthProviders(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, authProvidersUsage)
		return 2
	}
	if isHelpFlag(args[0]) {
		fmt.Fprint(os.Stdout, authProvidersUsage)
		return 0
	}
	switch args[0] {
	case "add", "set":
		return authProviderAdd(args[1:])
	case "remove", "rm", "delete":
		return authProviderRemove(args[1:])
	case "ls", "list":
		return authProviderList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "agentry auth providers: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(os.Stderr, authProvidersUsage)
		return 2
	}
}

func authProviderAdd(args []string) int {
	fs := flag.NewFlagSet("agentry auth providers add", flag.ContinueOnError)
	clientID := fs.String("client-id", "", "OAuth client ID issued by the provider")
	clientSecret := fs.String("client-secret", "", "OAuth client secret issued by the provider")
	issuer := fs.String("issuer", "", "issuer URL (required for generic-oidc; overrides default for google/microsoft/apple)")
	scopes := fs.String("scopes", "", "space-separated scopes (default: provider's standard)")
	flagArgs, posArgs := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(posArgs) != 1 {
		fmt.Fprint(os.Stderr, authProvidersUsage)
		return 2
	}
	name := strings.ToLower(posArgs[0])
	if !providerKnown(name) {
		return die("unknown provider %q (known: google, github, microsoft, apple, generic-oidc)", name)
	}
	if *clientID == "" || *clientSecret == "" {
		return die("--client-id and --client-secret are required")
	}
	if name == "generic-oidc" && *issuer == "" {
		return die("generic-oidc requires --issuer URL")
	}

	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no cluster pinned")
	}
	profile := resolveProfile(cfg, "")
	state, _ := loadAuthState(cfg.Cluster, profile)
	if state == nil || !state.Enabled {
		return die("auth not enabled on this profile — run `agentry auth enable` first")
	}

	fmt.Printf("Validating %s provider … ", name)
	if err := validateProvider(context.Background(), name, *issuer); err != nil {
		fmt.Println("failed")
		return die("provider validation: %v", err)
	}
	fmt.Println("ok")

	if state.Providers == nil {
		state.Providers = map[string]AuthProviderState{}
	}
	state.Providers[name] = AuthProviderState{
		ClientID:     *clientID,
		ClientSecret: *clientSecret,
		Scopes:       strings.Fields(*scopes),
	}
	if err := saveAuthState(cfg.Cluster, profile, state); err != nil {
		return die("save auth state: %v", err)
	}
	fmt.Printf("provider %s added on cluster %q (profile %q).\n", name, cfg.Cluster, profile)
	fmt.Fprintln(os.Stderr, "\nNext: register the callback URL with the provider's console:")
	fmt.Fprintf(os.Stderr, "  https://<deploy-url>/auth/%s/callback\n", name)
	runAuthSyncForActiveProfile("providers add " + name)
	return 0
}

func authProviderRemove(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: agentry auth providers remove NAME")
		return 2
	}
	name := strings.ToLower(args[0])
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no cluster pinned")
	}
	profile := resolveProfile(cfg, "")
	state, _ := loadAuthState(cfg.Cluster, profile)
	if state == nil || state.Providers == nil {
		fmt.Printf("provider %q not configured on this profile.\n", name)
		return 0
	}
	if _, ok := state.Providers[name]; !ok {
		fmt.Printf("provider %q not configured on this profile.\n", name)
		return 0
	}
	delete(state.Providers, name)
	if err := saveAuthState(cfg.Cluster, profile, state); err != nil {
		return die("save auth state: %v", err)
	}
	fmt.Printf("provider %s removed from cluster %q (profile %q).\n", name, cfg.Cluster, profile)
	runAuthSyncForActiveProfile("providers remove " + name)
	return 0
}

func authProviderList(args []string) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		fmt.Fprintln(os.Stdout, "Usage: agentry auth providers list")
		return 0
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no cluster pinned")
	}
	profile := resolveProfile(cfg, "")
	state, _ := loadAuthState(cfg.Cluster, profile)
	return authProviderListTo(os.Stdout, cfg.Cluster, profile, state)
}

func authProviderListTo(w io.Writer, cluster, profile string, state *AuthState) int {
	if state == nil || !state.Enabled {
		fmt.Fprintf(w, "auth disabled on cluster %q (profile %q). Run `agentry auth enable` first.\n", cluster, profile)
		return 0
	}
	if len(state.Providers) == 0 {
		fmt.Fprintf(w, "No providers configured on cluster %q (profile %q).\n", cluster, profile)
		fmt.Fprintln(w, "\nAdd one with:")
		fmt.Fprintln(w, "  agentry auth providers add google --client-id … --client-secret …")
		return 0
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tCLIENT ID\tSCOPES")
	for _, name := range sortedProviderKeys(state.Providers) {
		p := state.Providers[name]
		scopes := "(default)"
		if len(p.Scopes) > 0 {
			scopes = strings.Join(p.Scopes, " ")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, p.ClientID, scopes)
	}
	tw.Flush()
	return 0
}

// ── helpers ─────────────────────────────────────────────────────────────

// looksLikeHex returns true when s is hex-encoded AND at least
// minBytes bytes long once decoded. Refuses operator-supplied
// secrets that are obviously too short to HMAC anything meaningful.
func looksLikeHex(s string, minBytes int) bool {
	if s == "" || len(s)%2 != 0 || len(s) < minBytes*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func sortedProviderKeys(m map[string]AuthProviderState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
