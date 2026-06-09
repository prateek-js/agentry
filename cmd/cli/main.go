// xdp is the user-side daemon and CLI for ad-sandbox.
//
// In the steady state, the user has run `xdp init` once to write a
// config file and `xdp cluster use <name>` to pick which cluster to
// drive. Then Claude Desktop / Code is configured to spawn `xdp stdio`,
// which dials the configured broker over yamux and runs the MCP server
// with every outbound HTTP call routed through the tunnel.
//
// Subcommands:
//
//	xdp init [--broker URL]      write skeleton ~/.ad-sandbox/xdp.json
//	xdp cluster current          print the cluster xdp will target
//	xdp cluster use <name>       set the cluster
//	xdp stdio                    run the MCP server bound to stdio
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/agentry/agentry/pkg/bridge"
	"github.com/agentry/agentry/pkg/mcp"
	"github.com/agentry/agentry/pkg/tunnel"
)

// tabWriter is the shared tab-aligned writer used by every `ls` /
// `status` command. Centralised so column padding is consistent across
// the CLI — the polish bar says "if it looks like it was thrown
// together by three different people, it was". Tabs separate columns;
// padding 2 spaces; min width 2. minwidth=0 lets short columns hug.
func tabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

const usage = `agentry — laptop-side daemon + CLI for agentry.run

GETTING STARTED
  agentry login                            authorize this machine (browser flow)
  agentry init --app-url URL --token TOK   enroll the device cert (after login;
                                           token comes from "Add this machine")
  agentry server                           pick the server to drive
  agentry status                           show config + auth + selected server

DAILY USE
  agentry sandbox ls                       list sandboxes on the current server
  agentry sandbox use <id>                 pin <id> so other commands omit it
  agentry sandbox current                  print the pinned sandbox
  agentry sandbox rm <id>                  delete a sandbox

  agentry pull [<sandbox>]                 download the sandbox to ./<sandbox>/
  agentry env set NAME [VALUE] [--sandbox <id>]   (omit VALUE → hidden prompt)
  agentry env ls [--sandbox <id>]

MULTI-ENV (profiles — one cluster, many configurations)
  agentry profile                          print the active profile
  agentry profile list                     table of every profile on this laptop
  agentry profile use <name>               switch the active profile
  agentry profile create <name>            create an empty profile
  agentry profile show [--profile <name>]  list envs + binds under a profile
  agentry profile copy <src> <dst>         clone one profile into another

AUTH (login + sessions for apps built on agentry)
  agentry auth                             show the active profile's auth posture
  agentry auth enable                      fitness-check the DB + mint AUTH_SECRET
  agentry auth disable                     remove auth state for this profile
  agentry auth providers add <name>        register an OAuth provider (google, github, …)
  agentry auth providers list              table of every provider on this profile

EDITOR INTEGRATION
  agentry mcp                              MCP server on stdin/stdout
                                           (alias: agentry stdio)

SERVER (the box running your sandboxes)
  agentry server ls                        non-interactive list
  agentry server use <name>                set the server
  agentry server current                   print the server

SERVICE BINDINGS (postgres, openai, …)
  agentry service ls                       list available services in the catalog
  agentry service bind <service>           bind as server default (interactive)
  agentry service bind <service> --from-env       read values from shell env
  agentry service bind --sandbox <id> <service>   one-shot, this sandbox only
  agentry service binds                    list server defaults stored locally
  agentry service unbind <service>

SHARES + DEPLOYS
  Share a live sandbox port (*.agentry.live) or deploy a built image:
  both live in the dashboard at https://app.agentry.run.

OTHER
  agentry logout                           drop + revoke the local token
  agentry version                          print the build version
  agentry help [<command>]                 detailed help for a command

Configuration:   ~/.agentry/agentry.json
Pinned sandbox:  ~/.agentry/state.json
`

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	// stdio is the MCP transport on stdout; everything else logs to
	// stderr by default. Reset just to be explicit.
	log.SetOutput(os.Stderr)

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	os.Exit(dispatch(os.Args[1:]))
}

