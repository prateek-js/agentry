package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
)

// Profile is a named slice of cluster-scoped configuration: env vars,
// service binds, and (later) auth state. Same cluster, different envs
// — e.g. one cluster carrying both "dev" and "prod" profiles whose
// DATABASE_URL, GITHUB_TOKEN, and OAuth client_ids differ.
//
// Profiles live ENTIRELY on the operator's laptop. The provisioner
// knows nothing about them; the CLI's role at sandbox-create time is
// to stamp the active profile's envs/binds onto the new sandbox via
// the existing PostCreateHook. So switching profiles is just changing
// which JSON files the CLI reads at sandbox-create — no server-side
// state to worry about.
//
// Storage layout (one directory inserted at the profile level):
//
//	~/.agentry/envs/<cluster>/<profile>/<NAME>.json
//	~/.agentry/services/<cluster>/<profile>/<service>.json
//
// Migration: any legacy .json files directly under <cluster>/ (from
// before profiles existed) are moved into <cluster>/default/ on first
// access. Idempotent — runs once per process via sync.Once.

const defaultProfile = "default"

// resolveProfile picks the profile to operate on: an explicit override
// (typically from a --profile flag) wins; otherwise the active
// profile from config; otherwise the literal "default".
func resolveProfile(cfg *Config, override string) string {
	if override != "" {
		return override
	}
	if cfg != nil && cfg.Profile != "" {
		return cfg.Profile
	}
	return defaultProfile
}

// clusterAndProfile bridges the per-request cluster getter
// (TTL-cached, used by the round-tripper) with a fresh profile read
// from config. The PostCreateHook calls this on every sandbox_create,
// so a `agentry profile use prod` between two creates takes effect on
// the second one without restarting the stdio process.
//
// On config-read failure we still return the cluster but fall back to
// the default profile. Better to apply a partial set of binds than to
// silently skip the hook.
func clusterAndProfile(getCluster func() string) func() (string, string) {
	return func() (string, string) {
		cluster := ""
		if getCluster != nil {
			cluster = getCluster()
		}
		cfg, _, err := LoadConfig()
		if err != nil {
			return cluster, defaultProfile
		}
		return cluster, resolveProfile(cfg, "")
	}
}

// validateProfileName enforces single-segment names that work as
// directory components on every platform. Profiles are paths; allowing
// "/" or ".." would let a typo collide with the cluster layer.
func validateProfileName(name string) error {
	if name == "" {
		return fmt.Errorf("profile name is empty")
	}
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return fmt.Errorf("profile name must be a single path segment (no '/' or '\\')")
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("profile name must not start with '.'")
	}
	return nil
}

// migrationOnce gates a one-time scan that moves legacy .json files
// (pre-profile layout: <cluster>/<service>.json) into the default
// profile (<cluster>/default/<service>.json). Cheap when there's
// nothing to do — directory listing only.
var migrationOnce sync.Once

// resetMigrationOnce is only called from tests that need to re-run
// migration against a fresh temp dir.
func resetMigrationOnce() { migrationOnce = sync.Once{} }

func migrateLegacyProfileLayout() {
	migrationOnce.Do(func() {
		base := filepath.Dir(ConfigPath())
		for _, kind := range []string{"envs", "services"} {
			root := filepath.Join(base, kind)
			entries, err := os.ReadDir(root)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				cluster := filepath.Join(root, e.Name())
				moveLegacyJSONsToDefault(cluster)
			}
		}
	})
}

// moveLegacyJSONsToDefault walks one cluster dir; any .json files
// found directly inside (not within a subdir) move to a new "default"
// subdirectory. Already-profile-organised clusters have no .json
// children at this level and are left alone.
func moveLegacyJSONsToDefault(clusterDir string) {
	entries, err := os.ReadDir(clusterDir)
	if err != nil {
		return
	}
	var legacy []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") {
			legacy = append(legacy, e.Name())
		}
	}
	if len(legacy) == 0 {
		return
	}
	defDir := filepath.Join(clusterDir, defaultProfile)
	if err := os.MkdirAll(defDir, 0o700); err != nil {
		return
	}
	for _, name := range legacy {
		src := filepath.Join(clusterDir, name)
		dst := filepath.Join(defDir, name)
		// Skip if a dest with the same name already exists — never
		// clobber profile content with legacy content.
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		_ = os.Rename(src, dst)
	}
}

// ── Subcommand ───────────────────────────────────────────────────────────

