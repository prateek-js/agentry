package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/agentry/agentry/pkg/tunnel"
	"golang.org/x/term"
)

// serviceHelp is the `agentry service --help` block.
const serviceHelp = `agentry service - manage server service bindings (databases, AI APIs, …)

Services live in the server catalog: postgres, mysql, mongodb, redis,
clickhouse, aws-s3, smtp, stripe, openai, anthropic, http-api. Binding
a service stages its credentials so every sandbox on this server gets
the matching env vars on shell start.

Usage:
  agentry service ls                        list available services in the catalog
  agentry service bind <service>            bind for THIS server (default)
  agentry service bind --sandbox <id> <s>   bind only to one sandbox (one-shot)
  agentry service bind <s> --from-env       read values from current shell env
  agentry service binds                     list server-default bindings stored locally
  agentry service unbind <service>          drop a server default

Examples:
  # First time: pick a server, then bind postgres
  agentry server use hetzner-test
  agentry service bind postgres

  # One-shot bind on a specific sandbox (won't apply to future creates)
  agentry service bind --sandbox sales-demo redis

  # CI: read STRIPE_SECRET_KEY etc from the shell instead of prompting
  STRIPE_SECRET_KEY=sk_test_... agentry service bind stripe --from-env

Service-catalog vars and prompts:
  Each service declares 1+ "fields" (the inputs you provide). One field
  often fans out to multiple env vars so different SDKs can read their
  conventional name — e.g. postgres takes one "url" and exports both
  DATABASE_URL and POSTGRES_URL. The CLI prompts once per FIELD, never
  once per env var.
`

