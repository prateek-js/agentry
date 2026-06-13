package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/agentry/agentry/pkg/tunnel"
	"github.com/hashicorp/yamux"
	"golang.org/x/term"
)

// cmdEnv dispatches the `agentry env *` subcommands. Two scopes:
//
//   - cluster default (no --sandbox): saved on the laptop under
//     ~/.ad-sandbox/envs/<cluster>/<NAME>.json; the stdio post-create
//     hook replays it onto every new sandbox in that cluster. Run-once,
//     applies to all future sandboxes — what you want for things like
//     JIRA_TOKEN that belong on every sandbox you spin up.
//
//   - sandbox-scoped (--sandbox <id>): goes straight to the
//     provisioner's /api/sandboxes/{id}/secrets. Only that sandbox
//     sees it. What you want for per-app overrides or one-off tests.
//
// Why this lives on the CLI and not the MCP layer: the value entered
// here goes through a hidden prompt (term.ReadPassword) — secrets
// shouldn't enter chat context, and the MCP env_set tool actively
// rejects secret-shaped values to enforce this.
func cmdEnv(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "agentry env: need a subcommand (set|ls|unset)")
		return 2
	}
	switch args[0] {
	case "set":
		return envSet(args[1:])
	case "ls", "list":
		return envList(args[1:])
	case "unset", "rm", "delete":
		return envUnset(args[1:])
	default:
		return die("agentry env: unknown subcommand %q", args[0])
	}
}

func envSet(args []string) int {
	fs := flag.NewFlagSet("agentry env set", flag.ContinueOnError)
	// Explicit user choice ("") vs unset (""). flag.String returns the
	// default for both; we distinguish via fs.Lookup("sandbox").
	sandbox := fs.String("sandbox", "", "target sandbox id (omit = save as cluster default for every sandbox in this cluster)")
	profile := fs.String("profile", "", "profile to write to (default: active profile)")
	// Use splitFlagsAndPositionals so flags can appear AFTER positional
	// args too — `agentry env set NAME VALUE --profile dev` should
	// work the way users instinctively type it.
	flagArgs, rest := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(rest) < 1 {
		return die("agentry env set [--sandbox <id>] NAME [VALUE]\n" +
			"  (omit VALUE to be prompted with hidden input)\n" +
			"  (omit --sandbox to save as a cluster default — applied to every sandbox you create on the current server)")
	}
	name := rest[0]
	var value string
	if len(rest) >= 2 {
		// Value on the command line. Visible in shell history — only
		// use for non-sensitive values. For secrets, omit and use the
		// hidden prompt.
		value = rest[1]
	} else {
		v, err := readHidden(fmt.Sprintf("Value for %s: ", name))
		if err != nil {
			return die("read value: %v", err)
		}
		value = v
	}

	if *sandbox == "" {
		// Cluster-default path. Saves on the laptop; the next
		// sandbox_create from this CLI replays it onto the new
		// sandbox via the post-create hook in main.go.
		cfg, _, err := LoadConfig()
		if err != nil {
			return die("load config: %v", err)
		}
		if cfg.Cluster == "" {
			return die("no server set — run `agentry server use <name>` first " +
				"(or pass --sandbox <id> to set this only on one sandbox)")
		}
		prof := resolveProfile(cfg, *profile)
		if err := saveEnv(cfg.Cluster, prof, &StoredEnv{Name: name, Value: value}); err != nil {
			return die("save: %v", err)
		}
		fmt.Printf("staged %s on server %q (profile %q) — applied to every new sandbox you create with this profile active\n",
			name, cfg.Cluster, prof)
		return 0
	}

	sb := resolveSandbox(*sandbox)
	if sb == "" {
		return die("--sandbox was empty; pass an id or omit the flag to save as a cluster default")
	}
	return callProvisioner("POST", "/api/sandboxes/"+sb+"/secrets",
		map[string]string{"name": name, "value": value, "source": "cli"})
}

// envUnset removes a staged value. With --sandbox, removes from that
// sandbox's runtime secret store. Without, removes the cluster default
// from the laptop. Independent operations — sandbox-scoped values
// don't shadow cluster defaults, so you may need to call both.
func envUnset(args []string) int {
	fs := flag.NewFlagSet("agentry env unset", flag.ContinueOnError)
	sandbox := fs.String("sandbox", "", "target sandbox id (omit = remove cluster default)")
	profile := fs.String("profile", "", "profile to remove from (default: active profile)")
	flagArgs, rest := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(rest) < 1 {
		return die("agentry env unset [--sandbox <id>] NAME")
	}
	name := rest[0]
	if *sandbox == "" {
		cfg, _, err := LoadConfig()
		if err != nil {
			return die("load config: %v", err)
		}
		if cfg.Cluster == "" {
			return die("no server set — run `agentry server use <name>` first")
		}
		prof := resolveProfile(cfg, *profile)
		if err := deleteEnv(cfg.Cluster, prof, name); err != nil {
			return die("delete: %v", err)
		}
		fmt.Printf("removed %s on server %q (profile %q)\n", name, cfg.Cluster, prof)
		return 0
	}
	sb := resolveSandbox(*sandbox)
	if sb == "" {
		return die("--sandbox was empty; pass an id or omit the flag")
	}
	return callProvisioner("DELETE", "/api/sandboxes/"+sb+"/secrets/"+name, nil)
}