// dispatch is the body of main() lifted to its own function so tests
// can exercise it without forking a subprocess. Returns the exit code
// the binary should use.
func dispatch(args []string) int {
	switch args[0] {
	case "login":
		return cmdLogin(args[1:])
	case "logout":
		return cmdLogout(args[1:])
	case "init":
		// Enrolls the laptop's device cert against agentry-app using
		// the token shown on the dashboard's "Add this machine" panel.
		// Run after `agentry login`, before `agentry mcp`.
		return cmdInit(args[1:])
	case "server", "cluster":
		// "cluster" is the legacy name; "server" is what users see now.
		// Both wire to the same handler so existing scripts keep working.
		return cmdCluster(args[1:])
	case "sandbox":
		return cmdSandbox(args[1:])
	case "mcp", "stdio":
		// stdio is the legacy name kept as an alias so existing
		// Claude Desktop / Roo configs keep working.
		return cmdMCP(args[1:])
	case "env":
		return cmdEnv(args[1:])
	case "profile":
		return cmdProfile(args[1:])
	case "auth":
		return cmdAuth(args[1:])
	case "pull":
		return cmdPull(args[1:])
	case "share":
		fmt.Fprintln(os.Stderr,
			"agentry share moved to the dashboard:\n"+
				"  https://app.agentry.run")
		return 2
	case "deploy", "deployment":
		fmt.Fprintln(os.Stderr,
			"agentry deploy moved to the dashboard:\n"+
				"  https://app.agentry.run")
		return 2
	case "service":
		return cmdService(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "version", "-v", "--version":
		return cmdVersion(args[1:])
	case "help", "-h", "--help":
		return cmdHelp(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "agentry: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

// cmdHelp dispatches `agentry help [<command>]`. No arg → top-level
// usage. Arg → re-route the command with `--help` so each subcommand's
// own help block prints (where it has one). For commands without
// dedicated help text, falls back to the top-level usage.
func cmdHelp(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stdout, usage)
		return 0
	}
	// Hand off to the subcommand with --help. Subcommands that don't
	// recognise --help land in their default branch (which we treat
	// as "show top-level usage" below). The ones with proper help
	// blocks (service, login, init) print and exit cleanly.
	rc := dispatch([]string{args[0], "--help"})
	if rc != 0 {
		fmt.Fprint(os.Stdout, usage)
		return 0
	}
	return rc
}

// cmdMCP is the workhorse: read config, dial broker, run the MCP
// server bound to stdin/stdout with a tunneled HTTP client. The CLI
// alias `agentry stdio` routes here so legacy editor configs (which
// spawn "agentry stdio") keep working.
func cmdMCP(_ []string) int {
	cfg, path, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `agentry init` first; tried %s)", err, path)
	}
	if cfg.BrokerURL == "" {
		return die("config %s has no broker_url; run `agentry init` first", path)
	}
	if cfg.Cluster == "" {
		return die("no current cluster; run `agentry cluster use <name>` first")
	}

	ctx, cancel := signalContext()

	dial := tunnel.DialConfig{
		BrokerURL: cfg.BrokerURL,
		Role:      tunnel.RoleDevice,
		Headers:   http.Header{tunnel.HeaderDeviceID: []string{cfg.DeviceID}},
	}
	if cfg.DeviceCertPath != "" && cfg.DeviceKeyPath != "" {
		tlsConf, err := buildClientTLS(cfg)
		if err != nil {
			return die("build client TLS: %v", err)
		}
		dial.TLSConfig = tlsConf
	}

	// Start with a session-less RoundTripper. The broker dial happens
	// in the background goroutine below — synchronous dial here would
	// block the MCP server from responding to the host's `initialize`
	// request, and Roo/Cursor/Claude Desktop time it out at -32001
	// after ~5 s on slow/captive networks. Tool calls that arrive
	// before the session is ready get a clean "no live session" error
	// (the RoundTripper's nil-session path); subsequent calls work
	// once the dial lands.
	//
	// clusterRef is a TTL-cached reader for the active cluster so
	// `agentry cluster use <name>` takes effect on the very next tool
	// call from Roo, without forcing the user to kill + restart this
	// stdio process. Reused by the post-create hook below so a new
	// sandbox's cluster-default service binds also come from the
	// current cluster, not the boot-time snapshot.
	clusterRef := newConfigCluster(cfg.Cluster)
	inner := tunnel.NewRoundTripper(nil)
	rt := &clusterStampedRT{
		next:       inner,
		getCluster: clusterRef.Get,
	}

	// Dial the broker in the background + keep it connected through
	// session drops. First successful dial populates the RoundTripper;
	// subsequent re-dials replace it. yamux sessions die from idle
	// timeouts, network blips, autocert renewals on the bridge,
	// anything that closes the underlying TCP — the loop keeps the
	// MCP server alive across all of those.
	go connectAndKeepAlive(ctx, inner, dial, cfg.BrokerURL, cfg.DeviceID, cfg.Cluster)
	defer func() {
		// Order matters: cancel ctx first so the connect goroutine
		// returns through its `<-ctx.Done()` branch and doesn't log
		// "session closed; reconnecting" on graceful shutdown. THEN
		// close whatever session is currently held.
		cancel()
		if s := inner.Session(); s != nil {
			_ = s.Close()
		}
	}()
	tunneledHTTP := &http.Client{Transport: rt}
	mcpClient := mcp.NewClient(mcp.Config{
		// ProvisionerURL has to be non-empty for the MCP client to
		// build absolute URLs for sandbox_create etc. The host is
		// ignored by the tunnel transport; only the path matters,
		// and the broker routes based on the X-Cluster header.
		ProvisionerURL: "http://bridge.invalid",
		HTTPClient:     tunneledHTTP,
		// After every successful sandbox_create, replay each
		// cluster-default service bind + env var the user staged via
		// `agentry service bind <service>` / `agentry env set NAME`
		// (no --sandbox). Real creds + secrets live in
		// ~/.ad-sandbox/{services,envs}/<cluster>/; they ride the
		// same tunnel that just created the sandbox. clusterRef.Get is
		// the same TTL-cached reader the round-tripper uses, so a
		// fresh sandbox always gets the binds + envs matching the
		// cluster the request was routed to.
		PostCreateHook: chainHooks(
			applyClusterDefaults(clusterAndProfile(clusterRef.Get), tunneledHTTP),
			applyClusterEnvDefaults(clusterAndProfile(clusterRef.Get), tunneledHTTP),
		),
	})

	// Watchdogs around the stdio loop. Two failure modes we've seen
	// in practice — both can leave the process running after Roo /
	// Cursor / Claude Desktop is gone, where it shows up as a
	// duplicate "agentry mcp" the next time the host spawns a fresh
	// one and the new initialize times out at -32001:
	//
	//   1. Parent death without SIGHUP. macOS doesn't deliver SIGHUP
	//      to children when a VS Code window crashes; the process gets
	//      reparented to launchd (PID 1). Poll PPID and exit when we
	//      notice we've been re-parented.
	//   2. stdin EOF not propagating through the SDK's reader fast
	//      enough. RunStdio occasionally lingers in an interruptible
	//      sleep waiting on a channel. Once ctx is cancelled, give it
	//      2 s to clean up, then force-exit.
	go func() {
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		startPPID := os.Getppid()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if ppid := os.Getppid(); ppid == 1 || (startPPID != 1 && ppid != startPPID) {
					// Parent went away (re-parented to init, or PID
					// changed). Tear ourselves down.
					cancel()
					time.AfterFunc(2*time.Second, func() { os.Exit(0) })
					return
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		// Hard fence: 2 s after any cancellation path, exit regardless
		// of where RunStdio + the dial goroutine are.
		time.AfterFunc(2*time.Second, func() { os.Exit(0) })
	}()

	mcpErr := mcp.RunStdio(ctx, mcpClient)
	cancel()
	if s := inner.Session(); s != nil {
		_ = s.Close()
	}
	if mcpErr != nil && !errors.Is(mcpErr, context.Canceled) {
		return die("mcp server: %v", mcpErr)
	}
	return 0
}

// cmdStatus prints what the daemon would do if started. Does NOT dial
// the broker — it's a config-readback only, useful for "is my install
// healthy" before `agentry mcp` runs. The user + org line is filled by
// `agentry login`'s callback; no extra round-trip on each `status`.
func cmdStatus(_ []string) int {
	cfg, path, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `agentry login` first; tried %s)", err, path)
	}
	state := LoadState()
	tw := tabWriter()
	defer tw.Flush()

	who := "(not logged in; run `agentry login`)"
	if cfg.UserEmail != "" {
		who = cfg.UserEmail
		if cfg.Org != "" {
			who = cfg.UserEmail + "  in " + cfg.Org
		}
	}
	fmt.Fprintf(tw, "logged in as:\t%s\n", who)
	fmt.Fprintf(tw, "config file:\t%s\n", path)
	fmt.Fprintf(tw, "app URL:\t%s\n", emptyAs(cfg.AppURL, "(not set; run `agentry login`)"))
	fmt.Fprintf(tw, "broker URL:\t%s\n", emptyAs(cfg.BrokerURL, "(not set; run `agentry init` after login)"))
	fmt.Fprintf(tw, "device ID:\t%s\n", emptyAs(cfg.DeviceID, "(none)"))
	fmt.Fprintf(tw, "server:\t%s\n", emptyAs(cfg.Cluster, "(not set; run `agentry server`)"))
	fmt.Fprintf(tw, "sandbox:\t%s\n", emptyAs(state.CurrentSandbox, "(not pinned; run `agentry sandbox use <id>`)"))
	return 0
}