// cmdService dispatches `agentry service *` subcommands.
func cmdService(args []string) int {
	if len(args) == 0 || isHelpFlag(args[0]) {
		fmt.Fprint(os.Stdout, serviceHelp)
		return 0
	}
	switch args[0] {
	case "bind":
		return serviceBindCLI(args[1:])
	case "ls", "list":
		return serviceListCLI(args[1:])
	case "binds":
		return serviceBindsListCLI(args[1:])
	case "unbind":
		return serviceUnbindCLI(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "agentry service: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(os.Stderr, serviceHelp)
		return 2
	}
}

// isHelpFlag returns true if s is one of the canonical help flags.
// Used so every subcommand can support a uniform "--help".
func isHelpFlag(s string) bool {
	return s == "--help" || s == "-h" || s == "help"
}

// serviceBindCLI is the interactive binding path. Two modes:
//
//	agentry service bind <service>                  (cluster default — stored on disk)
//	agentry service bind --sandbox <id> <service>   (one-shot — POSTed now, not stored)
//
// In both modes the catalog teaches us which env vars to prompt for.
// --from-env reads them from the current shell instead of prompting,
// useful for CI / scripting.
func serviceBindCLI(args []string) int {
	fs := flag.NewFlagSet("agentry service bind", flag.ContinueOnError)
	sandbox := fs.String("sandbox", "", "one-shot bind: target this sandbox only (omit to store as cluster default)")
	fromEnv := fs.Bool("from-env", false, "read values from the current shell env instead of prompting")
	profile := fs.String("profile", "", "profile to store the bind under (default: active profile)")
	flagArgs, posArgs := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(posArgs) != 1 {
		return die("agentry service bind [--sandbox <id>] <service> [--from-env]")
	}
	service := posArgs[0]

	// Step 1 — fetch the catalog so we know which fields to prompt for.
	entry, err := fetchCatalogService(service)
	if err != nil {
		return die("fetch catalog: %v", err)
	}
	if entry == nil {
		return die("service %q not in cluster catalog (try `agentry service ls`)", service)
	}
	fields, _ := entry["fields"].([]any)
	if len(fields) == 0 {
		return die("catalog entry for %q has no fields declared", service)
	}

	// Step 2 — collect FIELD values (not env vars). One field can fan
	// out to many env vars via the inject template (e.g. mongodb's
	// `url` populates MONGODB_URL + MONGODB_URI + DATABASE_URL). Asking
	// per env var would prompt the user three times for the same input.
	fieldValues := make(map[string]string, len(fields))
	for _, fRaw := range fields {
		field, _ := fRaw.(map[string]any)
		if field == nil {
			continue
		}
		name, _ := field["name"].(string)
		if name == "" {
			continue
		}
		label, _ := field["label"].(string)
		if label == "" {
			label = name
		}
		secret, _ := field["secret"].(bool)
		required, _ := field["required"].(bool)
		defVal, _ := field["default"].(string)

		if *fromEnv {
			envName := strings.ToUpper(name)
			v := os.Getenv(envName)
			if v == "" {
				if required {
					return die("env var %s not set in current shell (for field %q)", envName, name)
				}
				continue
			}
			fieldValues[name] = v
			continue
		}

		prompt := label
		if defVal != "" {
			prompt = fmt.Sprintf("%s [%s]", label, defVal)
		}
		v, err := promptValue(prompt, secret)
		if err != nil {
			return die("read %s: %v", name, err)
		}
		if v == "" {
			v = defVal
		}
		if v == "" {
			if required {
				return die("%s is required", name)
			}
			fmt.Fprintf(os.Stderr, "(skipped %s — empty)\n", name)
			continue
		}
		fieldValues[name] = v
	}
	if len(fieldValues) == 0 {
		return die("no values supplied; aborting bind")
	}

	// Step 3 — template fields into env vars using inject.env from the
	// manifest. The server accepts pre-templated env values verbatim.
	values, err := injectEnvFromFields(entry, fieldValues)
	if err != nil {
		return die("template env: %v", err)
	}
	if len(values) == 0 {
		return die("catalog entry for %q has no inject.env declared", service)
	}

	version, _ := entry["version"].(string)

	// One-shot mode: POST now, don't persist.
	if *sandbox != "" {
		body := map[string]any{"service": service, "env": values}
		if version != "" {
			body["version"] = version
		}
		fmt.Printf("agentry service bind: sandbox=%s service=%s (%d vars, one-shot)\n",
			*sandbox, service, len(values))
		return callProvisioner("POST", "/api/sandboxes/"+*sandbox+"/bindings", body)
	}

	// Cluster-default mode: persist locally; auto-applies on every
	// future sandbox_create driven by `agentry stdio`.
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no server set; run `agentry server use <name>` first")
	}
	prof := resolveProfile(cfg, *profile)
	if err := saveBind(cfg.Cluster, prof, &StoredBind{
		Service: service,
		Version: version,
		Env:     values,
	}); err != nil {
		return die("save bind: %v", err)
	}
	fmt.Printf("agentry service bind: cluster=%s profile=%s service=%s (%d vars, stored)\n",
		cfg.Cluster, prof, service, len(values))
	fmt.Fprintln(os.Stderr, "  applied automatically to every new sandbox you create with this profile active.")
	return 0
}

// serviceBindsListCLI prints what cluster-default bindings are stored
// on this laptop for the active cluster.
//
// Output is a kubectl-style table: SERVICE | UNIQUE VALUES | ENV VARS.
// "Unique values" deduplicates identical injected values back to the
// number of distinct inputs the user supplied — so mongodb (one URL
// fanned out to 3 env vars) shows "1 input" instead of leaving the
// reader to puzzle out why there are three names.
func serviceBindsListCLI(args []string) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		fmt.Fprint(os.Stdout, `agentry service binds - list cluster-default bindings on this laptop

Usage:
  agentry service binds

Shows what cluster-default service bindings are staged on THIS laptop
for the server you've selected with `+"`agentry server use`"+`. These
get applied automatically to every new sandbox in the cluster.

Values themselves stay in ~/.agentry/services/<cluster>/<svc>.json
(0600). This command only prints metadata.
`)
		return 0
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no server set; run `agentry server use <name>` first")
	}
	prof := resolveProfile(cfg, "")
	binds, err := listBinds(cfg.Cluster, prof)
	if err != nil {
		return die("list binds: %v", err)
	}
	if len(binds) == 0 {
		fmt.Printf("No bindings stored for cluster %q profile %q.\n", cfg.Cluster, prof)
		fmt.Fprintf(os.Stderr, "Run `agentry service bind <service>` to add one.\n")
		return 0
	}
	fmt.Printf("Cluster %q profile %q — bindings stored at %s\n\n", cfg.Cluster, prof, bindsDir(cfg.Cluster, prof))

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tINPUTS\tENV VARS")
	for _, b := range binds {
		names := make([]string, 0, len(b.Env))
		for k := range b.Env {
			names = append(names, k)
		}
		sort.Strings(names)
		// Count distinct values — a proxy for "how many fields did the
		// user supply" when the catalog isn't reachable. mongodb (3
		// env vars sharing one value) collapses to "1".
		seen := make(map[string]struct{}, len(b.Env))
		for _, v := range b.Env {
			seen[v] = struct{}{}
		}
		fmt.Fprintf(tw, "%s\t%d input(s)\t%s\n", b.Service, len(seen), strings.Join(names, ", "))
	}
	tw.Flush()
	return 0
}

