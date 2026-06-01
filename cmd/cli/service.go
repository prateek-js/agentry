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

	"github.com/agentry/agentry/pkg/tunnel"
	"golang.org/x/term"
)

// cmdService dispatches `agentry service *` subcommands.
//
//	agentry service list                            (catalog)
//	agentry service bind <service>                  (cluster-default: stored locally)
//	agentry service bind --sandbox <id> <service>   (one-shot override)
//	agentry service binds                           (list stored cluster defaults)
//	agentry service unbind <service>                (drop a stored default)
func cmdService(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "agentry service: need a subcommand (ls|bind|binds|unbind)")
		return 2
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
		return die("agentry service: unknown subcommand %q", args[0])
	}
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
	flagArgs, posArgs := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(posArgs) != 1 {
		return die("agentry service bind [--sandbox <id>] <service> [--from-env]")
	}
	service := posArgs[0]

	// Step 1 — fetch the catalog so we know which env vars to prompt for.
	entry, err := fetchCatalogService(service)
	if err != nil {
		return die("fetch catalog: %v", err)
	}
	if entry == nil {
		return die("service %q not in cluster catalog (try `agentry service list`)", service)
	}
	envVars, _ := entry["env_vars"].([]any)
	if len(envVars) == 0 {
		return die("catalog entry for %q has no env_vars declared", service)
	}

	// Step 2 — collect values.
	values := make(map[string]string, len(envVars))
	for _, ev := range envVars {
		name, _ := ev.(string)
		if name == "" {
			continue
		}
		if *fromEnv {
			v := os.Getenv(name)
			if v == "" {
				return die("env var %s not set in current shell", name)
			}
			values[name] = v
			continue
		}
		// Hidden prompt for anything that smells like a secret.
		hidden := containsAny(name, "PASSWORD", "TOKEN", "SECRET", "KEY")
		v, err := promptValue(name, hidden)
		if err != nil {
			return die("read %s: %v", name, err)
		}
		if v == "" {
			fmt.Fprintf(os.Stderr, "(skipped %s — empty)\n", name)
			continue
		}
		values[name] = v
	}
	if len(values) == 0 {
		return die("no values supplied; aborting bind")
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
		return die("no cluster set; run `agentry cluster use <name>` first")
	}
	if err := saveBind(cfg.Cluster, &StoredBind{
		Service: service,
		Version: version,
		Env:     values,
	}); err != nil {
		return die("save bind: %v", err)
	}
	fmt.Printf("agentry service bind: cluster=%s service=%s (%d vars, stored)\n",
		cfg.Cluster, service, len(values))
	fmt.Fprintln(os.Stderr, "  applied automatically to every new sandbox in this cluster.")
	return 0
}

// serviceBindsListCLI prints what cluster-default bindings are
// stored on this laptop for the active cluster. Names + var names
// only — values stay in the JSON file.
func serviceBindsListCLI(_ []string) int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no cluster set; run `agentry cluster use <name>` first")
	}
	binds, err := listBinds(cfg.Cluster)
	if err != nil {
		return die("list binds: %v", err)
	}
	if len(binds) == 0 {
		fmt.Printf("(no cluster defaults stored for %s)\n", cfg.Cluster)
		fmt.Fprintln(os.Stderr, "  run `agentry service bind <service>` to stage one.")
		return 0
	}
	fmt.Printf("cluster=%s, stored at %s\n\n", cfg.Cluster, bindsDir(cfg.Cluster))
	for _, b := range binds {
		names := make([]string, 0, len(b.Env))
		for k := range b.Env {
			names = append(names, k)
		}
		sort.Strings(names)
		fmt.Printf("%-12s %s\n", b.Service, strings.Join(names, " "))
	}
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
		return die("no cluster set; run `agentry cluster use <name>` first")
	}
	if err := deleteBind(cfg.Cluster, args[0]); err != nil {
		return die("delete: %v", err)
	}
	fmt.Printf("agentry service unbind: cluster=%s service=%s removed\n", cfg.Cluster, args[0])
	return 0
}

// serviceListCLI is the CLI mirror of the service_list MCP tool —
// prints the catalog's services with their declared env vars.
func serviceListCLI(_ []string) int {
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
	for _, e := range wrap.Entries {
		fmt.Printf("%-12s %s\n", e["name"], e["description"])
		if extra, ok := e["extra"].(map[string]any); ok {
			if env, ok := extra["env_vars"].([]any); ok && len(env) > 0 {
				parts := make([]string, 0, len(env))
				for _, v := range env {
					if s, ok := v.(string); ok {
						parts = append(parts, s)
					}
				}
				fmt.Printf("             env: %s\n", strings.Join(parts, " "))
			}
		}
	}
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