func emptyAs(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// signalContext returns a context that cancels on SIGTERM/SIGINT/SIGHUP.
// The MCP RunStdio loop respects ctx so the host's subprocess
// management (Claude Desktop, Roo Code, Cursor, …) can unblock cleanly
// when it tears the connection down.
//
// SIGHUP is the load-bearing addition: when a VS Code window closes,
// macOS / Linux send SIGHUP to child processes. Without it, the agentry
// stdio process keeps running after the host disappears and shows up
// in the next `ps` as an orphan — the exact "two agentry mcp instances"
// shape that Roo Code users hit when Code is restarted mid-session.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		<-stop
		cancel()
	}()
	return ctx, cancel
}

func die(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "agentry: "+format+"\n", args...)
	return 1
}

// clusterStampedRT wraps a RoundTripper and adds X-Cluster: <name>
// to every request. This is the bridge between mcp.Client (which
// addresses one logical "provisioner") and the broker (which routes
// per request based on this header).
//
// Either `cluster` (static, used by one-shot CLI subcommands that
// exit before the user could plausibly switch clusters) or `getCluster`
// (dynamic, used by the long-running `agentry mcp` stdio process so
// `agentry cluster use <name>` takes effect on the next tool call
// without restarting the child) must be set. getCluster wins when
// both are present.
type clusterStampedRT struct {
	next       http.RoundTripper
	cluster    string
	getCluster func() string
}