// serviceUnbindCLI deletes one stored cluster-default service.
// Existing sandboxes are unaffected (their bindings are container-
// local already); only future creates skip this service.
func serviceUnbindCLI(args []string) int {
	if len(args) != 1 {
		return die("agentry service unbind <service>")
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no server set; run `agentry server use <name>` first")
	}
	prof := resolveProfile(cfg, "")
	if err := deleteBind(cfg.Cluster, prof, args[0]); err != nil {
		return die("delete: %v", err)
	}
	fmt.Printf("agentry service unbind: cluster=%s profile=%s service=%s removed\n", cfg.Cluster, prof, args[0])
	return 0
}

// serviceListCLI is the CLI mirror of the service_list MCP tool.
//
// Output is a kubectl-style table: NAME | CATEGORY | DESCRIPTION
// (first line only, truncated). The full description, the prompt
// shape, and the get-started block are intentionally omitted — they'd
// turn every `service ls` into a wall of text. `agentry service show
// <name>` is the verbose path (lands when there's a real need).
func serviceListCLI(args []string) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		fmt.Fprint(os.Stdout, `agentry service ls - list services in the cluster catalog

Usage:
  agentry service ls

Output columns:
  NAME         the id you pass to `+"`agentry service bind`"+`
  CATEGORY     database, cache, ai, storage, payments, email, analytics, other
  DESCRIPTION  one-line summary

Talk to the cluster's catalog over the tunnel — requires that you've
run `+"`agentry server use <name>`"+` first.
`)
		return 0
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	sess, err := openTunnel(cfg)
	if err != nil {
		return die("dial broker: %v", err)
	}
	defer sess.Close()
	rt := &clusterStampedRT{next: tunnel.NewRoundTripper(sess), cluster: cfg.Cluster}
	client := &http.Client{Transport: rt}

	resp, err := client.Get("http://bridge.invalid/api/catalog?kind=service")
	if err != nil {
		return die("fetch catalog: %v", err)
	}
	defer resp.Body.Close()
	var wrap struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return die("decode: %v", err)
	}
	if len(wrap.Entries) == 0 {
		fmt.Println("(catalog has no services)")
		return 0
	}

	sort.Slice(wrap.Entries, func(i, j int) bool {
		ni, _ := wrap.Entries[i]["name"].(string)
		nj, _ := wrap.Entries[j]["name"].(string)
		return ni < nj
	})

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCATEGORY\tDESCRIPTION")
	for _, e := range wrap.Entries {
		name, _ := e["name"].(string)
		desc, _ := e["description"].(string)
		category := ""
		if extra, ok := e["extra"].(map[string]any); ok {
			category, _ = extra["category"].(string)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, category, firstLine(desc, 80))
	}
	tw.Flush()
	return 0
}

// fetchCatalogService pulls one entry by name. Returns nil if absent.
func fetchCatalogService(name string) (map[string]any, error) {
	cfg, _, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	sess, err := openTunnel(cfg)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	rt := &clusterStampedRT{next: tunnel.NewRoundTripper(sess), cluster: cfg.Cluster}
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequestWithContext(context.Background(), "GET",
		"http://bridge.invalid/api/catalog?kind=service", nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("catalog status=%d body=%s", resp.StatusCode, raw)
	}
	var wrap struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, err
	}
	for _, e := range wrap.Entries {
		if e["name"] == name {
			// Flatten extra into the top-level map so callers can
			// read env_vars without a nested lookup.
			if extra, ok := e["extra"].(map[string]any); ok {
				for k, v := range extra {
					e[k] = v
				}
			}
			return e, nil
		}
	}
	return nil, nil
}