func envList(args []string) int {
	fs := flag.NewFlagSet("agentry env ls", flag.ContinueOnError)
	sandbox := fs.String("sandbox", "", "target sandbox id (omit = list cluster defaults staged on the laptop)")
	profile := fs.String("profile", "", "profile to list (default: active profile)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sandbox == "" {
		// Cluster-default view — what's staged on the laptop for the
		// active server. Values are NEVER echoed; only names.
		cfg, _, err := LoadConfig()
		if err != nil {
			return die("load config: %v", err)
		}
		if cfg.Cluster == "" {
			return die("no server set — run `agentry server use <name>` first")
		}
		prof := resolveProfile(cfg, *profile)
		envs, err := listEnvs(cfg.Cluster, prof)
		if err != nil {
			return die("list envs: %v", err)
		}
		if len(envs) == 0 {
			fmt.Printf("(no env vars staged on this laptop for server %q profile %q)\n", cfg.Cluster, prof)
			return 0
		}
		for _, e := range envs {
			fmt.Println(e.Name)
		}
		return 0
	}
	sb := resolveSandbox(*sandbox)
	if sb == "" {
		return die("--sandbox was empty; pass an id or omit the flag")
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

	resp, err := client.Get("http://bridge.invalid/api/sandboxes/" + sb + "/secrets")
	if err != nil {
		return die("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return die("status=%d %s", resp.StatusCode, raw)
	}
	var out struct {
		Names []string `json:"names"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return die("decode: %v", err)
	}
	if len(out.Names) == 0 {
		fmt.Println("(no env vars staged in this sandbox)")
		return 0
	}
	for _, n := range out.Names {
		fmt.Println(n)
	}
	return 0
}

// readHidden prompts the user with label and reads a line of input
// without echoing keystrokes. Used for secrets that should never
// appear in shell history or anyone's screen.
func readHidden(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr) // newline after the hidden read
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// callProvisioner is the shared "build a tunneled HTTP request, send,
// pretty-print the response" helper for agentry subcommands that
// proxy to the provisioner. body may be nil for verbs without one.
func callProvisioner(method, path string, body any) int {
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

	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return die("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, "http://bridge.invalid"+path, rdr)
	if err != nil {
		return die("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return die("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return die("status=%d %s", resp.StatusCode, raw)
	}
	if len(raw) > 0 {
		// Pretty-print JSON; pass through anything else.
		var pretty bytes.Buffer
		if json.Indent(&pretty, raw, "", "  ") == nil {
			fmt.Println(pretty.String())
		} else {
			fmt.Println(string(raw))
		}
	} else {
		fmt.Println("ok")
	}
	return 0
}

// openTunnel encapsulates the dial-broker boilerplate that several
// agentry subcommands need. Returns the yamux session for the caller to
// build a RoundTripper on.
//
// When the config carries a device cert + key (prod mode), we present
// them via TLS for the duration of the session. The broker pins them
// to its KMS-signed CA pool on the listener; if the cert is missing,
// expired, or unknown to the broker, the dial fails at TLS time.
func openTunnel(cfg *Config) (*yamux.Session, error) {
	if cfg.BrokerURL == "" {
		return nil, fmt.Errorf("config has no broker_url; run `agentry init`")
	}
	if cfg.Cluster == "" {
		return nil, fmt.Errorf("no server set; run `agentry server use <name>`")
	}
	dial := tunnel.DialConfig{
		BrokerURL: cfg.BrokerURL,
		Role:      tunnel.RoleDevice,
		Headers:   http.Header{tunnel.HeaderDeviceID: []string{cfg.DeviceID}},
	}
	if cfg.DeviceCertPath != "" && cfg.DeviceKeyPath != "" {
		tlsConf, err := buildClientTLS(cfg)
		if err != nil {
			return nil, fmt.Errorf("build client TLS: %w", err)
		}
		dial.TLSConfig = tlsConf
	}
	return tunnel.Dial(context.Background(), dial)
}

// buildClientTLS loads the persisted device cert + key + CA cert into
// a tls.Config the broker will accept. ServerName is omitted; tls.Dial
// derives it from the hostname so SNI works correctly behind the
// broker's autocert.
func buildClientTLS(cfg *Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.DeviceCertPath, cfg.DeviceKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load device cert/key: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	// Start from the system roots so the broker's LetsEncrypt server
	// cert verifies. Then add our KMS-signed CA — harmless extra trust
	// anchor that future-proofs us against a broker switching to a
	// self-signed server cert.
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s: no PEM certificates found", cfg.CACertPath)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