func (r *clusterStampedRT) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone so we don't mutate the caller's request — required by
	// http.RoundTripper contract.
	req = req.Clone(req.Context())
	name := r.cluster
	if r.getCluster != nil {
		name = r.getCluster()
	}
	req.Header.Set(bridge.HeaderTargetCluster, name)
	return r.next.RoundTrip(req)
}

// configCluster reads the active cluster from ~/.agentry/agentry.json
// with a 1-second TTL cache. Lets `agentry mcp` pick up an out-of-band
// `agentry cluster use <name>` without forcing the user to kill + let
// Roo respawn the child. The per-call cost is at most one file read
// per second; tool calls take 100-1000 ms of network anyway, so the
// 10-50 µs cost is invisible.
//
// Safe for concurrent use. Logs to stderr on the first call AFTER a
// switch, so operators can correlate "my Roo started routing to a
// different cluster" with the change.
type configCluster struct {
	mu       sync.Mutex
	value    string
	lastRead time.Time
	ttl      time.Duration
	prev     string // last logged value
}

// newConfigCluster returns a cache pre-warmed with the initial value.
// The cache is poll-on-read; there's no background goroutine to clean
// up at process exit.
func newConfigCluster(initial string) *configCluster {
	return &configCluster{
		value: initial,
		prev:  initial,
		ttl:   time.Second,
	}
}