// injectEnvFromFields runs the manifest's inject.env templating
// locally. Each value (and key) in inject.env can contain {field-name}
// placeholders that resolve against the user-supplied field map. We
// expand them here so the bind request body looks the same as it
// always has — server unchanged.
//
// Empty-value envs are dropped: a field that the user skipped should
// not stamp a blank into the sandbox env (which can mask a real value
// from another source).
func injectEnvFromFields(entry map[string]any, fieldValues map[string]string) (map[string]string, error) {
	inject, _ := entry["inject"].(map[string]any)
	if inject == nil {
		return nil, fmt.Errorf("manifest has no inject block")
	}
	envMap, _ := inject["env"].(map[string]any)
	if len(envMap) == 0 {
		return nil, fmt.Errorf("manifest has no inject.env entries")
	}
	out := make(map[string]string, len(envMap))
	for keyTemplate, vRaw := range envMap {
		valueTemplate, _ := vRaw.(string)
		key := expandFields(keyTemplate, fieldValues)
		value := expandFields(valueTemplate, fieldValues)
		if key == "" || value == "" {
			continue // user skipped the field this env depends on
		}
		// Percent-encode userinfo in DB connection URLs when raw
		// special characters (e.g. `%` in a password like
		// `4g.uW%azWbL*mZ9`) would otherwise break url.Parse downstream
		// — at fitness-check time, at sidecar boot, and at every
		// pgx/mongo connect. The user typed a valid password; we
		// just have to encode it before storing.
		if looksLikeDBURL(value) {
			if fixed := repairConnectionURL(value); fixed != value {
				fmt.Fprintf(os.Stderr,
					"  note: %s contained unescaped special chars; URL-encoded the userinfo so parsers accept it\n",
					key)
				value = fixed
			}
		}
		out[key] = value
	}
	return out, nil
}

// expandFields replaces `{name}` occurrences in s with fieldValues[name].
// A placeholder whose field is missing returns the empty string for that
// slot — caller decides whether that means "skip this env" or error.
func expandFields(s string, fieldValues map[string]string) string {
	if !strings.Contains(s, "{") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		open := strings.IndexByte(s[i:], '{')
		if open < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+open])
		close := strings.IndexByte(s[i+open:], '}')
		if close < 0 {
			b.WriteString(s[i+open:])
			break
		}
		name := s[i+open+1 : i+open+close]
		if v, ok := fieldValues[name]; ok {
			b.WriteString(v)
		} else {
			return "" // missing field → empty env (caller drops it)
		}
		i += open + close + 1
	}
	return b.String()
}

// promptValue prints label + ": " to stderr, reads one line of input.
// hidden=true uses term.ReadPassword (no echo) when stdin is a TTY;
// when stdin is piped (CI / scripting), falls back to a plain read
// with a one-time warning that input will be visible. Returns the
// trimmed string.
func promptValue(label string, hidden bool) (string, error) {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	if hidden && term.IsTerminal(int(os.Stdin.Fd())) {
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	if hidden {
		// Non-TTY: ReadPassword would error with "inappropriate
		// ioctl". Warn once so the operator knows the value will
		// echo (or land in shell history if they pasted), then
		// read a normal line.
		warnedNonTTYOnce()
	}
	return prompt(""), nil
}

var warnedNonTTY bool

func warnedNonTTYOnce() {
	if warnedNonTTY {
		return
	}
	warnedNonTTY = true
	fmt.Fprintln(os.Stderr, "\nagentry: stdin is not a TTY — hidden prompts disabled (input WILL be visible).")
	fmt.Fprintln(os.Stderr, "     for sensitive values, run interactively or use --from-env / --from-file.")
}

func containsAny(s string, needles ...string) bool {
	upper := strings.ToUpper(s)
	for _, n := range needles {
		if strings.Contains(upper, n) {
			return true
		}
	}
	return false
}

// firstLine returns the first non-empty line of s, truncated to max
// runes with an ellipsis if it overflows. Used by `service ls` to keep
// descriptions to one tabular column without wrapping.
func firstLine(s string, max int) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > max {
			return string(runes[:max-1]) + "…"
		}
		return line
	}
	return ""
}