const profileUsage = `Usage:
  agentry profile                       show the active profile
  agentry profile list                  list every profile on this laptop
  agentry profile use NAME              switch the active profile
  agentry profile create NAME           create an empty profile on the current server
  agentry profile delete NAME           remove a profile and its staged content
  agentry profile show [NAME]           describe a profile's envs and binds
  agentry profile copy SOURCE DEST      clone one profile into another

Profiles are operator-side, cluster-scoped slices of configuration
(envs + service binds). They let one cluster carry both 'dev' and
'prod' setups whose DATABASE_URL, GITHUB_TOKEN, and OAuth client_ids
differ. Switching profiles is local-only — no server-side change.

Examples:
  # Stand up a dev profile beside default:
  agentry profile create dev
  agentry profile use dev
  agentry env set DATABASE_URL postgres://localhost/dev
  agentry service bind postgres

  # Promote dev settings as the baseline for staging:
  agentry profile copy dev staging
`

func cmdProfile(args []string) int {
	if len(args) == 0 {
		// Bare "agentry profile" — print active context. Mirrors
		// kubectl's bare `kubectl config current-context` UX.
		return profileCurrent(nil)
	}
	if isHelpFlag(args[0]) {
		fmt.Fprint(os.Stdout, profileUsage)
		return 0
	}
	switch args[0] {
	case "use":
		return profileUse(args[1:])
	case "ls", "list":
		return profileList(args[1:])
	case "create", "new":
		return profileCreate(args[1:])
	case "delete", "rm":
		return profileDelete(args[1:])
	case "show", "describe":
		return profileShow(args[1:])
	case "copy", "cp":
		return profileCopy(args[1:])
	case "current":
		return profileCurrent(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "agentry profile: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(os.Stderr, profileUsage)
		return 2
	}
}

// profileCurrent prints the active (cluster, profile) pair. Quiet by
// default — `agentry profile` alone should be greppable on one line.
// kubectl precedent: `kubectl config current-context` prints just
// the name with no decoration.
func profileCurrent(_ []string) int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	profile := resolveProfile(cfg, "")
	if cfg.Cluster == "" {
		fmt.Printf("%s\n", profile)
		fmt.Fprintln(os.Stderr, "(no server pinned — run `agentry server use NAME`)")
		return 0
	}
	fmt.Printf("%s (cluster=%s)\n", profile, cfg.Cluster)
	return 0
}

// profileUse switches the active profile on the current server.
// Doesn't validate that the profile dir exists — switching to a fresh
// name just means "next env/bind set will land in this profile."
func profileUse(args []string) int {
	if len(args) < 1 || isHelpFlag(args[0]) {
		fmt.Fprintln(os.Stderr, "Usage: agentry profile use NAME")
		fmt.Fprintln(os.Stderr, "  Switches the active profile within the current server.")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	name := args[0]
	if err := validateProfileName(name); err != nil {
		return die("%v", err)
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	previous := resolveProfile(cfg, "")
	cfg.Profile = name
	if err := cfg.Save(); err != nil {
		return die("save config: %v", err)
	}
	cluster := cfg.Cluster
	if cluster == "" {
		cluster = "(no server pinned)"
	}
	if name == previous {
		fmt.Printf("Already on profile %q (cluster=%s).\n", name, cluster)
		return 0
	}
	fmt.Printf("Switched to profile %q on cluster %q.\n", name, cluster)
	return 0
}

// profileList shows every profile present on this laptop across every
// cluster. A profile "exists" iff it has at least one JSON child under
// envs/ or services/, OR it was created (and so has an empty dir).
// The active profile gets a "*" in the ACTIVE column. Counts mirror
// kubectl's get-tables: small numbers, easy to scan.
func profileList(args []string) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		fmt.Fprintln(os.Stdout, "Usage: agentry profile list")
		fmt.Fprintln(os.Stdout, "  Shows every profile on this laptop across every cluster.")
		fmt.Fprintln(os.Stdout, "  '*' in the ACTIVE column marks the current server + profile.")
		return 0
	}
	return listProfilesTo(os.Stdout)
}