// Get returns the active cluster name. Cheap when called within the
// TTL window; one config-file read otherwise. Falls back to the last
// known good value if the file became unreadable (e.g. user deleted
// ~/.agentry/ mid-session).
func (c *configCluster) Get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.lastRead) < c.ttl {
		return c.value
	}
	c.lastRead = time.Now()
	cfg, _, err := LoadConfig()
	if err != nil || cfg.Cluster == "" {
		return c.value // last good
	}
	if cfg.Cluster != c.prev {
		log.Printf("agentry: cluster switched %q → %q (next tool call routes there)",
			c.prev, cfg.Cluster)
		c.prev = cfg.Cluster
	}
	c.value = cfg.Cluster
	return c.value
}

// reconnectLoop watches the current tunnel session; whenever it
// closes (idle timeout, network blip, autocert renewal on the bridge,
// anything) we redial with exponential backoff and atomically swap the
// fresh session into the RoundTripper. The MCP server keeps running
// across the gap; the next tool call after a successful reconnect
// sees a healthy session instead of "no live session".
//
// Returns when ctx is cancelled (Ctrl+C or signalContext fire).
//
// Unified path for the initial dial + every subsequent reconnect.
// Logs to stderr (visible in the host's MCP debug pane) so the user
// can tell whether the tunnel is up; the MCP server itself stays
// responsive on stdin/stdout regardless of broker reachability.
func connectAndKeepAlive(ctx context.Context, rt *tunnel.RoundTripper, dial tunnel.DialConfig, brokerURL, deviceID, cluster string) {
	log.Printf("agentry: dialing broker %s as device=%s, cluster=%s",
		brokerURL, deviceID, cluster)
	for {
		if ctx.Err() != nil {
			return
		}
		sess := dialWithBackoff(ctx, dial)
		if sess == nil {
			return // ctx cancelled mid-backoff
		}
		rt.SetSession(sess)
		log.Printf("agentry: tunnel connected")

		select {
		case <-ctx.Done():
			return
		case <-sess.CloseChan():
			log.Printf("agentry: tunnel session closed; reconnecting")
		}
	}
}

// dialWithBackoff loops on tunnel.Dial with exponential backoff until
// either the dial succeeds or ctx is cancelled. Returns nil only on
// cancellation; every other failure mode (DNS, TLS, broker-rejected
// CSR) keeps retrying because the user usually wants the MCP server
// to recover automatically once the network or broker comes back.
func dialWithBackoff(ctx context.Context, dial tunnel.DialConfig) *yamux.Session {
	backoff := tunnel.NewBackoff(tunnel.DialerBackoff())
	for {
		if ctx.Err() != nil {
			return nil
		}
		sess, err := tunnel.Dial(ctx, dial)
		if err == nil {
			return sess
		}
		delay := backoff.Next()
		log.Printf("agentry: tunnel dial attempt %d failed: %v (retry in %s)",
			backoff.Attempts(), err, delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// tunnelSession is the subset of *yamux.Session reconnectLoop needs.
// Kept as a tiny interface so the loop is testable without standing up
// a real yamux session.
type tunnelSession interface {
	CloseChan() <-chan struct{}
}