// listProfilesTo separates the rendering from os.Stdout so tests can
// capture the output without juggling globals.
func listProfilesTo(w io.Writer) int {
	migrateLegacyProfileLayout()
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	rows := walkProfiles()

	if len(rows) == 0 {
		active := resolveProfile(cfg, "")
		fmt.Fprintf(w, "(no profiles staged yet)\n")
		fmt.Fprintf(w, "active context: profile=%s cluster=%s\n", active, dashWhenEmpty(cfg.Cluster))
		fmt.Fprintln(w, "\nCreate one with:")
		fmt.Fprintln(w, "  agentry profile create NAME")
		return 0
	}

	active := resolveProfile(cfg, "")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTIVE\tPROFILE\tCLUSTER\tENVS\tBINDS")
	for _, r := range rows {
		marker := " "
		if r.cluster == cfg.Cluster && r.profile == active {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\n", marker, r.profile, r.cluster, r.envs, r.binds)
	}
	tw.Flush()
	return 0
}

type profileRow struct {
	cluster, profile string
	envs, binds      int
}

// walkProfiles enumerates the on-disk state, returning one row per
// (cluster, profile) with counts. Profiles that exist as empty dirs
// (because of `profile create`) show counts of zero. Output is sorted
// by cluster, then profile.
func walkProfiles() []profileRow {
	base := filepath.Dir(ConfigPath())
	type key struct{ cluster, profile string }
	rows := map[key]*profileRow{}
	for _, kind := range []string{"envs", "services"} {
		root := filepath.Join(base, kind)
		clusters, _ := os.ReadDir(root)
		for _, c := range clusters {
			if !c.IsDir() {
				continue
			}
			profiles, _ := os.ReadDir(filepath.Join(root, c.Name()))
			for _, p := range profiles {
				if !p.IsDir() {
					continue
				}
				k := key{c.Name(), p.Name()}
				row, ok := rows[k]
				if !ok {
					row = &profileRow{cluster: c.Name(), profile: p.Name()}
					rows[k] = row
				}
				count := countJSONs(filepath.Join(root, c.Name(), p.Name()))
				if kind == "envs" {
					row.envs = count
				} else {
					row.binds = count
				}
			}
		}
	}
	out := make([]profileRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].cluster != out[j].cluster {
			return out[i].cluster < out[j].cluster
		}
		return out[i].profile < out[j].profile
	})
	return out
}

func countJSONs(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// profileCreate makes empty profile directories so `profile ls` can
// show them and `env set` / `service bind` have a home. Idempotent —
// re-running just exits 0.
func profileCreate(args []string) int {
	if len(args) < 1 || isHelpFlag(args[0]) {
		fmt.Fprintln(os.Stderr, "Usage: agentry profile create NAME")
		fmt.Fprintln(os.Stderr, "  Creates an empty profile on the current server.")
		fmt.Fprintln(os.Stderr, "  Run `agentry profile use NAME` afterwards to make it active.")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	name := args[0]
	if err := validateProfileName(name); err != nil {
		return die("%v", err)
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no server pinned — run `agentry server use NAME` first")
	}
	base := filepath.Dir(ConfigPath())
	for _, kind := range []string{"envs", "services"} {
		dir := filepath.Join(base, kind, cfg.Cluster, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return die("mkdir %s: %v", dir, err)
		}
	}
	fmt.Printf("Created profile %q on cluster %q.\n", name, cfg.Cluster)
	fmt.Fprintln(os.Stderr, "\nNext steps:")
	fmt.Fprintf(os.Stderr, "  agentry profile use %s\n", name)
	fmt.Fprintln(os.Stderr, "  agentry env set NAME VALUE")
	fmt.Fprintln(os.Stderr, "  agentry service bind <service>")
	return 0
}

// profileDelete purges the profile's content. Refuses to delete the
// active profile (the operator would strand themselves writing into a
// half-deleted profile). Refuses "default" outright — too many
// downstream paths assume it exists.
func profileDelete(args []string) int {
	if len(args) < 1 || isHelpFlag(args[0]) {
		fmt.Fprintln(os.Stderr, "Usage: agentry profile delete NAME")
		fmt.Fprintln(os.Stderr, "  Removes the profile dir and every env/bind staged under it.")
		fmt.Fprintln(os.Stderr, "  Existing sandboxes are NOT touched — only future creates.")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	name := args[0]
	if err := validateProfileName(name); err != nil {
		return die("%v", err)
	}
	if name == defaultProfile {
		return die("can't delete the default profile (it's the safety net for migrations and unconfigured commands)")
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no server pinned")
	}
	if resolveProfile(cfg, "") == name {
		return die("can't delete the active profile — run `agentry profile use OTHER` first")
	}
	base := filepath.Dir(ConfigPath())
	envsCount := countJSONs(filepath.Join(base, "envs", cfg.Cluster, name))
	bindsCount := countJSONs(filepath.Join(base, "services", cfg.Cluster, name))
	for _, kind := range []string{"envs", "services"} {
		dir := filepath.Join(base, kind, cfg.Cluster, name)
		_ = os.RemoveAll(dir)
	}
	fmt.Printf("Deleted profile %q on cluster %q (%d env(s), %d bind(s) purged).\n",
		name, cfg.Cluster, envsCount, bindsCount)
	return 0
}

// profileShow lists what's staged under a profile — names only,
// values never (env values are credentials). Useful for "wait, what
// does dev actually have set?" without leaking secrets to a
// screen-shared screen.
func profileShow(args []string) int {
	fs := flag.NewFlagSet("agentry profile show", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentry profile show [--profile NAME]")
		fmt.Fprintln(os.Stderr, "  Describes the active profile (or the one named with --profile)")
		fmt.Fprintln(os.Stderr, "  on the current server: env names + bind names. Values are")
		fmt.Fprintln(os.Stderr, "  never printed.")
	}
	override := fs.String("profile", "", "profile to show (default: active profile)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	migrateLegacyProfileLayout()
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no server pinned")
	}
	profile := resolveProfile(cfg, *override)
	return showProfileTo(os.Stdout, cfg.Cluster, profile)
}

// showProfileTo renders the show-output to w. Pure rendering so tests
// can assert against captured bytes.
func showProfileTo(w io.Writer, cluster, profile string) int {
	base := filepath.Dir(ConfigPath())
	envs := listProfileJSON(filepath.Join(base, "envs", cluster, profile))
	binds := listProfileJSON(filepath.Join(base, "services", cluster, profile))

	fmt.Fprintf(w, "Profile:  %s\n", profile)
	fmt.Fprintf(w, "Cluster:  %s\n", cluster)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Envs   (%d):\n", len(envs))
	if len(envs) == 0 {
		fmt.Fprintln(w, "  (none — `agentry env set NAME VALUE` to add one)")
	} else {
		for _, e := range envs {
			fmt.Fprintf(w, "  - %s\n", e)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Binds  (%d):\n", len(binds))
	if len(binds) == 0 {
		fmt.Fprintln(w, "  (none — `agentry service bind SERVICE` to add one)")
	} else {
		for _, b := range binds {
			fmt.Fprintf(w, "  - %s\n", b)
		}
	}
	return 0
}

func listProfileJSON(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out
}

// profileCopy clones one profile's contents into another. Source and
// dest are on the current server; cross-cluster copy isn't supported
// (different cluster usually means different DB URLs anyway, so a
// blind copy would mostly be wrong). Refuses to overwrite existing
// dest files — if dst already has the file, the operator probably set
// something there on purpose.
func profileCopy(args []string) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		fmt.Fprintln(os.Stdout, "Usage: agentry profile copy SOURCE DEST")
		fmt.Fprintln(os.Stdout, "  Clones every env + bind from SOURCE into DEST on the current")
		fmt.Fprintln(os.Stdout, "  cluster. Files that already exist in DEST are skipped.")
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: agentry profile copy SOURCE DEST")
		return 2
	}
	src, dst := args[0], args[1]
	if err := validateProfileName(src); err != nil {
		return die("source: %v", err)
	}
	if err := validateProfileName(dst); err != nil {
		return die("dest: %v", err)
	}
	if src == dst {
		return die("source and dest are the same")
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no server pinned")
	}
	base := filepath.Dir(ConfigPath())
	copied, skipped := 0, 0
	for _, kind := range []string{"envs", "services"} {
		srcDir := filepath.Join(base, kind, cfg.Cluster, src)
		dstDir := filepath.Join(base, kind, cfg.Cluster, dst)
		if err := os.MkdirAll(dstDir, 0o700); err != nil {
			return die("mkdir %s: %v", dstDir, err)
		}
		entries, _ := os.ReadDir(srcDir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
			if err != nil {
				continue
			}
			out := filepath.Join(dstDir, e.Name())
			if _, err := os.Stat(out); err == nil {
				skipped++
				continue
			}
			if err := os.WriteFile(out, raw, 0o600); err == nil {
				copied++
			}
		}
	}
	if copied == 0 && skipped == 0 {
		return die("source profile %q has no envs/binds to copy", src)
	}
	if skipped > 0 {
		fmt.Printf("Copied %d, skipped %d (existing) from %q → %q on cluster %q.\n",
			copied, skipped, src, dst, cfg.Cluster)
	} else {
		fmt.Printf("Copied %d file(s) from %q → %q on cluster %q.\n",
			copied, src, dst, cfg.Cluster)
	}
	return 0
}

func dashWhenEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
